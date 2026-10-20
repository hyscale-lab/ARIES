// Package grpc serves the aries.sandbox.v1 contract, granting one harness
// narrow, audited, revocable access to one task sandbox through a borrowed
// target. The server is plaintext and authenticates no caller.
package grpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultBridgeCleanup = 20 * time.Second

	// lockedUsername is the endpoint user the run service requires, as on SSH.
	lockedUsername = "aries"

	// defaultOutputLimit truncates each of stdout and stderr. Unary Exec holds a
	// whole reply in this process — the handler's buffer plus the marshalled
	// copy — where the SSH bridge streamed to the channel and accumulated
	// nothing, so this bound covers memory the gRPC design newly puts at risk.
	// It matters because the bridge is shared across tasks: if it dies, Stop
	// never runs, so revocation is unconfirmed and the audit unsealed for every
	// concurrent task, not just the one that produced the output.
	//
	// 16 MiB is far above anything measured — a Terminal-Bench run peaked at
	// 5.6 KB — and a high bound costs nothing until it fires. Erring high is
	// deliberate: truncation is not free either, because the agent stops seeing
	// the output it asked for, which can change a task's outcome.
	//
	// With the Docker backend this bound does not fire: Docker fails an exec
	// whose stdout or stderr passes its own 16 MiB maxExecOutput first.
	defaultOutputLimit = 16 << 20

	// maxMessageBytes is a backstop and must never be the binding constraint.
	// If the transport cap fired first, a command would run to completion and
	// then have its reply rejected — the exact failure mode truncation exists to
	// avoid, and the one grpc-go's 4 MiB receive default would have caused. Two
	// fully truncated streams plus framing must fit well inside it.
	maxMessageBytes = 64 << 20

	// refusedExitCode marks a record for a call that never ran, as Hermes's SSH
	// dialect does.
	refusedExitCode = -1
	kindAgent       = "agent"
)

// Options are the host-local inputs to one gRPC bridge.
type Options struct {
	SandboxID string
	// ResolveListen supplies the local binding. Deployment resolves reachable endpoints.
	ResolveListen  func(context.Context) (core.BridgeListen, error)
	OutputDir      string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	// OmitRawLog drops grpc_raw.log, the only artifact holding binary stdin
	// and written content that the structured log omits.
	OmitRawLog bool
}

// Manager owns one occurrence and serves its calls to the borrowed target.
type Manager struct {
	used           bool
	sandboxID      string
	resolveListen  func(context.Context) (core.BridgeListen, error)
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	omitRawLog     bool

	mu       sync.Mutex
	active   *bridgeSession
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

type bridgeSession struct {
	sandbox      target.Executor
	sandboxID    string
	listener     net.Listener
	server       *grpc.Server
	serveCtx     context.Context
	cancel       context.CancelFunc
	artifactDir  string
	toolLogPath  string
	rawLogPath   string
	audit        *bridgekit.AuditWriter
	partialStart bool

	// revoked is set before the transport is touched, so a call arriving
	// concurrently is refused by construction rather than by timing.
	revoked       atomic.Bool
	revokeOnce    sync.Once
	wait          sync.WaitGroup
	mu            sync.Mutex
	done          chan struct{}
	revocationMu  sync.Mutex
	revocationErr error
}

func New(options Options) (*Manager, error) {
	if options.SandboxID == "" {
		return nil, errors.New("gRPC sandbox ID is required")
	}
	if options.ResolveListen == nil {
		return nil, errors.New("gRPC bridge listen resolver is required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("gRPC output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve gRPC output directory: %w", err)
	}
	if err := bridgekit.EnsurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare gRPC output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		sandboxID: options.SandboxID, resolveListen: options.ResolveListen,
		outputDir: outputDir, cleanupTimeout: options.CleanupTimeout,
		logger: options.Logger, omitRawLog: options.OmitRawLog,
	}, nil
}

// StartTarget serves a borrowed execution capability without sandbox lifecycle authority.
func (manager *Manager) StartTarget(ctx context.Context, sandbox target.Executor) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.used || manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("gRPC bridge is already active")
	}
	if sandbox == nil {
		return core.ToolEndpoint{}, errors.New("gRPC target is required")
	}
	listen, err := manager.resolveListen(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve bridge listener: %w", err)
	}
	bindIP := net.ParseIP(listen.BindHost)
	if bindIP == nil || listen.BindPort < 0 || listen.BindPort > 65535 {
		return core.ToolEndpoint{}, errors.New("gRPC bridge requires a bind IP and valid port")
	}
	manager.used = true
	session := &bridgeSession{sandbox: sandbox, sandboxID: manager.sandboxID, artifactDir: manager.outputDir}
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		cleanupErr := session.finalize(cleanupCtx)
		if cleanupErr != nil {
			manager.active = session
		}
		return core.ToolEndpoint{}, errors.Join(primary, cleanupErr)
	}
	if err := bridgekit.EnsurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private gRPC artifact directory: %w", err))
	}
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.artifactDir, "grpc_raw.log")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(listen.BindHost, strconv.Itoa(listen.BindPort)))
	if err != nil {
		return fail(fmt.Errorf("listen on configured bridge host: %w", err))
	}
	session.listener = listener
	structured, err := bridgekit.OpenAuditFile(session.toolLogPath)
	if err != nil {
		return fail(fmt.Errorf("create gRPC tool log: %w", err))
	}
	var raw *bridgekit.AuditFile
	if session.rawLogPath != "" {
		raw, err = bridgekit.OpenAuditFile(session.rawLogPath)
		if err != nil {
			return fail(errors.Join(fmt.Errorf("create gRPC raw log: %w", err), structured.Close()))
		}
	}
	session.audit = bridgekit.NewAuditWriter(structured, raw)
	session.serveCtx, session.cancel = context.WithCancel(context.Background())
	session.server = grpc.NewServer(
		grpc.MaxRecvMsgSize(maxMessageBytes), grpc.MaxSendMsgSize(maxMessageBytes),
		// Stop must not return while a handler can still write its record;
		// see revoke.
		grpc.WaitForHandlers(true))
	sandboxv1.RegisterSandboxServer(session.server, &service{session: session})
	session.wait.Add(1)
	go func() {
		defer session.wait.Done()
		_ = session.server.Serve(listener)
	}()
	manager.active = session
	manager.stopErr = nil
	address := listener.Addr().String()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "container": sandbox.ContainerName()}).Info("gRPC bridge started")
	return core.ToolEndpoint{Protocol: "grpc", Address: address, Username: lockedUsername, Workdir: sandbox.Workdir(), LogPaths: session.logPaths()}, nil
}

// logPaths omits grpc_raw.log when it was not retained, so the endpoint never
// advertises an artifact that does not exist.
func (session *bridgeSession) logPaths() []string {
	if session.rawLogPath == "" {
		return []string{session.toolLogPath}
	}
	return []string{session.toolLogPath, session.rawLogPath}
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	if manager.active == nil && !manager.stopping {
		manager.used = true
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
	err := session.finalize(ctx)
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

// revoke marks the session revoked, cancels running calls, and stops the
// server. All cleanup attempts share one waiter, including retries after a
// deadline.
func (session *bridgeSession) revoke() {
	session.revokeOnce.Do(func() {
		session.revoked.Store(true)
		if session.cancel != nil {
			session.cancel()
		}
		if session.server != nil {
			// Stop waits for every handler, so it runs counted in wait and
			// finalize seals the audit only after it returns.
			session.wait.Add(1)
			go func() {
				defer session.wait.Done()
				session.server.Stop()
			}()
		} else if session.listener != nil {
			_ = session.listener.Close()
		}
		done := make(chan struct{})
		session.mu.Lock()
		session.done = done
		session.mu.Unlock()
		go func() { session.wait.Wait(); close(done) }()
	})
}

// finalize joins every handler, then seals the audit. A timeout leaves the
// session pending, so a later Stop can retry.
func (session *bridgeSession) finalize(ctx context.Context) error {
	session.mu.Lock()
	done := session.done
	session.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	auditErr := session.audit.SealAndWait(ctx)
	if !session.audit.Finished() {
		return auditErr
	}
	cleanupErr := errors.Join(session.revocationError(), auditErr)
	if session.partialStart && cleanupErr == nil {
		cleanupErr = os.RemoveAll(session.artifactDir)
	}
	return cleanupErr
}

func (session *bridgeSession) recordRevocationError(err error) {
	if !bridgekit.HasCancellationCause(err) {
		return
	}
	session.revocationMu.Lock()
	session.revocationErr = errors.Join(session.revocationErr, err)
	session.revocationMu.Unlock()
}

func (session *bridgeSession) revocationError() error {
	session.revocationMu.Lock()
	defer session.revocationMu.Unlock()
	if bridgekit.IsPureCancellation(session.revocationErr) {
		return nil
	}
	return session.revocationErr
}

// callContext ends when the client goes away or the session is revoked.
func (session *bridgeSession) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	callCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(session.serveCtx, cancel)
	return callCtx, func() { stop(); cancel() }
}

// service implements the generated server. It holds the session rather than
// the manager so that a call can never observe a later session.
type service struct {
	sandboxv1.UnimplementedSandboxServer
	session *bridgeSession
}

func (svc *service) Exec(ctx context.Context, request *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
	session := svc.session
	script, input := request.GetScript(), request.GetStdin()
	args := []string{"-c", script}
	if request.GetLogin() {
		args = []string{"-l", "-c", script}
	}
	command := core.Command{Path: bridgekit.RemoteShellPath, Args: args, Dir: session.sandbox.Workdir()}
	raw := rawGRPCRecord{OperationClass: kindAgent, Script: script, Login: request.GetLogin()}
	refuse := func(code codes.Code, message string) (*sandboxv1.ExecResponse, error) {
		session.writeRecord(toolCallRecord{ToolCallRecord: bridgekit.ToolCallRecord{
			OperationClass: kindAgent, CommandHash: bridgekit.CommandHash(script), Command: script,
			StdinEncoding: "utf-8", ExitCode: refusedExitCode, Status: "rejected", Error: message,
		}}, raw)
		return nil, status.Error(code, message)
	}
	if session.revoked.Load() {
		return refuse(codes.Unavailable, "session revoked")
	}
	// The whole input is in hand, so the call is refused before anything runs
	// and the refusal is the evidence.
	if len(input) > bridgekit.MaxRecordedInputBytes {
		return refuse(codes.ResourceExhausted, "stdin exceeds the retained bound")
	}
	if !bridgekit.ValidWorkdir(command.Dir) {
		return refuse(codes.FailedPrecondition, "sandbox workdir is not shell-neutral")
	}

	callCtx, cancel := session.callContext(ctx)
	defer cancel()
	started := time.Now()
	var stdoutBuffer, stderrBuffer bytes.Buffer
	stdout := newBoundedWriter(&stdoutBuffer, defaultOutputLimit)
	stderr := newBoundedWriter(&stderrBuffer, defaultOutputLimit)
	result, err := session.sandbox.ExecStream(callCtx, command, bytes.NewReader(input), stdout, stderr)
	err = bridgekit.WithCancellation(callCtx, err)

	exitCode := result.ExitCode
	reason := sandboxv1.Reason_REASON_COMPLETED
	statusText, message := "completed", ""
	if err != nil {
		session.recordRevocationError(err)
		exitCode = 255
		reason = sandboxv1.Reason_REASON_SANDBOX_ERROR
		statusText, message = "failed", "sandbox execution failed"
		if bridgekit.HasCancellationCause(err) {
			reason = sandboxv1.Reason_REASON_CANCELED
			statusText, message = "canceled", "session canceled"
			if errors.Is(err, context.DeadlineExceeded) {
				reason = sandboxv1.Reason_REASON_TIMED_OUT
			}
		}
	}
	if exitCode < 0 || exitCode > 255 {
		exitCode = 255
	}
	// The command itself completed; only the retained output was cut, so the
	// reason is left alone and truncation is reported in its own field.
	truncated := stdout.truncated() || stderr.truncated()
	stdinContent, stdinEncoding := session.describeInput(input, int64(len(input)))
	raw.Stdin, raw.StdinBytes = input, int64(len(input))
	session.writeRecord(toolCallRecord{ToolCallRecord: bridgekit.ToolCallRecord{
		OperationClass: kindAgent, Path: command.Path, Workdir: command.Dir,
		CommandHash: bridgekit.CommandHash(script), Command: script,
		Argv:  append([]string{command.Path}, command.Args...),
		Stdin: stdinContent, StdinEncoding: stdinEncoding, StdinBytes: int64(len(input)),
		StdoutBytes: stdout.count(), StderrBytes: stderr.count(),
		ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: statusText, Error: message,
	}, Truncated: truncated}, raw)
	return &sandboxv1.ExecResponse{
		ExitCode: int32(exitCode), Reason: reason,
		Stdout: stdoutBuffer.Bytes(), Stderr: stderrBuffer.Bytes(), Truncated: truncated,
	}, nil
}

func (session *bridgeSession) writeRecord(record toolCallRecord, raw rawGRPCRecord) {
	record.SandboxID = session.sandboxID
	record.ContainerID = session.sandbox.ContainerID()
	record.ContainerName = session.sandbox.ContainerName()
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	raw.Status = record.Status
	raw.SandboxID = session.sandboxID
	raw.RunID = record.RunID
	raw.TaskID = record.TaskID
	raw.ContainerID = record.ContainerID
	session.audit.Enqueue(&record, raw)
}
