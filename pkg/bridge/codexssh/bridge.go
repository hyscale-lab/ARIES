package codexssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	defaultBridgeCleanup  = 20 * time.Second
	maxRecordedInputBytes = 16 << 20
	maxToolLogBytes       = 256 << 20

	identityContainerPath = "/run/aries/ssh/id_ed25519"
	lockedUsername        = "aries"
	lockedConnectTimeout  = 5 * time.Second
)

// Options are the host-local inputs to one Codex SSH bridge.
type Options struct {
	ClientPath     string
	CodexPath      string
	SupervisorPath string
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
	clientPath     string
	codexPath      string
	supervisorPath string
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	openAudit      func(string) (*auditFile, error)
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
	TaskUser(context.Context) (string, error)
	ExecSupervisedStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

type bridgeSession struct {
	clientSource      string
	stageDir          string
	stageOwned        bool
	supervisorStaged  bool
	executorAttempted bool
	taskUser          string
	claimed           bool
	revoked           bool
	sandbox           bridgeSandbox
	listener          net.Listener
	configuration     *ssh.ServerConfig
	cancel            context.CancelFunc
	artifactDir       string
	identitySource    string
	knownSource       string
	toolLogPath       string
	rawLogPath        string
	audit             *auditWriter
	partialStart      bool
	replyRequest      func(*ssh.Request, bool) error

	mu            sync.Mutex
	connections   map[net.Conn]struct{}
	revocationMu  sync.Mutex
	revocationErr error
	wait          sync.WaitGroup
	revokeOnce    sync.Once
}

type toolCallRecord struct {
	Sequence       uint64   `json:"sequence"`
	Timestamp      string   `json:"timestamp"`
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

type rawSSHRecord struct {
	Sequence     uint64
	Timestamp    string
	RequestType  string
	WantReply    bool
	Status       string
	RunID        string
	TaskID       string
	ContainerID  string
	WireCommand  string
	Payload      []byte
	PayloadBytes int64
	Stdin        []byte
	StdinBytes   int64
}

type requestAudit struct {
	requestType   string
	wantReply     bool
	payload       []byte
	remoteCommand string
}

type auditFile struct {
	write func([]byte) (int, error)
	sync  func() error
	close func() error
}

type auditEntry struct {
	structured []byte
	raw        []byte
}

type auditWriter struct {
	structured *auditFile
	raw        *auditFile

	mu        sync.Mutex
	pending   []auditEntry
	sequence  uint64
	bytes     int64
	sealed    bool
	err       error
	wake      chan struct{}
	done      chan struct{}
	marshal   func(any) ([]byte, error)
	renderRaw func(rawSSHRecord) ([]byte, error)
	now       func() time.Time
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
			return n, fmt.Errorf("Codex SSH stdin exceeds %d bytes", maxRecordedInputBytes)
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
	if safeStructuredText(content) {
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

func safeStructuredText(content []byte) bool {
	if !utf8.Valid(content) {
		return false
	}
	for _, value := range string(content) {
		if unicode.IsControl(value) && value != '\t' && value != '\n' && value != '\r' {
			return false
		}
	}
	return true
}

func openAuditFile(path string) (*auditFile, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	return &auditFile{write: file.Write, sync: file.Sync, close: file.Close}, nil
}

func newAuditWriter(structured, raw *auditFile) *auditWriter {
	writer := &auditWriter{
		structured: structured, raw: raw,
		wake: make(chan struct{}, 1), done: make(chan struct{}),
		marshal: marshalJSONLine, renderRaw: renderRawSSHRecord, now: time.Now,
	}
	go writer.run()
	return writer
}

// retainsRaw reports whether this run writes ssh_raw.log, so callers can
// describe where omitted bytes were kept without guessing.
func (writer *auditWriter) retainsRaw() bool {
	return writer != nil && writer.raw != nil
}

func (writer *auditWriter) enqueue(structured toolCallRecord, raw rawSSHRecord) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.err != nil {
		return
	}
	if writer.sealed {
		writer.latchLocked(errors.New("enqueue Codex SSH audit after seal"))
		return
	}
	sequence := writer.sequence + 1
	timestamp := writer.now().UTC().Format(time.RFC3339Nano)
	structured.Sequence, structured.Timestamp = sequence, timestamp
	raw.Sequence, raw.Timestamp = sequence, timestamp
	structuredLine, err := writer.marshal(structured)
	if err != nil {
		writer.latchLocked(fmt.Errorf("marshal structured SSH audit: %w", err))
		return
	}
	var rawLine []byte
	if writer.raw != nil {
		rawLine, err = writer.renderRaw(raw)
		if err != nil {
			writer.latchLocked(fmt.Errorf("render raw SSH audit: %w", err))
			return
		}
	}
	charge := int64(len(structuredLine) + len(rawLine))
	if charge > maxToolLogBytes-writer.bytes {
		writer.latchLocked(fmt.Errorf("Codex SSH combined audit exceeds %d bytes", maxToolLogBytes))
		return
	}
	writer.sequence = sequence
	writer.bytes += charge
	writer.pending = append(writer.pending, auditEntry{structured: structuredLine, raw: rawLine})
	writer.signal()
}

func marshalJSONLine(value any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func renderRawSSHRecord(record rawSSHRecord) ([]byte, error) {
	var output bytes.Buffer
	output.WriteString("--- ARIES SSH CALL BEGIN ---\n")
	writeRawField(&output, "sequence", fmt.Sprint(record.Sequence))
	writeRawField(&output, "timestamp", record.Timestamp)
	writeRawField(&output, "request_type", record.RequestType)
	writeRawField(&output, "want_reply", fmt.Sprint(record.WantReply))
	writeRawField(&output, "status", record.Status)
	writeRawField(&output, "run_id", record.RunID)
	writeRawField(&output, "task_id", record.TaskID)
	writeRawField(&output, "container_id", record.ContainerID)
	writeRawField(&output, "wire_command", record.WireCommand)
	writeRawField(&output, "payload_bytes", fmt.Sprint(record.PayloadBytes))
	writeRawBytesField(&output, "payload", record.Payload)
	writeRawField(&output, "stdin_bytes", fmt.Sprint(record.StdinBytes))
	writeRawBytesField(&output, "stdin", record.Stdin)
	output.WriteString("--- ARIES SSH CALL END ---\n")
	return output.Bytes(), nil
}

func writeRawField(output *bytes.Buffer, key, value string) {
	writeRawBytesField(output, key, []byte(value))
}

func writeRawBytesField(output *bytes.Buffer, key string, value []byte) {
	output.WriteString(key)
	output.WriteByte('=')
	writeEscapedRaw(output, value)
	output.WriteByte('\n')
}

func writeEscapedRaw(output *bytes.Buffer, value []byte) {
	for len(value) > 0 {
		switch value[0] {
		case '\\':
			output.WriteString(`\\`)
			value = value[1:]
			continue
		case '\n':
			output.WriteString(`\n`)
			value = value[1:]
			continue
		case '\r':
			output.WriteString(`\r`)
			value = value[1:]
			continue
		case '\t':
			output.WriteString(`\t`)
			value = value[1:]
			continue
		}
		runeValue, size := utf8.DecodeRune(value)
		if runeValue != utf8.RuneError || size > 1 {
			if unicode.IsPrint(runeValue) {
				output.Write(value[:size])
			} else {
				writeHexEscapes(output, value[:size])
			}
			value = value[size:]
			continue
		}
		writeHexEscapes(output, value[:1])
		value = value[1:]
	}
}

func writeHexEscapes(output *bytes.Buffer, value []byte) {
	const uppercaseHex = "0123456789ABCDEF"
	for _, item := range value {
		output.WriteString(`\x`)
		output.WriteByte(uppercaseHex[item>>4])
		output.WriteByte(uppercaseHex[item&0x0f])
	}
}

func (writer *auditWriter) signal() {
	select {
	case writer.wake <- struct{}{}:
	default:
	}
}

func (writer *auditWriter) latchLocked(err error) {
	writer.err = errors.Join(writer.err, err)
}

func (writer *auditWriter) latch(err error) {
	writer.mu.Lock()
	writer.latchLocked(err)
	writer.mu.Unlock()
}

func (writer *auditWriter) run() {
	defer close(writer.done)
	for {
		<-writer.wake
		for {
			writer.mu.Lock()
			if len(writer.pending) == 0 {
				sealed := writer.sealed
				writer.mu.Unlock()
				if sealed {
					writer.finish()
					return
				}
				break
			}
			entry := writer.pending[0]
			writer.pending[0] = auditEntry{}
			writer.pending = writer.pending[1:]
			writer.mu.Unlock()
			writer.persist(entry)
		}
	}
}

func (writer *auditWriter) persist(entry auditEntry) {
	writer.persistLine(writer.structured, entry.structured, "structured write")
	writer.persistLine(writer.raw, entry.raw, "raw write")
	writer.persistSync(writer.structured, "structured sync")
	writer.persistSync(writer.raw, "raw sync")
}

func (writer *auditWriter) persistLine(file *auditFile, line []byte, operation string) {
	if file == nil {
		return
	}
	written, err := file.write(line)
	if err == nil && written != len(line) {
		err = io.ErrShortWrite
	}
	if err != nil {
		writer.mu.Lock()
		writer.latchLocked(fmt.Errorf("%s: %w", operation, err))
		writer.mu.Unlock()
	}
}

func (writer *auditWriter) persistSync(file *auditFile, operation string) {
	if file == nil {
		return
	}
	if err := file.sync(); err != nil {
		writer.mu.Lock()
		writer.latchLocked(fmt.Errorf("%s: %w", operation, err))
		writer.mu.Unlock()
	}
}

func (writer *auditWriter) finish() {
	writer.persistSync(writer.structured, "final structured sync")
	writer.persistSync(writer.raw, "final raw sync")
	for _, item := range []struct {
		name string
		file *auditFile
	}{{"structured close", writer.structured}, {"raw close", writer.raw}} {
		if item.file == nil {
			continue
		}
		if err := item.file.close(); err != nil {
			writer.mu.Lock()
			writer.latchLocked(fmt.Errorf("%s: %w", item.name, err))
			writer.mu.Unlock()
		}
	}
}

func (writer *auditWriter) sealAndWait(ctx context.Context) error {
	if writer == nil {
		return nil
	}
	writer.mu.Lock()
	writer.sealed = true
	writer.signal()
	writer.mu.Unlock()
	select {
	case <-writer.done:
		writer.mu.Lock()
		defer writer.mu.Unlock()
		return writer.err
	case <-ctx.Done():
		return fmt.Errorf("drain Codex SSH audit: %w", ctx.Err())
	}
}

func (writer *auditWriter) finished() bool {
	if writer == nil {
		return true
	}
	select {
	case <-writer.done:
		return true
	default:
		return false
	}
}

var _ runner.ToolBridge = (*Manager)(nil)

func New(options Options) (*Manager, error) {
	for name, path := range map[string]string{"client": options.ClientPath, "Codex": options.CodexPath, "supervisor": options.SupervisorPath} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return nil, fmt.Errorf("Codex SSH %s must be a regular executable file", name)
		}
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Codex SSH output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Codex SSH output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Codex SSH output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		outputDir: outputDir, cleanupTimeout: options.CleanupTimeout,
		clientPath: options.ClientPath, codexPath: options.CodexPath, supervisorPath: options.SupervisorPath,
		logger: options.Logger, openAudit: openAuditFile, omitRawLog: options.OmitRawLog,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return core.ToolEndpoint{}, errors.New("Codex SSH bridge is already active")
	}
	sandbox, ok := generic.(bridgeSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("Codex SSH bridge requires the local Docker sandbox capability")
	}
	if !validWorkdir(sandbox.Workdir()) {
		return core.ToolEndpoint{}, errors.New("Codex SSH requires an absolute task workdir")
	}
	taskUser, err := sandbox.TaskUser(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve Codex task user: %w", err)
	}
	gateway, err := sandbox.NetworkGateway(ctx)
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("resolve task network gateway: %w", err)
	}
	session := &bridgeSession{
		sandbox: sandbox, taskUser: taskUser, connections: make(map[net.Conn]struct{}),
		replyRequest: func(request *ssh.Request, accepted bool) error { return request.Reply(accepted, nil) },
	}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.revoke()
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
		defer cancel()
		waitErr := session.waitFor(cleanupCtx)
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
	if err := ensurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private Codex SSH artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.artifactDir, "aries-codex-ssh")
	if err := stageExecutable(manager.clientPath, session.clientSource); err != nil {
		return fail(err)
	}
	if err := session.stageExecutor(ctx, manager.codexPath, manager.supervisorPath); err != nil {
		return fail(err)
	}
	hostSigner, clientPEM, authorized, err := generateSessionKeys()
	if err != nil {
		return fail(err)
	}
	session.identitySource = filepath.Join(session.artifactDir, "id_ed25519")
	session.knownSource = filepath.Join(session.artifactDir, "known_hosts")
	session.toolLogPath = filepath.Join(session.artifactDir, "tool-calls.jsonl")
	if !manager.omitRawLog {
		session.rawLogPath = filepath.Join(session.artifactDir, "ssh_raw.log")
	}
	if err := writeExclusivePrivate(session.identitySource, clientPEM); err != nil {
		return fail(fmt.Errorf("write Codex SSH identity: %w", err))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return fail(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.listener = listener
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fail(fmt.Errorf("parse Codex SSH listener address: %w", err))
	}
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := writeExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return fail(fmt.Errorf("write Codex SSH known-hosts file: %w", err))
	}
	structured, err := manager.openAudit(session.toolLogPath)
	if err != nil {
		return fail(fmt.Errorf("create Codex SSH tool log: %w", err))
	}
	var raw *auditFile
	if session.rawLogPath != "" {
		raw, err = manager.openAudit(session.rawLogPath)
		if err != nil {
			return fail(errors.Join(fmt.Errorf("create Codex SSH raw log: %w", err), structured.close()))
		}
	}
	session.audit = newAuditWriter(structured, raw)
	serveCtx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	session.configuration = newServerConfig(hostSigner, authorized)
	session.wait.Add(1)
	go session.serve(serveCtx, manager.logger)
	if manager.afterStart != nil {
		if err := manager.afterStart(session); err != nil {
			return fail(err)
		}
	}
	manager.active = session
	manager.stopErr = nil
	address := net.JoinHostPort(host, port)
	network := sandbox.NetworkName()
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"address": address, "network": network, "container": sandbox.ContainerName()}).Info("Codex SSH bridge started")
	return core.ToolEndpoint{
		Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
		IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
		KnownHostsFile: "/run/aries/ssh/known_hosts", KnownHostsSourceFile: session.knownSource,
		ClientCommand: "/run/aries/ssh/aries-codex-ssh", ClientSourceFile: session.clientSource,
		Workdir:  sandbox.Workdir(),
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
	defer session.wait.Done()
	for {
		connection, err := session.listener.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				logger.WithError(err).Warn("Codex SSH accept failed")
			}
			return
		}
		session.mu.Lock()
		if session.revoked {
			session.mu.Unlock()
			_ = connection.Close()
			continue
		}
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
	// The one native executor connection outlives the SSH handshake deadline.
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
		audit := requestAudit{requestType: request.Type, wantReply: request.WantReply, payload: bytes.Clone(request.Payload)}
		var payload struct{ Command string }
		valid := request.Type == "exec" && request.WantReply && ssh.Unmarshal(request.Payload, &payload) == nil && payload.Command == "aries-codex-exec-server-v1"
		audit.remoteCommand = payload.Command
		if valid {
			session.mu.Lock()
			valid = !session.claimed && !session.revoked
			if valid {
				session.claimed = true
			}
			session.mu.Unlock()
		}
		if !valid {
			if request.WantReply {
				_ = session.reply(request, false)
			}
			session.logRejected(audit, "executor")
			return
		}
		if err := session.reply(request, true); err != nil {
			session.logRequestFailure(audit, "executor", "failed", "SSH accept reply failed")
			return
		}
		code := session.execute(ctx, channel, audit)
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}

// reply routes through session.replyRequest, which Start always populates and
// tests override to observe accept/reject outcomes.
func (session *bridgeSession) reply(request *ssh.Request, accepted bool) error {
	return session.replyRequest(request, accepted)
}

// Native execution can create sessions outside the initial process group.
// The subreaper's private proof, followed by Docker exec exit, is mandatory.
// Losing SSH only closes stdin: keep draining Docker output until that proof
// arrives instead of aborting the supervisor before it can reap its children.
func (session *bridgeSession) execute(ctx context.Context, channel ssh.Channel, audit requestAudit) int {
	started := time.Now()
	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		session.recordRevocationError(err)
		return 255
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	stdin := &recordedInput{reader: channel}
	stdout := &byteCounter{writer: disconnectedWriter{channel}}
	stderr := &byteCounter{writer: disconnectedWriter{channel.Stderr()}}
	proof := &executorProofWriter{writer: stderr, marker: []byte("\x1eARIES_CODEX_REAPED_" + nonce + "\x1f")}
	command := core.Command{Path: session.stageDir + "/supervisor", Args: []string{"--codex", session.stageDir + "/codex", "--stage-dir", session.stageDir, "--user", session.taskUser}, User: "0:0", Dir: session.sandbox.Workdir(), Env: map[string]string{"CODEX_HOME": session.stageDir + "/home"}, OutputLimitBytes: maxToolLogBytes}
	execCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-done:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(12 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			session.recordRevocationError(errors.New("Codex executor did not confirm descendant cleanup after SSH closed"))
			cancel()
		}
	}()
	session.executorAttempted = true
	result, err := session.sandbox.ExecSupervisedStream(execCtx, command, io.MultiReader(strings.NewReader(nonce+"\n"), stdin), stdout, proof)
	close(done)
	<-watchDone
	if err == nil {
		err = proof.finish()
	}
	if result.ExitCode != 0 {
		err = errors.Join(err, fmt.Errorf("Codex supervisor exited with status %d", result.ExitCode))
	}
	status, message, exitCode := "completed", "", 0
	if err != nil {
		session.recordRevocationError(err)
		status, message, exitCode = "failed", "executor cleanup was not confirmed", 255
	} else {
		// The protected supervisor removes its stage using Go filesystem calls
		// before proving cleanup. Never execute task-owned programs afterward.
		session.stageOwned = false
	}
	stdinBytes, stdinContent, stdinEncoding, rawStdin, overflow := stdin.record(session.audit.retainsRaw())
	if overflow {
		session.audit.latch(fmt.Errorf("retain Codex SSH RPC input: exceeds %d bytes", maxRecordedInputBytes))
		return exitCode
	}
	session.writeRecord(toolCallRecord{ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(), OperationClass: "executor", Path: command.Path, Workdir: command.Dir, CommandHash: commandHash(audit.remoteCommand), Command: audit.remoteCommand, Argv: append([]string{command.Path}, command.Args...), Stdin: stdinContent, StdinEncoding: stdinEncoding, StdinBytes: stdinBytes, StdoutBytes: stdout.count(), StderrBytes: stderr.count(), ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message, RequestType: audit.requestType, WantReply: audit.wantReply}, rawRecord(audit, stdinBytes, rawStdin, status))
	return exitCode
}

type disconnectedWriter struct{ writer io.Writer }

func (w disconnectedWriter) Write(p []byte) (int, error) {
	_, _ = w.writer.Write(p)
	return len(p), nil
}

type executorProofWriter struct {
	writer          io.Writer
	marker, pending []byte
}

func (w *executorProofWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	if n := len(w.pending) - len(w.marker); n > 0 {
		if _, err := w.writer.Write(w.pending[:n]); err != nil {
			return 0, err
		}
		w.pending = bytes.Clone(w.pending[n:])
	}
	return len(p), nil
}
func (w *executorProofWriter) finish() error {
	if !bytes.Equal(w.pending, w.marker) {
		_, _ = w.writer.Write(w.pending)
		return errors.New("Codex supervisor did not confirm all descendants reaped")
	}
	clear(w.pending)
	return nil
}

func (session *bridgeSession) logRejected(audit requestAudit, kind string) {
	session.logRequestFailure(audit, kind, "rejected", "invalid remote command")
}

// logRequestFailure records a refused request without claiming execution.
func (session *bridgeSession) logRequestFailure(audit requestAudit, kind, status, message string) {
	session.writeRecord(toolCallRecord{
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

func rawRecord(audit requestAudit, stdinBytes int64, stdin []byte, status string) rawSSHRecord {
	return rawSSHRecord{
		RequestType: audit.requestType, WantReply: audit.wantReply,
		WireCommand: audit.remoteCommand, Payload: bytes.Clone(audit.payload), PayloadBytes: int64(len(audit.payload)),
		Stdin: bytes.Clone(stdin), StdinBytes: stdinBytes, Status: status,
	}
}

func (session *bridgeSession) writeRecord(record toolCallRecord, raw rawSSHRecord) {
	record.RunID = session.sandbox.RunID()
	record.TaskID = session.sandbox.TaskID()
	raw.RunID = session.sandbox.RunID()
	raw.TaskID = session.sandbox.TaskID()
	raw.ContainerID = session.sandbox.ContainerID()
	session.audit.enqueue(record, raw)
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

func (session *bridgeSession) finalize(ctx context.Context) error {
	auditErr := session.closeAudit(ctx)
	if session.audit != nil && !session.audit.finished() {
		return auditErr
	}
	if session.cancel != nil {
		session.cancel()
	}
	// No task runtime staging remains when independent evaluation begins.
	cleanupErr := errors.Join(
		session.revocationError(), auditErr, session.removeExecutor(ctx),
		removeIfPresent(session.identitySource), removeIfPresent(session.clientSource),
	)
	if session.partialStart && cleanupErr == nil {
		cleanupErr = os.RemoveAll(session.artifactDir)
	}
	return cleanupErr
}

func (session *bridgeSession) recordRevocationError(err error) {
	if err == nil {
		return
	}
	session.revocationMu.Lock()
	session.revocationErr = errors.Join(session.revocationErr, err)
	session.revocationMu.Unlock()
}

func (session *bridgeSession) revocationError() error {
	session.revocationMu.Lock()
	defer session.revocationMu.Unlock()
	return session.revocationErr
}

func (session *bridgeSession) revoke() {
	session.revokeOnce.Do(func() {
		if session.listener != nil {
			_ = session.listener.Close()
		}
		session.mu.Lock()
		session.revoked = true
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

func (session *bridgeSession) closeAudit(ctx context.Context) error {
	if session.audit == nil {
		return nil
	}
	return session.audit.sealAndWait(ctx)
}

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
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

func writeExclusivePrivate(path string, content []byte) error {
	return writeExclusive(path, content, 0o600)
}

type exclusiveWriteFile interface {
	Write([]byte) (int, error)
	Sync() error
	Chmod(os.FileMode) error
	Close() error
}

type exclusiveWriteOperations struct {
	open   func(string, int, os.FileMode) (exclusiveWriteFile, error)
	remove func(string) error
}

func writeExclusive(path string, content []byte, mode os.FileMode) error {
	return writeExclusiveWithOperations(path, content, mode, exclusiveWriteOperations{
		open: func(path string, flags int, mode os.FileMode) (exclusiveWriteFile, error) {
			return os.OpenFile(path, flags, mode)
		},
		remove: os.Remove,
	})
}

func writeExclusiveWithOperations(path string, content []byte, mode os.FileMode, operations exclusiveWriteOperations) error {
	file, err := operations.open(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	cleanup := func(primary error, needsClose bool) error {
		var closeErr error
		if needsClose {
			if err := file.Close(); err != nil {
				closeErr = fmt.Errorf("close exclusive file: %w", err)
			}
		}
		removeErr := operations.remove(path)
		if removeErr != nil {
			removeErr = fmt.Errorf("remove failed exclusive file: %w", removeErr)
		}
		return errors.Join(primary, closeErr, removeErr)
	}
	written, err := file.Write(content)
	if err == nil && written != len(content) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return cleanup(fmt.Errorf("write exclusive file: %w", err), true)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync exclusive file data: %w", err), true)
	}
	if err := file.Chmod(mode); err != nil {
		return cleanup(fmt.Errorf("chmod exclusive file: %w", err), true)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("sync exclusive file metadata: %w", err), true)
	}
	if err := file.Close(); err != nil {
		return cleanup(fmt.Errorf("close exclusive file: %w", err), false)
	}
	return nil
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

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
