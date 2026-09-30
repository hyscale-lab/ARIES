package hermesssh

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
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/hermeswire"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

// syncDenied is the recorded reason for a refused Hermes file sync.
const syncDenied = "Hermes SSH file sync is denied by ARIES policy"

const (
	defaultBridgeCleanup  = 20 * time.Second
	maxRecordedInputBytes = 16 << 20
	maxToolLogBytes       = 256 << 20

	identityContainerPath = "/run/aries/ssh/id_ed25519"
	lockedUsername        = "aries"
	lockedConnectTimeout  = 5 * time.Second
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
	configuration  *ssh.ServerConfig
	cancel         context.CancelFunc
	identitySource string
	knownSource    string
	toolLogPath    string
	rawLogPath     string

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
			return n, fmt.Errorf("Hermes SSH stdin exceeds %d bytes", maxRecordedInputBytes)
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
		session := &bridgeSession{sandbox: sandbox, connections: make(map[net.Conn]struct{})}
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
	return session.Finish(ctx, session.identitySource)
}

// open allocates the session's files, keys, listener, and audit. On error the
// caller closes the partial session.
func (manager *Manager) open(ctx context.Context, session *bridgeSession, gateway string) (core.ToolEndpoint, error) {
	failed := func(err error) (core.ToolEndpoint, error) { return core.ToolEndpoint{}, err }
	sandbox := session.sandbox
	if err := bridgekit.EnsurePrivateDirectory(session.ArtifactDir); err != nil {
		return failed(fmt.Errorf("create private Hermes SSH artifact directory: %w", err))
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
	if err := bridgekit.WritePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return failed(fmt.Errorf("write Hermes SSH known-hosts file: %w", err))
	}
	if session.Audit, err = bridgekit.Open(session.toolLogPath, session.rawLogPath, maxToolLogBytes); err != nil {
		return failed(fmt.Errorf("open Hermes SSH audit: %w", err))
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.configuration = newServerConfig(hostSigner, authorized)
	session.Wait.Add(1)
	go session.serve(serveCtx, manager.logger)
	address := net.JoinHostPort(host, port)
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": network, "container": sandbox.ContainerName()}).Info("Hermes SSH bridge started")
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
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
				logger.WithError(err).Warn("Hermes SSH accept failed")
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
	// Hermes holds one ControlMaster connection open for the whole run and
	// multiplexes every later command onto it, so the handshake deadline must
	// not survive into the session channels.
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
	stdin := &recordedInput{reader: channel}
	stdout := &byteCounter{writer: channel}
	stderr := &byteCounter{writer: channel.Stderr()}
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
	stdinBytes, stdinContent, stdinEncoding, rawStdin, stdinOverflow := stdin.record(session.rawLogPath != "")
	if stdinOverflow {
		session.Audit.Latch(fmt.Errorf("retain Hermes SSH stdin: input exceeds %d bytes", maxRecordedInputBytes))
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
		StdinBytes: stdinBytes, StdoutBytes: stdout.count(), StderrBytes: stderr.count(),
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
