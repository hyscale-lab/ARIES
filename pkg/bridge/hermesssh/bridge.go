package hermesssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/sshbridge"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultBridgeCleanup  = 20 * time.Second
	defaultSupervisorPath = "bin/aries-exec"

	identityContainerPath = "/run/aries/ssh/id_ed25519"
	lockedUsername        = "aries"
)

// Options are the host-local inputs to one Hermes SSH bridge.
type Options struct {
	OutputDir      string
	SupervisorPath string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
	// OmitRawLog drops ssh_raw.log, the byte-level record of every channel
	// request. That log is the only artifact holding the raw wire command,
	// the request payload, and binary stdin that the structured log omits,
	// so it is the forensic record rather than a duplicate of
	// tool-calls.jsonl. The zero value retains it; setting this trades that
	// evidence for disk.
	OmitRawLog bool
}

// Manager exposes one SSH endpoint at a time and proxies its exec requests to
// the exact Docker sandbox passed to Start.
type Manager struct {
	outputDir      string
	supervisorPath string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	openAudit      func(string) (*sshbridge.AuditFile, error)
	afterStart     func(*bridgeSession) error
	omitRawLog     bool

	mu       sync.Mutex
	active   *bridgeSession
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

type bridgeSandbox interface {
	runner.Sandbox
	ContainerID() string
	ContainerName() string
	NetworkName() string
	NetworkGateway(context.Context) (string, error)
	RunID() string
	TaskID() string
	Workdir() string
	StartAgentSession(context.Context, string) error
	ExecAgentStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
	StopAgentSession(context.Context) error
}

type bridgeSession struct {
	sandbox               bridgeSandbox
	server                *sshbridge.Server
	artifactDir           string
	identitySource        string
	knownSource           string
	toolLogPath           string
	rawLogPath            string
	audit                 *sshbridge.AuditWriter
	partialStart          bool
	agentSessionAttempted bool
	agentStopErr          error
	replyRequest          func(*ssh.Request, bool) error

	revocationMu  sync.Mutex
	revocationErr error
}

type requestAudit struct {
	requestType   string
	wantReply     bool
	payload       []byte
	remoteCommand string
}

var _ runner.ToolBridge = (*Manager)(nil)

func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Hermes SSH output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Hermes SSH output directory: %w", err)
	}
	if err := sshbridge.EnsurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Hermes SSH output directory: %w", err)
	}
	if options.SupervisorPath == "" {
		options.SupervisorPath = defaultSupervisorPath
	}
	supervisorPath, err := filepath.Abs(options.SupervisorPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Hermes SSH supervisor: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		outputDir: outputDir, supervisorPath: supervisorPath, cleanupTimeout: options.CleanupTimeout,
		logger: options.Logger, openAudit: sshbridge.OpenAuditFile, omitRawLog: options.OmitRawLog,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("Hermes SSH bridge is already active")
	}
	sandbox, ok := generic.(bridgeSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("Hermes SSH bridge requires the local Docker sandbox capability")
	}
	gateway, err := sandbox.NetworkGateway(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve task network gateway: %w", err)
	}
	session := &bridgeSession{
		sandbox:      sandbox,
		replyRequest: func(request *ssh.Request, accepted bool) error { return request.Reply(accepted, nil) },
	}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.server.Revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		waitErr := session.server.Wait(cleanupCtx)
		if waitErr != nil {
			manager.active = session
			return core.ToolEndpoint{}, errors.Join(primary, waitErr)
		}
		cleanupErr := session.finalize(cleanupCtx)
		if cleanupErr != nil {
			manager.active = session
		}
		return core.ToolEndpoint{}, errors.Join(primary, cleanupErr)
	}
	if err := sshbridge.EnsurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private Hermes SSH artifact directory: %w", err))
	}
	hostSigner, clientPEM, authorized, err := sshbridge.GenerateSessionKeys()
	if err != nil {
		return fail(err)
	}
	session.identitySource = filepath.Join(session.artifactDir, "id_ed25519")
	session.knownSource = filepath.Join(session.artifactDir, "known_hosts")
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.artifactDir, "ssh_raw.log")
	}
	if err := sshbridge.WriteExclusivePrivate(session.identitySource, clientPEM); err != nil {
		return fail(fmt.Errorf("write Hermes SSH identity: %w", err))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return fail(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.server = sshbridge.NewServer(listener, hostSigner, authorized)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fail(fmt.Errorf("parse Hermes SSH listener address: %w", err))
	}
	// Retained as evidence of the host key Hermes pins on first use. Hermes
	// forces StrictHostKeyChecking=accept-new and offers no way to preload a
	// known-hosts file, so this is not handed to the harness.
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := sshbridge.WriteExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return fail(fmt.Errorf("write Hermes SSH known-hosts file: %w", err))
	}
	structured, err := manager.openAudit(session.toolLogPath)
	if err != nil {
		return fail(fmt.Errorf("create Hermes SSH tool log: %w", err))
	}
	var raw *sshbridge.AuditFile
	if session.rawLogPath != "" {
		raw, err = manager.openAudit(session.rawLogPath)
		if err != nil {
			return fail(errors.Join(fmt.Errorf("create Hermes SSH raw log: %w", err), structured.Close()))
		}
	}
	session.audit = sshbridge.NewAuditWriter("Hermes", structured, raw)
	session.agentSessionAttempted = true
	if err := sandbox.StartAgentSession(ctx, manager.supervisorPath); err != nil {
		return fail(fmt.Errorf("start Hermes agent supervision: %w", err))
	}
	session.server.Start(session.handleSession, manager.logger)
	if manager.afterStart != nil {
		if err := manager.afterStart(session); err != nil {
			return fail(err)
		}
	}
	manager.active = session
	manager.stopErr = nil
	address := net.JoinHostPort(host, port)
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": network, "container": sandbox.ContainerName()}).Info("Hermes SSH bridge started")
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
		LogPaths: session.logPaths(),
	}, nil
}

// logPaths omits ssh_raw.log when it was not retained, so the endpoint never
// advertises an artifact that does not exist.
func (session *bridgeSession) logPaths() []string {
	if session.rawLogPath == "" {
		return []string{session.toolLogPath}
	}
	return []string{session.toolLogPath, session.rawLogPath}
}

func (session *bridgeSession) handleSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	for request := range requests {
		audit := requestAudit{requestType: request.Type, wantReply: request.WantReply, payload: bytes.Clone(request.Payload)}
		if request.Type != "exec" {
			// OpenSSH sends an `env` request on every channel before the exec.
			// Refusing it is correct and expected, but the channel must stay
			// open or Hermes loses every command it ever issues. The refusal is
			// still recorded: the wire-side evidence is only lossless if every
			// request Hermes sends appears, including the ones ARIES declines.
			if request.WantReply {
				_ = session.reply(request, false)
			}
			session.logRequestFailure(audit, kindUnknown, "unsupported", "channel request type is not exec")
			continue
		}
		if !request.WantReply {
			session.logRejected(audit, kindUnknown)
			return
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = session.reply(request, false)
			session.logRejected(audit, kindUnknown)
			return
		}
		audit.remoteCommand = payload.Command
		command, err := decodeRemoteCommand(payload.Command)
		if err != nil {
			_ = session.reply(request, false)
			if errors.Is(err, errSyncDenied) {
				session.logRequestFailure(audit, kindSync, "denied", errSyncDenied.Error())
			} else {
				session.logRejected(audit, kindUnknown)
			}
			return
		}
		prepared, err := prepareRemoteCommand(command, session.sandbox.Workdir())
		if err != nil {
			_ = session.reply(request, false)
			session.logRejected(audit, command.kind)
			return
		}
		if err := session.reply(request, true); err != nil {
			session.logRequestFailure(audit, prepared.kind, "failed", "SSH accept reply failed")
			return
		}
		exitCode := session.execute(ctx, channel, prepared, audit)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
		return
	}
}

// reply routes through session.replyRequest, which Start always populates and
// tests override to observe accept/reject outcomes.
func (session *bridgeSession) reply(request *ssh.Request, accepted bool) error {
	return session.replyRequest(request, accepted)
}

func (session *bridgeSession) execute(ctx context.Context, channel ssh.Channel, prepared preparedRemoteCommand, audit requestAudit) int {
	started := time.Now()
	stdin := sshbridge.NewRecordedInput("Hermes", channel)
	stdout := &sshbridge.ByteCounter{Writer: channel}
	stderr := &sshbridge.ByteCounter{Writer: channel.Stderr()}
	result, err := session.sandbox.ExecAgentStream(ctx, prepared.command, stdin, stdout, stderr)
	if contextErr := ctx.Err(); contextErr != nil && !hasCancellationCause(err) {
		// A sandbox error returned after revocation is ambiguous unless it carries
		// the cancellation cause. Preserve both so Stop fails closed rather than
		// silently treating an unconfirmed tool termination as an earlier error.
		if err == nil {
			err = contextErr
		} else {
			err = errors.Join(contextErr, err)
		}
	}
	exitCode := result.ExitCode
	status, message := "completed", ""
	if err != nil {
		session.recordRevocationError(err)
		exitCode = 255
		status, message = "failed", "sandbox execution failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status, message = "canceled", "session canceled"
		}
	}
	if exitCode < 0 || exitCode > 255 {
		exitCode = 255
	}
	stdinBytes, stdinContent, stdinEncoding, rawStdin, stdinOverflow := stdin.Record(session.audit.RetainsRaw())
	if stdinOverflow {
		session.audit.Latch(fmt.Errorf("retain Hermes SSH stdin: input exceeds %d bytes", sshbridge.MaxRecordedInputBytes))
		return exitCode
	}
	session.writeRecord(sshbridge.ToolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: prepared.kind, Path: prepared.command.Path, Workdir: prepared.command.Dir,
		CommandHash: commandHash(prepared.encoded),
		Command:     prepared.encoded,
		Argv:        append([]string{prepared.command.Path}, prepared.command.Args...),
		Stdin:       stdinContent, StdinEncoding: stdinEncoding,
		StdinBytes: stdinBytes, StdoutBytes: stdout.Count(), StderrBytes: stderr.Count(),
		ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message,
		RequestType: audit.requestType, WantReply: audit.wantReply,
	}, rawRecord(audit, stdinBytes, rawStdin, status))
	return exitCode
}

func (session *bridgeSession) logRejected(audit requestAudit, kind string) {
	session.logRequestFailure(audit, kind, "rejected", "invalid remote command")
}

// logRequestFailure records a request that never reached the sandbox. The kind
// is whatever decoding established before the failure, so a refused file sync
// is not filed as an agent command; kindUnknown marks a payload that never
// decoded far enough to classify.
func (session *bridgeSession) logRequestFailure(audit requestAudit, kind, status, message string) {
	session.writeRecord(sshbridge.ToolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: kind, CommandHash: commandHash(audit.remoteCommand),
		StdinEncoding: "utf-8",
		// The request never ran, so the record must not carry the exit code of
		// a successful command.
		ExitCode: -1,
		Status:   status, Error: message,
		RequestType: audit.requestType, WantReply: audit.wantReply,
	}, rawRecord(audit, 0, nil, status))
}

func rawRecord(audit requestAudit, stdinBytes int64, stdin []byte, status string) sshbridge.RawRecord {
	return sshbridge.RawRecord{
		RequestType: audit.requestType, WantReply: audit.wantReply,
		WireCommand: audit.remoteCommand, Payload: bytes.Clone(audit.payload), PayloadBytes: int64(len(audit.payload)),
		Stdin: bytes.Clone(stdin), StdinBytes: stdinBytes, Status: status,
	}
}

func (session *bridgeSession) writeRecord(record sshbridge.ToolCallRecord, raw sshbridge.RawRecord) {
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	raw.RunID = session.sandbox.RunID()
	raw.TaskID = session.sandbox.TaskID()
	raw.ContainerID = session.sandbox.ContainerID()
	session.audit.Enqueue(record, raw)
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

	session.server.Revoke()
	err := session.server.Wait(ctx)
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

func (session *bridgeSession) finalize(ctx context.Context) error {
	if session.agentSessionAttempted {
		// A later cleanup retry cannot erase a missing descendant proof.
		session.agentStopErr = errors.Join(session.agentStopErr, session.sandbox.StopAgentSession(ctx))
	}
	auditErr := session.audit.SealAndWait(ctx)
	if session.audit != nil && !session.audit.Finished() {
		return errors.Join(session.agentStopErr, auditErr)
	}
	// Only the private identity is removed; that is revocation. knownSource
	// holds nothing but the ephemeral host public key and is retained as the
	// evidence of what Hermes pinned on first use.
	cleanupErr := errors.Join(
		session.agentStopErr, session.revocationError(), auditErr,
		sshbridge.RemoveIfPresent(session.identitySource),
	)
	if session.partialStart && cleanupErr == nil {
		cleanupErr = os.RemoveAll(session.artifactDir)
	}
	return cleanupErr
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

func hasCancellationCause(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isPureCancellation(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !isPureCancellation(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return isPureCancellation(wrapped.Unwrap())
	}
	return err == context.Canceled || err == context.DeadlineExceeded
}

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}
