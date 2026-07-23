package openclawssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultClientPath      = "bin/aries-ssh"
	defaultBridgeCleanup   = 20 * time.Second
	maxRecordedInputBytes  = 16 << 20
	maxToolLogBytes        = 256 << 20
	openClawWorkspace      = "/aries/openclaw/openclaw-ssh-shared-8198076c/workspace"
	workspacePrepareScript = `set -eu
target=$1
workdir=$2
owner=$3
if [ "$target" = "$workdir" ]; then
  exit 0
elif [ -L "$target" ]; then
  [ "$(readlink "$target")" = "$workdir" ]
elif [ -e "$target" ]; then
  exit 73
else
  mkdir -p "$(dirname "$target")"
  (set -C; : > "$owner")
  ln -s "$workdir" "$target"
fi`
	workspaceRollbackScript = `set -eu
target=$1
workdir=$2
owner=$3
if [ -f "$owner" ] && [ -L "$target" ] && [ "$(readlink "$target")" = "$workdir" ]; then
  rm "$target"
fi
rm -f "$owner"`
	workspaceReleaseScript = `set -eu
rm -f "$1"`
)

// Options are the host-local inputs to one OpenClaw SSH bridge.
type Options struct {
	OutputDir      string
	ClientPath     string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Manager exposes one SSH endpoint at a time and proxies its exec requests to
// the exact Docker sandbox passed to Start.
type Manager struct {
	outputDir      string
	clientPath     string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	newID          func() (string, error)

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
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

type bridgeSession struct {
	sandbox        bridgeSandbox
	listener       net.Listener
	configuration  *ssh.ServerConfig
	cancel         context.CancelFunc
	artifactDir    string
	clientSource   string
	identitySource string
	knownSource    string
	toolLogPath    string
	toolLog        *os.File
	workspaceOwner string
	logger         *logrus.Logger

	mu            sync.Mutex
	connections   map[net.Conn]struct{}
	sequence      uint64
	logMu         sync.Mutex
	logErr        error
	logBytes      int64
	closeLogErr   error
	revocationMu  sync.Mutex
	revocationErr error
	wait          sync.WaitGroup
	revokeOnce    sync.Once
	closeLogOnce  sync.Once
}

type toolCallRecord struct {
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
	ContainerID    string   `json:"container_id"`
	ContainerName  string   `json:"container_name"`
	OperationClass string   `json:"operation_class"`
	Path           string   `json:"path,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
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
}

type byteCounter struct {
	reader io.Reader
	writer io.Writer
	n      atomic.Int64
}

func (counter *byteCounter) Read(content []byte) (int, error) {
	n, err := counter.reader.Read(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *byteCounter) Write(content []byte) (int, error) {
	n, err := counter.writer.Write(content)
	counter.n.Add(int64(n))
	return n, err
}

func (counter *byteCounter) count() int64 { return counter.n.Load() }

type recordedInput struct {
	reader io.Reader
	mu     sync.Mutex
	n      int64
	data   bytes.Buffer
}

func (input *recordedInput) Read(content []byte) (int, error) {
	n, err := input.reader.Read(content)
	if n > 0 {
		input.mu.Lock()
		remaining := maxRecordedInputBytes - input.data.Len()
		accepted := min(n, max(remaining, 0))
		_, _ = input.data.Write(content[:accepted])
		input.n += int64(accepted)
		input.mu.Unlock()
		if accepted != n {
			return accepted, fmt.Errorf("OpenClaw SSH stdin exceeds %d bytes", maxRecordedInputBytes)
		}
	}
	return n, err
}

func (input *recordedInput) record() (int64, string, string) {
	input.mu.Lock()
	count := input.n
	content := bytes.Clone(input.data.Bytes())
	input.mu.Unlock()
	if utf8.Valid(content) {
		return count, string(content), "utf-8"
	}
	return count, base64.StdEncoding.EncodeToString(content), "base64"
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
	if err := ensurePrivateDirectory(outputDir); err != nil {
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
		newID: randomBridgeID,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("OpenClaw SSH bridge is already active")
	}
	sandbox, ok := generic.(bridgeSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("OpenClaw SSH bridge requires the local Docker sandbox capability")
	}
	gateway, err := sandbox.NetworkGateway(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve task network gateway: %w", err)
	}
	id, err := manager.newID()
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("generate OpenClaw SSH bridge ID: %w", err)
	}
	session := &bridgeSession{sandbox: sandbox, connections: make(map[net.Conn]struct{}), logger: manager.logger}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")
	session.workspaceOwner = openClawWorkspace + ".aries-owner-" + id
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		waitErr := session.waitFor(cleanupCtx)
		workspaceErr := rollbackWorkspace(cleanupCtx, session)
		logErr := session.closeLog()
		cleanupErr := os.RemoveAll(session.artifactDir)
		return core.ToolEndpoint{}, errors.Join(primary, waitErr, workspaceErr, logErr, cleanupErr)
	}
	if err := prepareWorkspace(ctx, session); err != nil {
		return fail(err)
	}
	if err := ensurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private OpenClaw SSH artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.artifactDir, "aries-ssh")
	if err := stageExecutable(manager.clientPath, session.clientSource); err != nil {
		return fail(fmt.Errorf("stage OpenClaw SSH client: %w", err))
	}
	hostSigner, clientPEM, authorized, err := generateSessionKeys()
	if err != nil {
		return fail(err)
	}
	session.identitySource = filepath.Join(session.artifactDir, "id_ed25519")
	session.knownSource = filepath.Join(session.artifactDir, "known_hosts")
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	if err := writeExclusivePrivate(session.identitySource, clientPEM); err != nil {
		return fail(fmt.Errorf("write OpenClaw SSH identity: %w", err))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return fail(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fail(fmt.Errorf("parse OpenClaw SSH listener address: %w", err))
	}
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := writeExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return fail(fmt.Errorf("write OpenClaw SSH known-hosts file: %w", err))
	}
	session.toolLog, err = os.OpenFile(session.toolLogPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fail(fmt.Errorf("create OpenClaw SSH tool log: %w", err))
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	configuration := newServerConfig(hostSigner, authorized)
	session.configuration = configuration
	session.wait.Add(1)
	go session.serve(serveCtx, manager.logger)
	if err := releaseWorkspaceOwner(ctx, session); err != nil {
		return fail(err)
	}

	manager.active = session
	manager.stopErr = nil
	address := net.JoinHostPort(host, port)
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": network, "container": sandbox.ContainerName()}).Info("OpenClaw SSH bridge started")
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
		ClientCommand: clientContainerPath, ClientSourceFile: session.clientSource,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
		KnownHostsFile: knownHostsContainerPath, KnownHostsSourceFile: session.knownSource,
		LogPaths: []string{session.toolLogPath},
	}, nil
}

func newServerConfig(hostSigner ssh.Signer, authorized ssh.PublicKey) *ssh.ServerConfig {
	configuration := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != lockedUsername || !bytes.Equal(key.Marshal(), authorized.Marshal()) {
				return nil, errors.New("public key rejected")
			}
			return &ssh.Permissions{}, nil
		},
	}
	configuration.AddHostKey(hostSigner)
	return configuration
}

func (session *bridgeSession) serve(ctx context.Context, logger *logrus.Logger) {
	defer session.wait.Done()
	for {
		connection, err := session.listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				logger.WithError(err).Warn("OpenClaw SSH accept failed")
			}
			return
		}
		session.mu.Lock()
		session.connections[connection] = struct{}{}
		session.mu.Unlock()
		session.wait.Add(1)
		go session.handleConnection(ctx, connection)
	}
}

func (session *bridgeSession) handleConnection(ctx context.Context, connection net.Conn) {
	defer session.wait.Done()
	defer func() {
		_ = connection.Close()
		session.mu.Lock()
		delete(session.connections, connection)
		session.mu.Unlock()
	}()
	_ = connection.SetDeadline(time.Now().Add(lockedConnectTimeout))
	server, channels, requests, err := ssh.NewServerConn(connection, session.configuration)
	if err != nil {
		return
	}
	defer server.Close()
	_ = connection.SetDeadline(time.Time{})
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = server.Wait()
		cancel()
	}()
	go serveGlobalRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" || len(incoming.ExtraData()) != 0 {
			_ = incoming.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			continue
		}
		session.wait.Add(1)
		go func() {
			defer session.wait.Done()
			defer channel.Close()
			session.handleSession(connectionCtx, channel, channelRequests)
		}()
	}
}

func serveGlobalRequests(requests <-chan *ssh.Request) {
	for request := range requests {
		accepted := request.Type == "keepalive@openssh.com" && len(request.Payload) == 0
		if request.WantReply {
			_ = request.Reply(accepted, nil)
		}
	}
}

func (session *bridgeSession) handleSession(ctx context.Context, channel ssh.Channel, requests <-chan *ssh.Request) {
	for request := range requests {
		if request.Type != "exec" || !request.WantReply {
			if request.WantReply {
				_ = request.Reply(false, nil)
			}
			session.logRejected(request.Payload)
			return
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
			_ = request.Reply(false, nil)
			session.logRejected(request.Payload)
			return
		}
		command, err := decodeRemoteCommand(payload.Command)
		if err != nil {
			_ = request.Reply(false, nil)
			session.logRejected([]byte(payload.Command))
			return
		}
		if err := request.Reply(true, nil); err != nil {
			return
		}
		exitCode := session.execute(ctx, channel, payload.Command, command)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitCode)}))
		return
	}
}

func (session *bridgeSession) execute(ctx context.Context, channel ssh.Channel, encoded string, remote remoteCommand) int {
	started := time.Now()
	command := remote.command(session.sandbox.Workdir())
	stdin := &recordedInput{reader: channel}
	stdout := &byteCounter{writer: channel}
	stderr := &byteCounter{writer: channel.Stderr()}
	result, err := session.sandbox.ExecStream(ctx, command, stdin, stdout, stderr)
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
	stdinBytes, stdinContent, stdinEncoding := stdin.record()
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: operationClass(command), Path: command.Path, Workdir: command.Dir,
		Environment: slices.Sorted(maps.Keys(command.Env)), CommandHash: commandHash(encoded),
		Command: replayDisplayCommand(command), Argv: append([]string{command.Path}, command.Args...),
		Stdin: stdinContent, StdinEncoding: stdinEncoding,
		StdinBytes: stdinBytes, StdoutBytes: stdout.count(), StderrBytes: stderr.count(),
		ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message,
	})
	return exitCode
}

func replayDisplayCommand(command core.Command) string {
	if operationClass(command) != "exec" {
		return ""
	}
	return shellCommand(command)
}

func (session *bridgeSession) logRejected(payload []byte) {
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: "exec", CommandHash: commandHash(string(payload)),
		StdinEncoding: "utf-8",
		Status:        "rejected", Error: "invalid remote command",
	})
}

func (session *bridgeSession) writeRecord(record toolCallRecord) {
	session.logMu.Lock()
	defer session.logMu.Unlock()
	if session.logErr != nil {
		return
	}
	session.sequence++
	record.Sequence = session.sequence
	record.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	content, err := json.Marshal(record)
	if err == nil {
		content = append(content, '\n')
		if int64(len(content)) > maxToolLogBytes-session.logBytes {
			err = fmt.Errorf("OpenClaw SSH tool log exceeds %d bytes", maxToolLogBytes)
		} else {
			var written int
			written, err = session.toolLog.Write(content)
			session.logBytes += int64(written)
			if err == nil && written != len(content) {
				err = io.ErrShortWrite
			}
		}
	}
	if err == nil {
		err = session.toolLog.Sync()
	}
	if err != nil {
		session.logErr = errors.Join(session.logErr, err)
		session.logger.WithError(err).Error("write OpenClaw SSH tool log")
	}
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
		err = errors.Join(
			session.revocationError(),
			session.closeLog(),
			removeIfPresent(session.clientSource),
			removeIfPresent(session.identitySource),
			removeIfPresent(session.knownSource),
		)
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

func (session *bridgeSession) revoke() {
	session.revokeOnce.Do(func() {
		if session.cancel != nil {
			session.cancel()
		}
		if session.listener != nil {
			_ = session.listener.Close()
		}
		session.mu.Lock()
		for connection := range session.connections {
			_ = connection.Close()
		}
		session.mu.Unlock()
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

func (session *bridgeSession) closeLog() error {
	session.closeLogOnce.Do(func() {
		session.logMu.Lock()
		defer session.logMu.Unlock()
		if session.toolLog != nil {
			session.closeLogErr = session.toolLog.Close()
		}
	})
	return errors.Join(session.logErr, session.closeLogErr)
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

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}

func shellCommand(command core.Command) string {
	if command.Path == remoteShell && len(command.Args) >= 2 && command.Args[0] == "-c" {
		return command.Args[1]
	}
	return command.Path
}

func operationClass(command core.Command) string {
	if command.Path != remoteShell || len(command.Args) < 3 || command.Args[0] != "-c" {
		return "exec"
	}
	switch command.Args[2] {
	case "openclaw-sandbox-upload":
		return "workspace_upload"
	default:
		return "exec"
	}
}

func prepareWorkspace(ctx context.Context, session *bridgeSession) error {
	result, err := session.sandbox.Exec(ctx, workspaceCommand(
		workspacePrepareScript, session.sandbox.Workdir(), session.workspaceOwner,
	))
	if err != nil || result.ExitCode != 0 {
		return commandFailure("prepare OpenClaw workspace alias", result, err)
	}
	return nil
}

func rollbackWorkspace(ctx context.Context, session *bridgeSession) error {
	if session == nil || session.workspaceOwner == "" {
		return nil
	}
	result, err := session.sandbox.Exec(ctx, workspaceCommand(
		workspaceRollbackScript, session.sandbox.Workdir(), session.workspaceOwner,
	))
	if err != nil || result.ExitCode != 0 {
		return commandFailure("roll back OpenClaw workspace alias", result, err)
	}
	return nil
}

func releaseWorkspaceOwner(ctx context.Context, session *bridgeSession) error {
	result, err := session.sandbox.Exec(ctx, core.Command{
		Path: remoteShell,
		Args: []string{"-c", workspaceReleaseScript, "aries-workspace", session.workspaceOwner},
		Dir:  session.sandbox.Workdir(),
	})
	if err != nil || result.ExitCode != 0 {
		return commandFailure("release OpenClaw workspace ownership marker", result, err)
	}
	return nil
}

func workspaceCommand(script, workdir, owner string) core.Command {
	return core.Command{
		Path: remoteShell,
		Args: []string{"-c", script, "aries-workspace", openClawWorkspace, workdir, owner},
		Dir:  workdir,
	}
}

func commandFailure(operation string, result core.CommandResult, err error) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	return fmt.Errorf("%s: exit %d: %s", operation, result.ExitCode, strings.TrimSpace(result.Stderr))
}

func generateSessionKeys() (ssh.Signer, []byte, ssh.PublicKey, error) {
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create SSH host signer: %w", err)
	}
	_, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate SSH client key: %w", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create SSH client signer: %w", err)
	}
	clientPEM, err := marshalEd25519PrivateKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal SSH client key: %w", err)
	}
	return hostSigner, clientPEM, clientSigner.PublicKey(), nil
}

func marshalEd25519PrivateKey(private ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func stageExecutable(source, destination string) error {
	before, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return errors.New("helper source must be a regular executable")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != int64(len(content)) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("helper source changed while being staged")
	}
	return writeExclusive(destination, content, 0o555)
}

func writeExclusivePrivate(path string, content []byte) error {
	return writeExclusive(path, content, 0o600)
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("directory path contains a symbolic link")
	}
	return os.Chmod(path, 0o700)
}

func randomBridgeID() (string, error) {
	var content [8]byte
	if _, err := rand.Read(content[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(content[:]), nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
