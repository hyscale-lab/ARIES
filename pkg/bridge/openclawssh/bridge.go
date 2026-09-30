package openclawssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/sshserve"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultClientPath    = "bin/aries-ssh"
	defaultBridgeCleanup = 20 * time.Second
	maxToolLogBytes      = 256 << 20
)

// Options are the host-local inputs to one OpenClaw SSH bridge.
type Options struct {
	OutputDir      string
	ClientPath     string
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
	clientPath     string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	afterStart     func(*bridgeSession) error
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
	clientSource   string
	identitySource string
	knownSource    string
	toolLogPath    string
	rawLogPath     string
	replyRequest   func(*ssh.Request, bool) error
}

type toolCallRecord struct {
	bridgekit.Stamp
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	WorkspaceHome  string   `json:"workspace_home,omitempty"`
	Environment    []string `json:"env_names,omitempty"`
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
		return nil, errors.New("OpenClaw SSH output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw SSH output directory: %w", err)
	}
	if err := bridgekit.EnsurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare OpenClaw SSH output directory: %w", err)
	}
	if options.ClientPath == "" {
		options.ClientPath = defaultClientPath
	}
	clientPath, err := filepath.Abs(options.ClientPath)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw SSH client helper: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		outputDir: outputDir, clientPath: clientPath,
		cleanupTimeout: options.CleanupTimeout, logger: options.Logger,
		omitRawLog: options.OmitRawLog,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	var endpoint core.ToolEndpoint
	err := manager.slot.Start(ctx, manager.cleanupTimeout, func() (bridgekit.Closer, error) {
		sandbox, ok := generic.(bridgeSandbox)
		if !ok {
			return nil, errors.New("OpenClaw SSH bridge requires the local Docker sandbox capability")
		}
		gateway, err := sandbox.NetworkGateway(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve task network gateway: %w", err)
		}
		session := &bridgeSession{
			sandbox:      sandbox,
			replyRequest: func(request *ssh.Request, accepted bool) error { return request.Reply(accepted, nil) },
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

// Close revokes the session and confirms it by removing the staged client,
// the identity, and the known-hosts file.
func (session *bridgeSession) Close(ctx context.Context) error {
	session.revoke()
	return session.Finalize(ctx, session.clientSource, session.identitySource, session.knownSource)
}

// open allocates the session's client, keys, listener, and audit. On error
// the caller closes the partial session.
func (manager *Manager) open(ctx context.Context, session *bridgeSession, gateway string) (core.ToolEndpoint, error) {
	failed := func(err error) (core.ToolEndpoint, error) { return core.ToolEndpoint{}, err }
	sandbox := session.sandbox
	if err := bridgekit.EnsurePrivateDirectory(session.ArtifactDir); err != nil {
		return failed(fmt.Errorf("create private OpenClaw SSH artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.ArtifactDir, "aries-ssh")
	if err := bridgekit.StageExecutable(manager.clientPath, session.clientSource); err != nil {
		return failed(fmt.Errorf("stage OpenClaw SSH client: %w", err))
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
		return failed(fmt.Errorf("write OpenClaw SSH identity: %w", err))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return failed(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return failed(fmt.Errorf("parse OpenClaw SSH listener address: %w", err))
	}
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := bridgekit.WriteExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return failed(fmt.Errorf("write OpenClaw SSH known-hosts file: %w", err))
	}
	if session.Audit, err = bridgekit.Open(session.toolLogPath, session.rawLogPath, maxToolLogBytes); err != nil {
		return failed(fmt.Errorf("open OpenClaw SSH audit: %w", err))
	}
	session.server = sshserve.Serve(listener, hostSigner, authorized, &session.Wait, manager.logger, "OpenClaw", session.handleSession)
	if manager.afterStart != nil {
		if err := manager.afterStart(session); err != nil {
			return failed(err)
		}
	}
	address := net.JoinHostPort(host, port)
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": network, "container": sandbox.ContainerName()}).Info("OpenClaw SSH bridge started")
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
		ClientCommand: clientContainerPath, ClientSourceFile: session.clientSource,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
		KnownHostsFile: knownHostsContainerPath, KnownHostsSourceFile: session.knownSource,
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
		if request.Type != "exec" || !request.WantReply {
			if request.WantReply {
				_ = session.reply(request, false)
			}
			session.logRejected(audit)
			return
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = session.reply(request, false)
			session.logRejected(audit)
			return
		}
		audit.remoteCommand = payload.Command
		command, err := decodeRemoteCommand(payload.Command)
		if err != nil {
			_ = session.reply(request, false)
			session.logRejected(audit)
			return
		}
		prepared, err := prepareRemoteCommand(command, session.sandbox.Workdir())
		if err != nil {
			_ = session.reply(request, false)
			session.logRejected(audit)
			return
		}
		if err := session.reply(request, true); err != nil {
			session.logRequestFailure(audit, "failed", "SSH accept reply failed")
			return
		}
		exitCode := session.execute(ctx, channel, prepared, audit)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
		return
	}
}

// reply routes through replyRequest, which Start always sets and tests
// override to fail an accepted reply.
func (session *bridgeSession) reply(request *ssh.Request, accepted bool) error {
	return session.replyRequest(request, accepted)
}

func (session *bridgeSession) execute(ctx context.Context, channel ssh.Channel, prepared preparedRemoteCommand, audit requestAudit) int {
	started := time.Now()
	command := prepared.command
	stdin := &sshserve.RecordedInput{Reader: channel}
	stdout := &sshserve.ByteCounter{Writer: channel}
	stderr := &sshserve.ByteCounter{Writer: channel.Stderr()}
	result := core.CommandResult{}
	var err error
	if !prepared.suppressed {
		result, err = session.sandbox.ExecStream(ctx, command, stdin, stdout, stderr)
	} else {
		_, err = io.Copy(io.Discard, stdin)
	}
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
		session.Audit.Latch(fmt.Errorf("retain OpenClaw SSH stdin: input exceeds %d bytes", sshserve.MaxRecordedInputBytes))
		return exitCode
	}
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: operationClass(command), Path: command.Path, Workdir: command.Dir, WorkspaceHome: prepared.workspaceHome,
		Environment: slices.Sorted(maps.Keys(command.Env)), CommandHash: bridgekit.CommandHash(prepared.encoded),
		Command: replayDisplayCommand(command), Argv: append([]string{command.Path}, command.Args...),
		Stdin: stdinContent, StdinEncoding: stdinEncoding,
		StdinBytes: stdinBytes, StdoutBytes: stdout.Count(), StderrBytes: stderr.Count(),
		ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message,
		RequestType: audit.requestType, WantReply: audit.wantReply,
	}, rawRecord(audit, stdinBytes, rawStdin, status))
	return exitCode
}

func replayDisplayCommand(command core.Command) string {
	if operationClass(command) != "exec" {
		return ""
	}
	return shellCommand(command)
}

func (session *bridgeSession) logRejected(audit requestAudit) {
	session.logRequestFailure(audit, "rejected", "invalid remote command")
}

func (session *bridgeSession) logRequestFailure(audit requestAudit, status, message string) {
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: "exec", CommandHash: bridgekit.CommandHash(audit.remoteCommand),
		StdinEncoding: "utf-8",
		Status:        status, Error: message,
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

func (remote remoteCommand) command(workdir string) core.Command {
	index := 0
	environment := make(map[string]string)
	if remote.argv[0] == remoteEnv {
		index++
		for remote.argv[index] != remoteShell {
			name, value, _ := strings.Cut(remote.argv[index], "=")
			environment[name] = value
			index++
		}
	}
	if len(environment) == 0 {
		environment = nil
	}
	return core.Command{Path: remote.argv[index], Args: append([]string(nil), remote.argv[index+1:]...), Dir: workdir, Env: environment}
}

func shellCommand(command core.Command) string {
	if command.Path == remoteShell && len(command.Args) >= 2 && command.Args[0] == "-c" {
		return command.Args[1]
	}
	return command.Path
}

func operationClass(command core.Command) string {
	if command.Path == remoteShell && matchesExactArgv(command.Args, "-c", directoryUploadScript, directoryUploadLabel, virtualSkillsWorkspace, virtualRuntimeRoot) {
		return "workspace_upload"
	}
	return "exec"
}
