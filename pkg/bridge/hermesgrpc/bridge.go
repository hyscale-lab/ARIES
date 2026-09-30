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
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/hermeswire"
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

	// syncDenied is the recorded reason, and the status message, for a
	// refused Hermes file sync.
	syncDenied = "Hermes gRPC file sync is denied by ARIES policy"

	// identityContainerPath holds the client's own certificate and key;
	// trustedContainerPath holds the single server certificate the client
	// accepts. The harness stages both at these paths.
	identityContainerPath = "/run/aries/grpc/client.pem"
	trustedContainerPath  = "/run/aries/grpc/server.crt"

	// clientContainerPath is where the staged client lands. The ARIES Hermes
	// plugin runs it by this path.
	clientContainerPath = "/run/aries/bin/aries-grpc"

	lockedUsername = "aries"

	// defaultOutputLimit truncates each of stdout and stderr. Unary Exec holds a
	// whole reply in this process — the handler's buffer plus the marshalled
	// copy — where the SSH bridge streamed to the channel and accumulated
	// nothing, so this bound covers memory the gRPC design newly puts at risk.
	// It matters because ARIES is shared across tasks and unconstrained: if it
	// dies, Stop never runs, so revocation is unconfirmed and the audit unsealed
	// for every concurrent task, not just the one that produced the output.
	//
	// 16 MiB is far above anything measured — a Terminal-Bench run peaked at
	// 5.6 KB — and a high bound costs nothing until it fires, since the limit is
	// one comparison against a length already on the wire. Erring high is
	// deliberate: truncation is not free either, because the agent stops seeing
	// the output it asked for, which can change a task's outcome.
	defaultOutputLimit = int64(16 << 20)

	// maxMessageBytes is a backstop and must never be the binding constraint.
	// If the transport cap fired first, a command would run to completion and
	// then have its reply rejected — the exact failure mode truncation exists to
	// avoid, and the one grpc-go's 4 MiB receive default would have caused. Two
	// fully truncated streams plus framing must fit well inside it.
	maxMessageBytes = 64 << 20
)

// Options are the host-local inputs to one Hermes gRPC bridge.
type Options struct {
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	// ClientPath is the host path of the aries-grpc executable staged into the
	// harness container.
	ClientPath string
	// OutputLimit bounds retained stdout and stderr per Exec call. File
	// content streams and has no bound. Zero selects defaultOutputLimit.
	OutputLimit int64
	// RetainContent keeps file content, base64, in tool-calls.jsonl. It is the
	// profile's bridge.retain_raw_log: the most verbose evidence level.
	RetainContent bool
}

// Manager exposes one gRPC endpoint at a time and proxies its calls to the
// exact Docker sandbox passed to Start.
type Manager struct {
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	clientPath     string
	outputLimit    int64
	retainContent  bool
	// auditLimit bounds tool-calls.jsonl; tests lower it.
	auditLimit int64

	slot bridgekit.Slot
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
	runner.StreamExecutor
}

type bridgeSession struct {
	bridgekit.Session
	sandbox      bridgeSandbox
	listener     net.Listener
	server       *grpc.Server
	cancel       context.CancelFunc
	clientSource string
	identityFile string
	trustedFile  string
	toolLogPath  string
	logger       *logrus.Logger
	outputLimit  int64
	// retainContent keeps file content in the audit; see Options.
	retainContent bool

	revoked    chan struct{}
	revokeOnce sync.Once
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
	if err := bridgekit.EnsurePrivateDirectory(outputDir); err != nil {
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
		outputLimit: options.OutputLimit, retainContent: options.RetainContent, auditLimit: maxToolLogBytes,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	var endpoint core.ToolEndpoint
	err := manager.slot.Start(ctx, manager.cleanupTimeout, func() (bridgekit.Closer, error) {
		sandbox, ok := generic.(bridgeSandbox)
		if !ok {
			return nil, errors.New("Hermes gRPC bridge requires the local Docker sandbox capability")
		}
		gateway, err := sandbox.NetworkGateway(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve task network gateway: %w", err)
		}
		session := &bridgeSession{
			sandbox: sandbox, revoked: make(chan struct{}),
			logger: manager.logger, outputLimit: manager.outputLimit, retainContent: manager.retainContent,
		}
		session.ArtifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")
		if endpoint, err = manager.open(ctx, session, gateway); err != nil {
			session.Partial = true
			return session, err
		}
		return session, nil
	})
	if err != nil {
		return core.ToolEndpoint{}, err
	}
	return endpoint, nil
}

func (manager *Manager) Stop(ctx context.Context) error {
	return manager.slot.Stop(ctx)
}

// Close revokes the session and confirms it. Only the client identity is
// removed; that is revocation. The server certificate is retained as evidence
// of what the harness was told to trust, exactly as the SSH bridge retains its
// known-hosts line.
func (session *bridgeSession) Close(ctx context.Context) error {
	session.revoke()
	return session.Finalize(ctx, session.identityFile)
}

// open allocates the session's client, credentials, listener, audit, and
// server. On error the caller closes the partial session.
func (manager *Manager) open(ctx context.Context, session *bridgeSession, gateway string) (core.ToolEndpoint, error) {
	failed := func(err error) (core.ToolEndpoint, error) { return core.ToolEndpoint{}, err }
	sandbox := session.sandbox
	if err := bridgekit.EnsurePrivateDirectory(session.ArtifactDir); err != nil {
		return failed(fmt.Errorf("create private Hermes gRPC artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.ArtifactDir, "aries-grpc")
	if err := bridgekit.StageExecutable(manager.clientPath, session.clientSource); err != nil {
		return failed(fmt.Errorf("stage Hermes gRPC client: %w", err))
	}

	credentialMaterial, err := generateSessionCertificates(gateway)
	if err != nil {
		return failed(err)
	}
	session.identityFile = filepath.Join(session.ArtifactDir, "client.pem")
	session.trustedFile = filepath.Join(session.ArtifactDir, "server.crt")
	session.toolLogPath = filepath.Join(session.ArtifactDir, "tool-calls.jsonl")
	if err := bridgekit.WriteExclusivePrivate(session.identityFile, credentialMaterial.identity); err != nil {
		return failed(fmt.Errorf("write Hermes gRPC client identity: %w", err))
	}
	if err := bridgekit.WriteExclusivePrivate(session.trustedFile, credentialMaterial.trusted); err != nil {
		return failed(fmt.Errorf("write Hermes gRPC trusted certificate: %w", err))
	}

	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return failed(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener

	if session.Audit, err = bridgekit.Open(session.toolLogPath, "", manager.auditLimit); err != nil {
		return failed(fmt.Errorf("open Hermes gRPC audit: %w", err))
	}

	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.server = grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMessageBytes), grpc.MaxSendMsgSize(maxMessageBytes),
		// Stop must not return while a handler can still write its record;
		// see revoke.
		grpc.WaitForHandlers(true),
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

	session.Wait.Add(1)
	go func() {
		defer session.Wait.Done()
		_ = session.server.Serve(listener)
	}()

	// Addr().String() is already built with net.JoinHostPort, zone included, so
	// splitting it apart to rejoin it can only lose information: a failed split
	// would yield ":" and advertise an endpoint that reaches nothing.
	address := listener.Addr().String()
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
		LogPaths: []string{session.toolLogPath}, Workdir: sandbox.Workdir(),
	}, nil
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
			// Stop waits for every handler, so it runs counted in Wait:
			// Finalize then waits for it under its own bounded context
			// before sealing the audit. revoke always runs before that Wait,
			// so this Add never races it.
			session.Wait.Add(1)
			go func() {
				defer session.Wait.Done()
				session.server.Stop()
			}()
		} else if session.listener != nil {
			_ = session.listener.Close()
		}
	})
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
	command, kind, prepareErr := hermeswire.Prepare(payload, session.sandbox.Workdir())
	if errors.Is(prepareErr, hermeswire.ErrSyncDenied) {
		session.logRequestFailure(payload, kind, "denied", syncDenied)
		return nil, status.Error(codes.PermissionDenied, syncDenied)
	}
	if prepareErr != nil {
		session.logRequestFailure(payload, kind, "rejected", "invalid remote command")
		return nil, status.Error(codes.InvalidArgument, "invalid remote command")
	}

	// Checked before the command runs. The SSH bridge can only discover this
	// mid-stream and has to latch an audit error, which blocks revocation;
	// here the whole input is already in hand, so the call is refused outright
	// and the refusal is the evidence.
	input := request.GetStdin()
	if int64(len(input)) > maxRecordedInputBytes {
		session.logRequestFailure(payload, kind, "rejected", "stdin exceeds the retained bound")
		return nil, status.Error(codes.ResourceExhausted, "stdin exceeds the retained bound")
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

	started := time.Now()
	stdin := bytes.NewReader(input)
	var stdoutBuffer, stderrBuffer bytes.Buffer
	stdout := newBoundedWriter(&stdoutBuffer, session.outputLimit)
	stderr := newBoundedWriter(&stderrBuffer, session.outputLimit)

	result, execErr := session.sandbox.ExecStream(callCtx, command, stdin, stdout, stderr)
	execErr = bridgekit.WithCancellation(callCtx, execErr)

	exitCode := result.ExitCode
	reason := sandboxv1.Reason_REASON_COMPLETED
	statusText, message := "completed", ""
	if execErr != nil {
		session.RecordRevocationError(execErr)
		exitCode = 255
		reason = sandboxv1.Reason_REASON_SANDBOX_ERROR
		statusText, message = "failed", "sandbox execution failed"
		if bridgekit.HasCancellationCause(execErr) {
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

	stdinContent, stdinEncoding, stdinRaw := describeStdin(input)

	// The command itself completed; only the retained output was cut, so the
	// reason is left alone and truncation is reported in its own field.
	truncated := stdout.truncated() || stderr.truncated()
	duration := time.Since(started).Milliseconds()

	// commandHash and Command are taken from the request field directly:
	// hermeswire accepts only the canonical encoding, so it is replayable.
	session.writeRecord(toolCallRecord{
		OperationClass: kind,
		Path:           command.Path,
		Workdir:        command.Dir,
		CommandHash:    bridgekit.CommandHash(payload),
		Command:        payload,
		Argv:           append([]string{command.Path}, command.Args...),
		Stdin:          stdinContent, StdinEncoding: stdinEncoding, StdinRaw: stdinRaw, StdinBytes: int64(len(input)),
		StdoutBytes: stdout.count(), StderrBytes: stderr.count(), Truncated: truncated,
		ExitCode:   int(exitCode),
		DurationMS: duration,
		Status:     statusText, Error: message,
	})

	// Per-call sizes are surfaced here so a real run supplies the numbers the
	// output bound should eventually be chosen from.
	session.logger.WithFields(logrus.Fields{
		"kind": kind, "status": statusText, "exit_code": exitCode,
		"stdin_bytes": len(input), "stdout_bytes": stdout.count(), "stderr_bytes": stderr.count(),
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
		session.logRequestFailure(payload, hermeswire.KindUnknown, "rejected", "session revoked")
		return status.Error(codes.Unavailable, "session revoked")
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
		CommandHash:    bridgekit.CommandHash(payload),
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
	session.Audit.Enqueue(&record, nil, len(record.ContentRaw))
}
