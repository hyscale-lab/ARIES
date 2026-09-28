package codexssh

import (
	"bytes"
	"context"
	"crypto/rand"
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
	defaultBridgeCleanup = 20 * time.Second

	identityContainerPath = "/run/aries/ssh/id_ed25519"
	lockedUsername        = "aries"
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
	server            *sshbridge.Server
	artifactDir       string
	identitySource    string
	knownSource       string
	toolLogPath       string
	rawLogPath        string
	audit             *sshbridge.AuditWriter
	partialStart      bool
	replyRequest      func(*ssh.Request, bool) error

	mu            sync.Mutex
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
	if err := sshbridge.EnsurePrivateDirectory(outputDir); err != nil {
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
		logger: options.Logger, openAudit: sshbridge.OpenAuditFile, omitRawLog: options.OmitRawLog,
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
		sandbox: sandbox, taskUser: taskUser,
		replyRequest: func(request *ssh.Request, accepted bool) error { return request.Reply(accepted, nil) },
	}
	session.artifactDir = filepath.Join(manager.outputDir, sandbox.TaskID(), "bridge")
	fail := func(primary error) (core.ToolEndpoint, error) {
		session.partialStart = true
		session.revoke()
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
		return fail(fmt.Errorf("create private Codex SSH artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.artifactDir, "aries-codex-ssh")
	if err := stageExecutable(manager.clientPath, session.clientSource); err != nil {
		return fail(err)
	}
	if err := session.stageExecutor(ctx, manager.codexPath, manager.supervisorPath); err != nil {
		return fail(err)
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
		return fail(fmt.Errorf("write Codex SSH identity: %w", err))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(gateway, "0"))
	if err != nil {
		return fail(fmt.Errorf("listen on task network gateway: %w", err))
	}
	session.server = sshbridge.NewServer(listener, hostSigner, authorized)
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return fail(fmt.Errorf("parse Codex SSH listener address: %w", err))
	}
	knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	if err := sshbridge.WriteExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return fail(fmt.Errorf("write Codex SSH known-hosts file: %w", err))
	}
	structured, err := manager.openAudit(session.toolLogPath)
	if err != nil {
		return fail(fmt.Errorf("create Codex SSH tool log: %w", err))
	}
	var raw *sshbridge.AuditFile
	if session.rawLogPath != "" {
		raw, err = manager.openAudit(session.rawLogPath)
		if err != nil {
			return fail(errors.Join(fmt.Errorf("create Codex SSH raw log: %w", err), structured.Close()))
		}
	}
	session.audit = sshbridge.NewAuditWriter("Codex", structured, raw)
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
	stdin := sshbridge.NewRecordedInput("Codex", channel)
	stdout := &sshbridge.ByteCounter{Writer: disconnectedWriter{channel}}
	stderr := &sshbridge.ByteCounter{Writer: disconnectedWriter{channel.Stderr()}}
	proof := &executorProofWriter{writer: stderr, marker: []byte("\x1eARIES_CODEX_REAPED_" + nonce + "\x1f")}
	command := core.Command{Path: session.stageDir + "/supervisor", Args: []string{"--codex", session.stageDir + "/codex", "--stage-dir", session.stageDir, "--user", session.taskUser}, User: "0:0", Dir: session.sandbox.Workdir(), Env: map[string]string{"CODEX_HOME": session.stageDir + "/home"}, OutputLimitBytes: sshbridge.MaxToolLogBytes}
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
	stdinBytes, stdinContent, stdinEncoding, rawStdin, overflow := stdin.Record(session.audit.RetainsRaw())
	if overflow {
		session.audit.Latch(fmt.Errorf("retain Codex SSH RPC input: exceeds %d bytes", sshbridge.MaxRecordedInputBytes))
		return exitCode
	}
	session.writeRecord(sshbridge.ToolCallRecord{ContainerID: session.sandbox.ContainerID(), ContainerName: session.sandbox.ContainerName(), OperationClass: "executor", Path: command.Path, Workdir: command.Dir, CommandHash: commandHash(audit.remoteCommand), Command: audit.remoteCommand, Argv: append([]string{command.Path}, command.Args...), Stdin: stdinContent, StdinEncoding: stdinEncoding, StdinBytes: stdinBytes, StdoutBytes: stdout.Count(), StderrBytes: stderr.Count(), ExitCode: exitCode, DurationMS: time.Since(started).Milliseconds(), Status: status, Error: message, RequestType: audit.requestType, WantReply: audit.wantReply}, rawRecord(audit, stdinBytes, rawStdin, status))
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

	session.revoke()
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
	auditErr := session.audit.SealAndWait(ctx)
	if session.audit != nil && !session.audit.Finished() {
		return auditErr
	}
	// No task runtime staging remains when independent evaluation begins.
	cleanupErr := errors.Join(
		session.revocationError(), auditErr, session.removeExecutor(ctx),
		sshbridge.RemoveIfPresent(session.identitySource), sshbridge.RemoveIfPresent(session.clientSource),
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
	session.mu.Lock()
	session.revoked = true
	session.mu.Unlock()
	session.server.Revoke()
}

func commandHash(command string) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])
}
