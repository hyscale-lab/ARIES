package openclawssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultClientPath     = "bin/aries-ssh"
	defaultBridgeCleanup  = 20 * time.Second
	maxRecordedInputBytes = 16 << 20
	maxToolLogBytes       = 256 << 20
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
	configuration  *ssh.ServerConfig
	cancel         context.CancelFunc
	clientSource   string
	identitySource string
	knownSource    string
	toolLogPath    string
	rawLogPath     string
	replyRequest   func(*ssh.Request, bool) error

	mu          sync.Mutex
	connections map[net.Conn]struct{}
	revokeOnce  sync.Once
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
	reader   io.Reader
	mu       sync.Mutex
	n        int64
	data     bytes.Buffer
	overflow bool
}

func (input *recordedInput) Read(content []byte) (int, error) {
	n, err := input.reader.Read(content)
	if n > 0 {
		input.mu.Lock()
		remaining := maxRecordedInputBytes - input.data.Len()
		if n > remaining {
			input.n += int64(n)
			input.data.Reset()
			input.overflow = true
			input.mu.Unlock()
			return n, fmt.Errorf("OpenClaw SSH stdin exceeds %d bytes", maxRecordedInputBytes)
		}
		_, _ = input.data.Write(content[:n])
		input.n += int64(n)
		input.mu.Unlock()
	}
	return n, err
}

func (input *recordedInput) record(retainedRaw bool) (int64, string, string, []byte, bool) {
	input.mu.Lock()
	count := input.n
	content := bytes.Clone(input.data.Bytes())
	overflow := input.overflow
	input.mu.Unlock()
	if bridgekit.SafeText(content) {
		return count, string(content), "utf-8", content, overflow
	}
	// Without the raw log the bytes are retained nowhere, so the note must not
	// point at an artifact this run did not write.
	note := fmt.Sprintf("[binary input omitted; %d bytes not retained]", count)
	if retainedRaw {
		note = fmt.Sprintf("[binary input omitted; %d bytes retained in ssh_raw.log]", count)
	}
	return count, note, "binary-omitted", content, overflow
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
			sandbox: sandbox, connections: make(map[net.Conn]struct{}),
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
	return session.Finish(ctx, session.clientSource, session.identitySource, session.knownSource)
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
	hostSigner, clientPEM, authorized, err := generateSessionKeys()
	if err != nil {
		return failed(err)
	}
	session.identitySource = filepath.Join(session.ArtifactDir, "id_ed25519")
	session.knownSource = filepath.Join(session.ArtifactDir, "known_hosts")
	session.toolLogPath = filepath.Join(session.ArtifactDir, "tool-calls.jsonl")
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.ArtifactDir, "ssh_raw.log")
	}
	if err := bridgekit.WritePrivate(session.identitySource, clientPEM); err != nil {
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
	if err := bridgekit.WritePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return failed(fmt.Errorf("write OpenClaw SSH known-hosts file: %w", err))
	}
	if session.Audit, err = bridgekit.Open(session.toolLogPath, session.rawLogPath, maxToolLogBytes); err != nil {
		return failed(fmt.Errorf("open OpenClaw SSH audit: %w", err))
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	configuration := newServerConfig(hostSigner, authorized)
	session.configuration = configuration
	session.Wait.Add(1)
	go session.serve(serveCtx, manager.logger)
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
	defer session.Wait.Done()
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
		session.Wait.Add(1)
		go session.handleConnection(ctx, connection)
	}
}

func (session *bridgeSession) handleConnection(ctx context.Context, connection net.Conn) {
	defer session.Wait.Done()
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
		session.Wait.Add(1)
		go func() {
			defer session.Wait.Done()
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
	stdin := &recordedInput{reader: channel}
	stdout := &byteCounter{writer: channel}
	stderr := &byteCounter{writer: channel.Stderr()}
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
	stdinBytes, stdinContent, stdinEncoding, rawStdin, stdinOverflow := stdin.record(session.rawLogPath != "")
	if stdinOverflow {
		session.Audit.Latch(fmt.Errorf("retain OpenClaw SSH stdin: input exceeds %d bytes", maxRecordedInputBytes))
		return exitCode
	}
	session.writeRecord(toolCallRecord{
		ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(),
		OperationClass: operationClass(command), Path: command.Path, Workdir: command.Dir, WorkspaceHome: prepared.workspaceHome,
		Environment: slices.Sorted(maps.Keys(command.Env)), CommandHash: bridgekit.CommandHash(prepared.encoded),
		Command: replayDisplayCommand(command), Argv: append([]string{command.Path}, command.Args...),
		Stdin: stdinContent, StdinEncoding: stdinEncoding,
		StdinBytes: stdinBytes, StdoutBytes: stdout.count(), StderrBytes: stderr.count(),
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

func rawRecord(audit requestAudit, stdinBytes int64, stdin []byte, status string) bridgekit.RawRecord {
	return bridgekit.RawRecord{
		RequestType: audit.requestType, WantReply: audit.wantReply,
		WireCommand: audit.remoteCommand, Payload: bytes.Clone(audit.payload), PayloadBytes: int64(len(audit.payload)),
		Stdin: bytes.Clone(stdin), StdinBytes: stdinBytes, Status: status,
	}
}

func (session *bridgeSession) writeRecord(record toolCallRecord, raw bridgekit.RawRecord) {
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	raw.RunID = session.sandbox.RunID()
	raw.TaskID = session.sandbox.TaskID()
	raw.ContainerID = session.sandbox.ContainerID()
	session.Audit.Enqueue(&record, &raw, 0)
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
