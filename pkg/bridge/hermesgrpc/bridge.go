// Package hermesgrpc serves the aries.sandbox.v1 contract from the ARIES
// process, granting one harness narrow, audited, revocable access to one task
// sandbox.
//
// The listener binds the task network's gateway, exactly where the SSH bridge
// binds today. The task container serves nothing and gains no daemon: an
// accepted request becomes a core.Command and reaches the container through
// the Docker Engine API, so a tool call still leaves the harness container,
// arrives at the host, and re-enters the sandbox from outside.
//
// This is the first iteration and carries Exec alone. See
// docs/design/grpc-bridge.md for the full design and for why the request
// carries only a script and stdin.
package hermesgrpc

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	defaultBridgeCleanup  = 20 * time.Second
	maxRecordedInputBytes = 16 << 20
	maxToolLogBytes       = 256 << 20

	// remoteShellPath is where a script runs. The sandbox requires an absolute
	// command path and performs no PATH lookup, so a task image must provide
	// /bin/bash — the same requirement the SSH bridge imposes.
	remoteShellPath = "/bin/bash"

	// identityContainerPath holds the client's own certificate and key;
	// trustedContainerPath holds the single server certificate the client
	// accepts. The harness stages both at these paths.
	identityContainerPath = "/run/aries/grpc/client.pem"
	trustedContainerPath  = "/run/aries/grpc/server.crt"

	// clientContainerPath is where the staged client lands. It is named `ssh`
	// and reached by a PATH entry the harness prepends, because Hermes resolves
	// its terminal client by name. See docs/design/grpc-bridge.md section 9 for
	// why that shadowing is temporary.
	clientContainerPath = "/run/aries/bin/ssh"

	lockedUsername = "aries"

	certificateLifetime = 24 * time.Hour

	// maxMessageBytes and defaultOutputLimit are deliberately unlimited for the
	// first iteration. grpc-go caps *receive* at 4 MiB by default on both sides
	// and would reject a response after the command had already run, which the
	// SSH bridge never did because it wrote straight to the channel. Go has no
	// -1 sentinel, so math.MaxInt32 is the unlimited idiom and it matches the
	// send default. The truncation path below is built and tested so that
	// imposing a real bound later is a constant, not new code; the number
	// should come from the per-call sizes logged on real runs.
	maxMessageBytes    = math.MaxInt32
	defaultOutputLimit = int64(math.MaxInt32)
)

// Options are the host-local inputs to one Hermes gRPC bridge.
type Options struct {
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	// ClientPath is the host path of the aries-grpc executable staged into the
	// harness container.
	ClientPath string
	// OutputLimit bounds retained stdout and stderr per call. Zero selects
	// defaultOutputLimit.
	OutputLimit int64
}

// Manager exposes one gRPC endpoint at a time and proxies its calls to the
// exact Docker sandbox passed to Start.
type Manager struct {
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	clientPath     string
	outputLimit    int64
	openAudit      func(string) (*auditFile, error)

	mu       sync.Mutex
	active   *bridgeSession
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

// bridgeSandbox is the narrow local-sandbox capability this bridge requires,
// asserted from the runner.Sandbox handed to Start.
type bridgeSandbox interface {
	runner.Sandbox
	ContainerID() string
	ContainerName() string
	NetworkName() string
	NetworkGateway(context.Context) (string, error)
	RunID() string
	TaskID() string
	Workdir() string
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

type bridgeSession struct {
	sandbox      bridgeSandbox
	listener     net.Listener
	server       *grpc.Server
	cancel       context.CancelFunc
	artifactDir  string
	clientSource string
	identityFile string
	trustedFile  string
	toolLogPath  string
	audit        *auditWriter
	logger       *logrus.Logger
	outputLimit  int64
	partialStart bool

	revoked       chan struct{}
	revocationMu  sync.Mutex
	revocationErr error
	wait          sync.WaitGroup
	revokeOnce    sync.Once
}

var _ runner.ToolBridge = (*Manager)(nil)

// New constructs a bridge without binding anything.
func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Hermes gRPC output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Hermes gRPC output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Hermes gRPC output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.OutputLimit <= 0 {
		options.OutputLimit = defaultOutputLimit
	}
	if strings.TrimSpace(options.ClientPath) == "" {
		return nil, errors.New("Hermes gRPC client path is required")
	}
	return &Manager{
		outputDir: outputDir, cleanupTimeout: options.CleanupTimeout,
		logger: options.Logger, clientPath: options.ClientPath,
		outputLimit: options.OutputLimit, openAudit: openAuditFile,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("Hermes gRPC bridge is already active")
	}
	sandbox, ok := generic.(bridgeSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("Hermes gRPC bridge requires the local Docker sandbox capability")
	}
	gateway, err := sandbox.NetworkGateway(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve task network gateway: %w", err)
	}

	session := &bridgeSession{
		sandbox: sandbox, revoked: make(chan struct{}),
		logger: manager.logger, outputLimit: manager.outputLimit,
	}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")

	// Start may fail after allocating task-local resources. Stop is idempotent,
	// so every Start attempt must still be followed by a positive revocation
	// confirmation; fail leaves the session recorded when its own cleanup fails
	// so that Stop can retry.
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		if waitErr := session.waitFor(cleanupCtx); waitErr != nil {
			manager.active = session
			return core.ToolEndpoint{}, errors.Join(primary, waitErr)
		}
		if cleanupErr := session.finalize(cleanupCtx); cleanupErr != nil {
			manager.active = session
			return core.ToolEndpoint{}, errors.Join(primary, cleanupErr)
		}
		return core.ToolEndpoint{}, primary
	}

	if err := ensurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private Hermes gRPC artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.artifactDir, "aries-grpc")
	if err := stageExecutable(manager.clientPath, session.clientSource); err != nil {
		return fail(fmt.Errorf("stage Hermes gRPC client: %w", err))
	}

	credentialMaterial, err := generateSessionCertificates(gateway)
	if err != nil {
		return fail(err)
	}
	session.identityFile = filepath.Join(session.artifactDir, "client.pem")
	session.trustedFile = filepath.Join(session.artifactDir, "server.crt")
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	if err := writeExclusivePrivate(session.identityFile, credentialMaterial.identity); err != nil {
		return fail(fmt.Errorf("write Hermes gRPC client identity: %w", err))
	}
	if err := writeExclusivePrivate(session.trustedFile, credentialMaterial.trusted); err != nil {
		return fail(fmt.Errorf("write Hermes gRPC trusted certificate: %w", err))
	}

	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return fail(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener

	structured, err := manager.openAudit(session.toolLogPath)
	if err != nil {
		return fail(fmt.Errorf("create Hermes gRPC tool log: %w", err))
	}
	session.audit = newAuditWriter(structured)

	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.server = grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMessageBytes), grpc.MaxSendMsgSize(maxMessageBytes),
		grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{credentialMaterial.server},
			ClientAuth:   tls.RequireAnyClientCert,
			MinVersion:   tls.VersionTLS13,
			// Exactly one client is authorized, pinned by its raw certificate
			// bytes. This mirrors the SSH bridge's exact public-key comparison
			// rather than trusting a certificate authority.
			VerifyPeerCertificate: pinnedPeer(credentialMaterial.client),
		})))
	sandboxv1.RegisterSandboxServer(session.server, &service{session: session, serveCtx: serveCtx})

	session.wait.Add(1)
	go func() {
		defer session.wait.Done()
		_ = session.server.Serve(listener)
	}()

	manager.active = session
	manager.stopErr = nil
	address := net.JoinHostPort(listenerHost(listener), listenerPort(listener))
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{
		"address": address, "network": network, "container": sandbox.ContainerName(),
	}).Info("Hermes gRPC bridge started")
	// The SSH-shaped names carry their SSH meanings: IdentityFile is the
	// client's own credential, KnownHostsFile the server identity it must
	// accept and nothing else.
	return core.ToolEndpoint{
		Protocol: "grpc", Address: address, Username: lockedUsername, Network: network,
		ClientCommand: clientContainerPath, ClientSourceFile: session.clientSource,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identityFile,
		KnownHostsFile: trustedContainerPath, KnownHostsSourceFile: session.trustedFile,
		LogPaths: []string{session.toolLogPath},
	}, nil
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	if manager.active == nil && !manager.stopping {
		err := manager.stopErr
		manager.mu.Unlock()
		return err
	}
	if manager.stopping {
		done := manager.stopDone
		manager.mu.Unlock()
		select {
		case <-done:
			manager.mu.Lock()
			err := manager.stopErr
			manager.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	session := manager.active
	manager.stopping = true
	manager.stopDone = make(chan struct{})
	done := manager.stopDone
	manager.mu.Unlock()

	session.revoke()
	err := session.waitFor(ctx)
	if err == nil {
		err = session.finalize(ctx)
	}

	manager.mu.Lock()
	manager.stopErr = err
	manager.stopping = false
	if err == nil {
		manager.active = nil
	}
	close(done)
	manager.mu.Unlock()
	return err
}

// revoke marks the session revoked before touching the transport, so a call
// arriving concurrently is refused by construction rather than by timing. The
// SSH bridge closes its listener first and has a narrow window where a
// connection accepted but not yet registered escapes the close loop.
func (session *bridgeSession) revoke() {
	session.revokeOnce.Do(func() {
		close(session.revoked)
		if session.cancel != nil {
			session.cancel()
		}
		if session.server != nil {
			session.server.Stop()
		} else if session.listener != nil {
			_ = session.listener.Close()
		}
	})
}

func (session *bridgeSession) waitFor(ctx context.Context) error {
	done := make(chan struct{})
	go func() { session.wait.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (session *bridgeSession) finalize(ctx context.Context) error {
	auditErr := session.closeAudit(ctx)
	if session.audit != nil && !session.audit.finished() {
		return auditErr
	}
	// Only the client identity is removed; that is revocation. The server
	// certificate is retained as evidence of what the harness was told to
	// trust, exactly as the SSH bridge retains its known-hosts line.
	cleanupErr := errors.Join(
		session.revocationError(), auditErr,
		removeIfPresent(session.identityFile),
	)
	if session.partialStart && cleanupErr == nil {
		cleanupErr = os.RemoveAll(session.artifactDir)
	}
	return cleanupErr
}

func (session *bridgeSession) closeAudit(ctx context.Context) error {
	if session.audit == nil {
		return nil
	}
	return session.audit.sealAndWait(ctx)
}

func (session *bridgeSession) recordRevocationError(err error) {
	if !hasCancellationCause(err) {
		return
	}
	session.revocationMu.Lock()
	session.revocationErr = errors.Join(session.revocationErr, err)
	session.revocationMu.Unlock()
}

func (session *bridgeSession) revocationError() error {
	session.revocationMu.Lock()
	defer session.revocationMu.Unlock()
	if isPureCancellation(session.revocationErr) {
		return nil
	}
	return session.revocationErr
}

func (session *bridgeSession) isRevoked() bool {
	select {
	case <-session.revoked:
		return true
	default:
		return false
	}
}

// service implements the generated server. It holds the session rather than
// the manager so that a call can never observe a later session.
type service struct {
	sandboxv1.UnimplementedSandboxServer
	session  *bridgeSession
	serveCtx context.Context
}

func (svc *service) Exec(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
	session := svc.session
	// The payload is read before the guard so a refusal records what was
	// attempted. Reading a field runs nothing.
	payload := request.GetScript()
	if err := session.authorize(payload); err != nil {
		return nil, err
	}

	// The allowlist is enforced here rather than in the client: the harness
	// container holds the credentials, so a check that ran there could be
	// routed around. Refusing a file sync keeps harness scaffold and the
	// credential files iter_sync_files collects out of the container the
	// verifier later inspects.
	remote, decodeErr := decodeRemoteCommand(payload)
	if decodeErr != nil {
		if errors.Is(decodeErr, errSyncDenied) {
			session.logRequestFailure(payload, kindSync, "denied", errSyncDenied.Error())
			return nil, status.Error(codes.PermissionDenied, errSyncDenied.Error())
		}
		session.logRequestFailure(payload, kindUnknown, "rejected", "invalid remote command")
		return nil, status.Error(codes.InvalidArgument, "invalid remote command")
	}
	prepared, prepareErr := prepareRemoteCommand(remote, session.sandbox.Workdir())
	if prepareErr != nil {
		session.logRequestFailure(payload, remote.kind, "rejected", "invalid remote command")
		return nil, status.Error(codes.InvalidArgument, "invalid remote command")
	}

	// The serve context carries revocation; the call context carries the
	// client going away. Either must abort the command.
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-svc.serveCtx.Done():
			cancel()
		case <-callCtx.Done():
		}
	}()

	command := prepared.command

	started := time.Now()
	stdin := &recordedInput{reader: bytes.NewReader(request.GetStdin())}
	var stdoutBuffer, stderrBuffer bytes.Buffer
	stdout := newBoundedWriter(&stdoutBuffer, session.outputLimit)
	stderr := newBoundedWriter(&stderrBuffer, session.outputLimit)

	result, execErr := session.sandbox.ExecStream(callCtx, command, stdin, stdout, stderr)
	if contextErr := callCtx.Err(); contextErr != nil && !hasCancellationCause(execErr) {
		// A sandbox error returned after revocation is ambiguous unless it
		// carries the cancellation cause. Preserve both so Stop fails closed
		// rather than treating an unconfirmed termination as an earlier error.
		if execErr == nil {
			execErr = contextErr
		} else {
			execErr = errors.Join(contextErr, execErr)
		}
	}

	exitCode := result.ExitCode
	reason := sandboxv1.Reason_REASON_COMPLETED
	statusText, message := "completed", ""
	if execErr != nil {
		session.recordRevocationError(execErr)
		exitCode = 255
		reason = sandboxv1.Reason_REASON_SANDBOX_ERROR
		statusText, message = "failed", "sandbox execution failed"
		if hasCancellationCause(execErr) {
			reason = sandboxv1.Reason_REASON_CANCELED
			statusText, message = "canceled", "session canceled"
			if errors.Is(execErr, context.DeadlineExceeded) {
				reason = sandboxv1.Reason_REASON_TIMED_OUT
			}
		}
	}
	if exitCode < 0 || exitCode > 255 {
		exitCode = 255
	}

	stdinBytes, stdinContent, stdinEncoding, stdinRaw, stdinOverflow := stdin.record()
	if stdinOverflow {
		session.audit.latch(fmt.Errorf("retain Hermes gRPC stdin: input exceeds %d bytes", maxRecordedInputBytes))
		return nil, status.Error(codes.ResourceExhausted, "stdin exceeds the retained bound")
	}

	// The command itself completed; only the retained output was cut, so the
	// reason is left alone and truncation is reported in its own field.
	truncated := stdout.truncated() || stderr.truncated()
	duration := time.Since(started).Milliseconds()

	// commandHash and Command are taken from the request field directly. The
	// canonical round-trip check in decodeShellToken already proves a
	// re-encoding would reproduce it, so there is nothing to re-encode.
	session.writeRecord(toolCallRecord{
		OperationClass: prepared.kind,
		Path:           command.Path,
		Workdir:        command.Dir,
		CommandHash:    commandHash(payload),
		Command:        payload,
		Argv:           append([]string{command.Path}, command.Args...),
		Stdin:          stdinContent, StdinEncoding: stdinEncoding, StdinRaw: stdinRaw, StdinBytes: stdinBytes,
		StdoutBytes: stdout.count(), StderrBytes: stderr.count(), Truncated: truncated,
		ExitCode:   int(exitCode),
		DurationMS: duration,
		Status:     statusText, Error: message,
	})

	// Per-call sizes are surfaced here so a real run supplies the numbers the
	// output bound should eventually be chosen from.
	session.logger.WithFields(logrus.Fields{
		"kind": prepared.kind, "status": statusText, "exit_code": exitCode,
		"stdin_bytes": stdinBytes, "stdout_bytes": stdout.count(), "stderr_bytes": stderr.count(),
		"truncated": truncated, "duration_ms": duration,
	}).Info("Hermes gRPC exec")

	return &sandboxv1.ExecResponse{
		ExitCode:  int32(exitCode),
		Reason:    reason,
		Stdout:    stdoutBuffer.Bytes(),
		Stderr:    stderrBuffer.Bytes(),
		Truncated: truncated,
	}, nil
}

// authorize refuses a call on a revoked session and records the refusal. A
// refused call is a recordable event: the SSH bridge leaves several refusal
// classes with no audit entry at all, and that gap is deliberately not
// reproduced.
//
// There is no per-call identity token. Identity is settled at the TLS
// handshake, where exactly one client certificate is accepted by raw bytes,
// and the SSH bridge likewise checks its pinned key once and nothing per call.
// The revocation check below is the part that has no SSH counterpart: it makes
// revocation provable per call rather than only by the transport having closed.
func (session *bridgeSession) authorize(payload string) error {
	if session.isRevoked() {
		session.logRequestFailure(payload, kindUnknown, "rejected", "session revoked")
		return status.Error(codes.FailedPrecondition, "session revoked")
	}
	return nil
}

// logRequestFailure records a call that never reached the sandbox. Unlike the
// SSH bridge, which stores only a hash and keeps the bytes in ssh_raw.log, the
// verbatim payload is recorded here: a payload that failed to decode has no
// canonical encoding, so a hash alone would leave it unrecoverable.
func (session *bridgeSession) logRequestFailure(payload, kind, status, message string) {
	session.writeRecord(toolCallRecord{
		OperationClass: kind,
		CommandHash:    commandHash(payload),
		Command:        payload,
		// The call never ran, so the record must not carry an exit code that
		// could be mistaken for one.
		ExitCode:      -1,
		Status:        status,
		Error:         message,
		Stdin:         "",
		StdinEncoding: "utf-8",
	})
}

func (session *bridgeSession) writeRecord(record toolCallRecord) {
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	record.ContainerID = session.sandbox.ContainerID()
	record.ContainerName = session.sandbox.ContainerName()
	session.audit.enqueue(record)
}
