package hermesssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/hermeswire"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/sshserve"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// syncDenied is the recorded reason for a refused Hermes file sync.
const syncDenied = "Hermes SSH file sync is denied by ARIES policy"

const (
	defaultBridgeCleanup = 20 * time.Second
	maxToolLogBytes      = 256 << 20

	identityContainerPath = "/run/aries/ssh/id_ed25519"
)

// Options are the host-local inputs to one Hermes SSH bridge.
type Options struct {
	OutputDir      string
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
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	omitRawLog     bool

	slot bridgekit.Slot
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
	runner.StreamExecutor
}

type bridgeSession struct {
	bridgekit.Session
	sandbox        bridgeSandbox
	listener       net.Listener
	server         *sshserve.Server
	identitySource string
	knownSource    string
	toolLogPath    string
	rawLogPath     string
}

type toolCallRecord struct {
	bridgekit.Stamp
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	CommandHash    string   `json:"command_hash"`
	Command        string   `json:"command,omitempty"`
	Argv           []string `json:"argv,omitempty"`
	Stdin          string   `json:"stdin"`
	StdinEncoding  string   `json:"stdin_encoding"`
	StdinBytes     int64    `json:"stdin_bytes"`
	StdoutBytes    int64    `json:"stdout_bytes"`
	StderrBytes    int64    `json:"stderr_bytes"`
	ExitCode       int      `json:"exit_code"`
	DurationMS     int64    `json:"duration_ms"`
	Status         string   `json:"status"`
	Error          string   `json:"error,omitempty"`
	RunID          string   `json:"run_id,omitempty"`
	TaskID         string   `json:"task_id,omitempty"`
	RequestType    string   `json:"request_type"`
	WantReply      bool     `json:"want_reply"`
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
	if err := bridgekit.EnsurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Hermes SSH output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		outputDir: outputDir, cleanupTimeout: options.CleanupTimeout,
		logger: options.Logger, omitRawLog: options.OmitRawLog,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	var endpoint core.ToolEndpoint
	err := manager.slot.Start(ctx, manager.cleanupTimeout, func() (bridgekit.Closer, error) {
		sandbox, ok := generic.(bridgeSandbox)
		if !ok {
			return nil, errors.New("Hermes SSH bridge requires the local Docker sandbox capability")
		}
		gateway, err := sandbox.NetworkGateway(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve task network gateway: %w", err)
		}
		session := &bridgeSession{sandbox: sandbox}
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

// Close revokes the session and confirms it. Only the private identity is
// removed; that is revocation. knownSource holds nothing but the ephemeral
// host public key and is retained as the evidence of what Hermes pinned on
// first use.
func (session *bridgeSession) Close(ctx context.Context) error {
	session.revoke()
	return session.Finalize(ctx, session.identitySource)
}

// open allocates the session's files, keys, listener, and audit. On error the
// caller closes the partial session.
func (manager *Manager) open(ctx context.Context, session *bridgeSession, gateway string) (core.ToolEndpoint, error) {
	failed := func(err error) (core.ToolEndpoint, error) { return core.ToolEndpoint{}, err }
	sandbox := session.sandbox
	if err := bridgekit.EnsurePrivateDirectory(session.ArtifactDir); err != nil {
		return failed(fmt.Errorf("create private Hermes SSH artifact directory: %w", err))
	}
	hostSigner, clientPEM, authorized, err := sshserve.GenerateSessionKeys()
	if err != nil {
		return failed(err)
	}
	session.identitySource = filepath.Join(session.ArtifactDir, "id_ed25519")
	session.knownSource = filepath.Join(session.ArtifactDir, "known_hosts")
	session.toolLogPath = filepath.Join(session.ArtifactDir, "tool-calls.jsonl")
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.ArtifactDir, "ssh_raw.log")
	}
	if err := bridgekit.WriteExclusivePrivate(session.identitySource, clientPEM); err != nil {
		return failed(fmt.Errorf("write Hermes SSH identity: %w", err))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return failed(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return failed(fmt.Errorf("parse Hermes SSH listener address: %w", err))
	}
	// Retained as evidence of the host key Hermes pins on first use. Hermes
	// forces StrictHostKeyChecking=accept-new and offers no way to preload a
	// known-hosts file, so this is not handed to the harness.
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := bridgekit.WriteExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return failed(fmt.Errorf("write Hermes SSH known-hosts file: %w", err))
	}
	if session.Audit, err = bridgekit.Open(session.toolLogPath, session.rawLogPath, maxToolLogBytes); err != nil {
		return failed(fmt.Errorf("open Hermes SSH audit: %w", err))
	}
	session.server = sshserve.Serve(listener, hostSigner, authorized, &session.Wait, manager.logger, "Hermes", session.handleSession)
	address := net.JoinHostPort(host, port)
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": network, "container": sandbox.ContainerName()}).Info("Hermes SSH bridge started")
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: sshserve.LockedUsername, Network: network,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
		LogPaths: session.logPaths(), Workdir: sandbox.Workdir(),
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
				_ = request.Reply(false, nil)
			}
			session.logRequestFailure(audit, hermeswire.KindUnknown, "unsupported", "channel request type is not exec")
			continue
		}
		if !request.WantReply {
			session.logRejected(audit, hermeswire.KindUnknown)
			return
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			session.logRejected(audit, hermeswire.KindUnknown)
			return
		}
		audit.remoteCommand = payload.Command
		command, kind, err := hermeswire.Prepare(payload.Command, session.sandbox.Workdir())
		if err != nil {
			_ = request.Reply(false, nil)
			if errors.Is(err, hermeswire.ErrSyncDenied) {
				session.logRequestFailure(audit, kind, "denied", syncDenied)
			} else {
				session.logRejected(audit, kind)
			}
			return
		}
		if err := request.Reply(true, nil); err != nil {
			session.logRequestFailure(audit, kind, "failed", "SSH accept reply failed")
			return
		}
		exitCode := session.execute(ctx, channel, command, kind, audit)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
		return
	}
}

func (session *bridgeSession) execute(ctx context.Context, channel ssh.Channel, command core.Command, kind string, audit requestAudit) int {
	started := time.Now()
	stdin := &sshserve.RecordedInput{Reader: channel}
	stdout := &sshserve.ByteCounter{Writer: channel}
	stderr := &sshserve.ByteCounter{Writer: channel.Stderr()}
	result, err := session.sandbox.ExecStream(ctx, command, stdin, stdout, stderr)
	err = bridgekit.WithCancellation(ctx, err)
	exitCode := result.ExitCode
	status, message := "completed", ""
	if err != nil {
		session.RecordRevocationError(err)
		exitCode = 255
		status, message = "failed", "sandbox execution failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status, message = "canceled", "session canceled"
		}
	}
	if exitCode < 0 || exitCode > 255 {
		exitCode = 255
	}
	stdinBytes, stdinContent, stdinEncoding, rawStdin, stdinOverflow := stdin.Record(session.rawLogPath != "")
	if stdinOverflow {
		session.Audit.Latch(fmt.Errorf("retain Hermes SSH stdin: input exceeds %d bytes", sshserve.MaxRecordedInputBytes))
		return exitCode
	}
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: kind, Path: command.Path, Workdir: command.Dir,
		// Decoding accepts only the canonical encoding, so the wire payload is
		// already the replayable form.
		CommandHash: bridgekit.CommandHash(audit.remoteCommand),
		Command:     audit.remoteCommand,
		Argv:        append([]string{command.Path}, command.Args...),
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
// is not filed as an agent command; KindUnknown marks a payload that never
// decoded far enough to classify.
func (session *bridgeSession) logRequestFailure(audit requestAudit, kind, status, message string) {
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: kind, CommandHash: bridgekit.CommandHash(audit.remoteCommand),
		StdinEncoding: "utf-8",
		// The request never ran, so the record must not carry the exit code of
		// a successful command.
		ExitCode: -1,
		Status:   status, Error: message,
		RequestType: audit.requestType, WantReply: audit.wantReply,
	}, rawRecord(audit, 0, nil, status))
}

func rawRecord(audit requestAudit, stdinBytes int64, stdin []byte, status string) bridgekit.RawSSHRecord {
	return bridgekit.RawSSHRecord{
		RequestType: audit.requestType, WantReply: audit.wantReply,
		WireCommand: audit.remoteCommand, Payload: bytes.Clone(audit.payload), PayloadBytes: int64(len(audit.payload)),
		Stdin: bytes.Clone(stdin), StdinBytes: stdinBytes, Status: status,
	}
}

func (session *bridgeSession) writeRecord(record toolCallRecord, raw bridgekit.RawSSHRecord) {
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	raw.RunID = session.sandbox.RunID()
	raw.TaskID = session.sandbox.TaskID()
	raw.ContainerID = session.sandbox.ContainerID()
	session.Audit.Enqueue(&record, &raw, 0)
}

func (session *bridgeSession) revoke() {
	if session.server != nil {
		session.server.Revoke()
	} else if session.listener != nil {
		_ = session.listener.Close()
	}
}
