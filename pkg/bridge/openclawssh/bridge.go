package openclawssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"golang.org/x/crypto/ssh"
)

const (
	defaultClientPath    = "bin/aries-ssh"
	defaultServerPath    = "bin/aries-ssh-server"
	defaultBridgeCleanup = 20 * time.Second
	controlContainerDir  = "/run/aries"
	trustedExecHelper    = "/opt/aries/bin/aries-exec-helper"
	maxControlPeers      = 32
)

// Options are the explicit host-local inputs to the OpenClaw SSH bridge.
type Options struct {
	OutputDir      string
	ClientPath     string
	ServerPath     string
	WorkspaceRoot  string
	RuntimeID      string
	CleanupTimeout time.Duration
	Logger         *slog.Logger
}

// Manager grants one task-local OpenClaw SSH endpoint at a time.
type Manager struct {
	outputDir      string
	clientPath     string
	serverPath     string
	workspaceRoot  string
	runtimeID      string
	cleanupTimeout time.Duration
	logger         *slog.Logger
	newID          func() (string, error)
	probe          func(context.Context, string, ssh.Signer, ssh.PublicKey) error
	waitListener   func(context.Context, string) error
	oldKeyRejected func(context.Context, *bridgeSession) error
	startServer    func(context.Context, bridgeSandbox, string, string, string, []byte, []byte, []byte) (io.Closer, int, bool, error)

	mu       sync.Mutex
	active   *bridgeSession
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

type bridgeSandbox interface {
	runner.Sandbox
	NetworkName() string
	Workdir() string
	RuntimeDir() string
	ContainerIPv4(context.Context) (string, error)
	ProcessPresent(context.Context, int) (bool, error)
	RestartForIsolation(context.Context) error
}

type bridgeSession struct {
	sandbox              bridgeSandbox
	artifactDir          string
	clientSource         string
	identitySource       string
	knownSource          string
	controlHost          string
	control              io.Closer
	serverPID            int
	containerIP          string
	clientSigner         ssh.Signer
	hostKey              ssh.PublicKey
	prepared             bool
	prepareAttempted     bool
	workspaceOwnerToken  []byte
	serverUploaded       bool
	serverSpawnAttempted bool
}

var _ runner.ToolBridge = (*Manager)(nil)

// New constructs an OpenClaw SSH bridge without starting a process.
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
	if options.ServerPath == "" {
		options.ServerPath = defaultServerPath
	}
	clientPath, err := filepath.Abs(options.ClientPath)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw SSH client helper: %w", err)
	}
	serverPath, err := filepath.Abs(options.ServerPath)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw SSH server helper: %w", err)
	}
	if options.WorkspaceRoot == "" {
		options.WorkspaceRoot = defaultWorkspaceRoot
	}
	if options.RuntimeID == "" {
		options.RuntimeID = lockedRuntimeID
	}
	if _, _, err := validateWorkspacePaths("/temporary-workdir", options.WorkspaceRoot, options.RuntimeID); err != nil {
		return nil, fmt.Errorf("validate OpenClaw SSH workspace settings: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultBridgeCleanup
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Manager{
		outputDir:      outputDir,
		clientPath:     clientPath,
		serverPath:     serverPath,
		workspaceRoot:  options.WorkspaceRoot,
		runtimeID:      options.RuntimeID,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
		newID:          randomBridgeID,
		probe:          probeSSH,
		waitListener:   waitListenerAbsent,
		oldKeyRejected: requireOldKeyRejected,
		startServer:    startManagedServer,
	}, nil
}

// Start deploys a task-local server, prepares the pinned OpenClaw workspace,
// and returns only file paths and endpoint metadata. A failed partial Start
// rolls itself back under a fresh bounded context.
func (m *Manager) Start(ctx context.Context, generic runner.Sandbox) (core.ToolEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil || m.stopping {
		return core.ToolEndpoint{}, errors.New("OpenClaw SSH bridge is already active")
	}
	sandbox, ok := generic.(bridgeSandbox)
	if !ok {
		return core.ToolEndpoint{}, errors.New("OpenClaw SSH bridge requires the local Docker sandbox capability")
	}
	id, err := m.newID()
	if err != nil {
		return core.ToolEndpoint{}, fmt.Errorf("generate OpenClaw SSH bridge ID: %w", err)
	}
	session := &bridgeSession{sandbox: sandbox}
	fail := func(primary error) (core.ToolEndpoint, error) {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.cleanupTimeout)
		defer cancel()
		cleanupErr := m.rollbackStart(cleanupCtx, session)
		if cleanupErr != nil {
			return core.ToolEndpoint{}, errors.Join(primary, fmt.Errorf("rollback partial OpenClaw SSH bridge: %w", cleanupErr))
		}
		return core.ToolEndpoint{}, primary
	}

	session.artifactDir = filepath.Join(m.outputDir, "bridges", id)
	if err := ensurePrivateDirectory(session.artifactDir); err != nil {
		return fail(fmt.Errorf("create private OpenClaw SSH artifact directory: %w", err))
	}
	session.clientSource = filepath.Join(session.artifactDir, "aries-ssh")
	if err := stageExecutable(m.clientPath, session.clientSource); err != nil {
		return fail(fmt.Errorf("stage OpenClaw SSH client: %w", err))
	}
	hostPrivate, identityPrivate, hostPublic, clientSigner, authorized, err := generateSessionKeys()
	if err != nil {
		return fail(err)
	}
	session.clientSigner = clientSigner
	session.hostKey = hostPublic
	session.identitySource = filepath.Join(session.artifactDir, "id_ed25519")
	session.knownSource = filepath.Join(session.artifactDir, "known_hosts")
	if err := writeExclusivePrivate(session.identitySource, identityPrivate); err != nil {
		return fail(fmt.Errorf("write OpenClaw SSH identity: %w", err))
	}
	knownLine := fmt.Sprintf("[%s]:%d %s", lockedHostName, lockedPort, ssh.MarshalAuthorizedKey(hostPublic))
	if err := writeExclusivePrivate(session.knownSource, []byte(knownLine)); err != nil {
		return fail(fmt.Errorf("write OpenClaw SSH known-hosts file: %w", err))
	}
	if err := sandbox.Upload(ctx, m.serverPath, serverContainerPath); err != nil {
		return fail(fmt.Errorf("upload OpenClaw SSH server: %w", err))
	}
	session.serverUploaded = true
	session.workspaceOwnerToken = make([]byte, workspaceOwnerTokenBytes)
	if _, err := rand.Read(session.workspaceOwnerToken); err != nil {
		return fail(fmt.Errorf("generate workspace ownership token: %w", err))
	}
	workspace := filepath.Join(m.workspaceRoot, m.runtimeID, "workspace")
	prepare := core.Command{
		Path: serverContainerPath,
		Args: []string{"prepare", "--workdir", sandbox.Workdir(), "--workspace-root", m.workspaceRoot, "--runtime-id", m.runtimeID},
		Dir:  sandbox.Workdir(), Stdin: session.workspaceOwnerToken, Timeout: 10 * time.Second,
	}
	session.prepareAttempted = true
	result, err := sandbox.Exec(ctx, prepare)
	if err != nil || result.ExitCode != 0 {
		return fail(commandFailure("prepare OpenClaw SSH workspace", result, err))
	}
	session.prepared = true

	controlName := "ssh-" + id + ".sock"
	session.controlHost = filepath.Join(sandbox.RuntimeDir(), controlName)
	if err := os.Remove(session.controlHost); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(fmt.Errorf("remove stale OpenClaw SSH control socket: %w", err))
	}
	token := make([]byte, controlTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return fail(fmt.Errorf("generate OpenClaw SSH control token: %w", err))
	}
	controlContainer := filepath.Join(controlContainerDir, controlName)
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	defer readyCancel()
	session.serverSpawnAttempted = true
	control, pid, spawned, err := m.startServer(readyCtx, sandbox, session.controlHost, controlContainer, workspace, token, hostPrivate, authorized)
	session.serverSpawnAttempted = session.serverSpawnAttempted || spawned
	if err != nil {
		return fail(err)
	}
	session.control = control
	session.serverPID = pid
	containerIP, err := sandbox.ContainerIPv4(readyCtx)
	if err != nil {
		return fail(err)
	}
	session.containerIP = containerIP
	if err := m.probe(readyCtx, containerIP, clientSigner, hostPublic); err != nil {
		return fail(fmt.Errorf("probe OpenClaw SSH endpoint: %w", err))
	}

	m.active = session
	m.stopErr = nil
	m.logger.InfoContext(ctx, "OpenClaw SSH bridge started", "address", lockedHostName+":"+strconv.Itoa(lockedPort), "network", sandbox.NetworkName())
	return core.ToolEndpoint{
		Protocol:             "ssh",
		Address:              net.JoinHostPort(lockedHostName, strconv.Itoa(lockedPort)),
		Username:             lockedUsername,
		Network:              sandbox.NetworkName(),
		ClientCommand:        clientContainerPath,
		ClientSourceFile:     session.clientSource,
		IdentityFile:         identityContainerPath,
		IdentitySourceFile:   session.identitySource,
		KnownHostsFile:       knownHostsContainerPath,
		KnownHostsSourceFile: session.knownSource,
	}, nil
}

func startManagedServer(ctx context.Context, sandbox bridgeSandbox, controlHost, controlContainer, workspace string, token, hostPrivate, authorized []byte) (io.Closer, int, bool, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: controlHost, Net: "unix"})
	if err != nil {
		return nil, 0, false, fmt.Errorf("listen for OpenClaw SSH control: %w", err)
	}
	defer listener.Close()
	if err := os.Chmod(controlHost, 0o600); err != nil {
		return nil, 0, false, fmt.Errorf("protect OpenClaw SSH control socket: %w", err)
	}
	spawn := core.Command{
		Path: serverContainerPath,
		Args: []string{"spawn", "--control", controlContainer, "--listen", net.JoinHostPort("0.0.0.0", strconv.Itoa(lockedPort)), "--workspace", workspace},
		Dir:  workspace, Stdin: token, Timeout: 10 * time.Second,
	}
	spawnResult, err := sandbox.Exec(ctx, spawn)
	if err != nil || spawnResult.ExitCode != 0 {
		return nil, 0, true, commandFailure("spawn OpenClaw SSH server", spawnResult, err)
	}
	control, pid, err := acceptControl(ctx, listener, token)
	if err != nil {
		return nil, 0, true, fmt.Errorf("authenticate OpenClaw SSH server control: %w", err)
	}
	if err := requireProcessPresent(ctx, sandbox, pid); err != nil {
		_ = control.Close()
		return nil, pid, true, err
	}
	bootstrap := controlBootstrap{
		HostPrivateKey: hostPrivate,
		AuthorizedKey:  authorized,
		Username:       lockedUsername,
		Listen:         net.JoinHostPort("0.0.0.0", strconv.Itoa(lockedPort)),
		Workspace:      workspace,
	}
	if err := writeControlFrame(control, bootstrap); err != nil {
		_ = control.Close()
		return nil, pid, true, fmt.Errorf("bootstrap OpenClaw SSH server: %w", err)
	}
	var ready controlReady
	if err := readControlFrame(control, &ready); err != nil {
		_ = control.Close()
		return nil, pid, true, fmt.Errorf("wait for OpenClaw SSH server readiness: %w", err)
	}
	if ready.Address != net.JoinHostPort("0.0.0.0", strconv.Itoa(lockedPort)) {
		_ = control.Close()
		return nil, pid, true, fmt.Errorf("OpenClaw SSH server reported unexpected address %q", ready.Address)
	}
	_ = control.SetDeadline(time.Time{})
	return control, pid, true, nil
}

func requireProcessPresent(ctx context.Context, sandbox bridgeSandbox, pid int) error {
	present, err := sandbox.ProcessPresent(ctx, pid)
	if err != nil {
		return fmt.Errorf("confirm authenticated OpenClaw SSH server PID before bootstrap: %w", err)
	}
	if !present {
		return errors.New("authenticated OpenClaw SSH server PID disappeared before bootstrap")
	}
	return nil
}

// Stop positively revokes the listener and exact authenticated server PID,
// rejects the old key, removes the deployed server and deletes all credentials.
// Concurrent callers share one attempt; a later call retries a failed attempt.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.active == nil && !m.stopping {
		err := m.stopErr
		m.mu.Unlock()
		return err
	}
	if m.stopping {
		done := m.stopDone
		m.mu.Unlock()
		select {
		case <-done:
			m.mu.Lock()
			err := m.stopErr
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	session := m.active
	m.stopping = true
	m.stopDone = make(chan struct{})
	done := m.stopDone
	m.mu.Unlock()

	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), m.cleanupTimeout)
	err := m.stopSession(cleanupCtx, session, false)
	cleanupCancel()

	m.mu.Lock()
	m.stopErr = err
	m.stopping = false
	if err == nil {
		m.active = nil
	}
	close(done)
	m.mu.Unlock()
	return err
}

func (m *Manager) rollbackStart(ctx context.Context, session *bridgeSession) error {
	return m.stopSession(ctx, session, true)
}

func (m *Manager) stopSession(ctx context.Context, session *bridgeSession, restore bool) error {
	if session == nil {
		return nil
	}
	var errs []error
	if session.control != nil {
		if err := session.control.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, fmt.Errorf("close OpenClaw SSH control: %w", err))
		}
		session.control = nil
	}
	if session.serverSpawnAttempted {
		if err := session.sandbox.RestartForIsolation(ctx); err != nil {
			errs = append(errs, fmt.Errorf("restart task sandbox to revoke every SSH descendant: %w", err))
		}
		currentIP, err := session.sandbox.ContainerIPv4(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect restarted task sandbox address: %w", err))
		} else {
			session.containerIP = currentIP
		}
		if session.serverPID > 0 {
			present, err := session.sandbox.ProcessPresent(ctx, session.serverPID)
			if err != nil {
				errs = append(errs, fmt.Errorf("confirm prior OpenClaw SSH server PID absent after restart: %w", err))
			} else if present {
				errs = append(errs, errors.New("prior OpenClaw SSH server PID remains after restart"))
			}
		}
		if session.containerIP != "" {
			if err := m.waitListener(ctx, session.containerIP); err != nil {
				errs = append(errs, err)
			}
			if err := m.oldKeyRejected(ctx, session); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if !restore && session.prepared {
		if err := verifyWorkspaceThroughTrustedHelper(ctx, session.sandbox, m.workspaceRoot, m.runtimeID); err != nil {
			errs = append(errs, err)
		}
	}
	if restore && session.prepareAttempted {
		result, err := session.sandbox.Exec(ctx, core.Command{
			Path:    trustedExecHelper,
			Args:    []string{"--recover-workspace", session.sandbox.Workdir(), m.workspaceRoot, m.runtimeID},
			Stdin:   session.workspaceOwnerToken,
			Timeout: 10 * time.Second,
		})
		if err != nil || result.ExitCode != 0 {
			errs = append(errs, commandFailure("reconcile task workdir after bridge Start failure", result, err))
		} else {
			session.prepared = false
			session.prepareAttempted = false
			clear(session.workspaceOwnerToken)
			session.workspaceOwnerToken = nil
		}
	}
	if session.serverUploaded {
		if err := removeServerBinary(ctx, session.sandbox); err != nil {
			errs = append(errs, err)
		} else {
			session.serverUploaded = false
		}
	}
	if session.controlHost != "" {
		if err := os.Remove(session.controlHost); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove OpenClaw SSH control socket: %w", err))
		}
		if _, err := os.Lstat(session.controlHost); err == nil {
			errs = append(errs, errors.New("confirm OpenClaw SSH control socket absence: path remains"))
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("confirm OpenClaw SSH control socket absence: %w", err))
		}
	}
	if session.artifactDir != "" {
		if err := os.RemoveAll(session.artifactDir); err != nil {
			errs = append(errs, fmt.Errorf("delete OpenClaw SSH credentials: %w", err))
		}
		if _, err := os.Lstat(session.artifactDir); err == nil {
			errs = append(errs, errors.New("confirm OpenClaw SSH credential deletion: artifact directory remains"))
		} else if !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("confirm OpenClaw SSH credential deletion: %w", err))
		}
	}
	return errors.Join(errs...)
}

func acceptControl(ctx context.Context, listener *net.UnixListener, token []byte) (*net.UnixConn, int, error) {
	for rejected := 0; rejected <= maxControlPeers; rejected++ {
		deadline := time.Now().Add(100 * time.Millisecond)
		if remaining, ok := ctx.Deadline(); ok && remaining.Before(deadline) {
			deadline = remaining
		}
		if err := listener.SetDeadline(deadline); err != nil {
			return nil, 0, err
		}
		connection, err := listener.AcceptUnix()
		if err != nil {
			var netError net.Error
			if errors.As(err, &netError) && netError.Timeout() {
				if ctx.Err() != nil {
					return nil, 0, ctx.Err()
				}
				continue
			}
			return nil, 0, err
		}
		_ = connection.SetDeadline(time.Now().Add(time.Second))
		pid, peerErr := unixPeerPID(connection)
		var hello controlHello
		frameErr := readControlFrame(connection, &hello)
		if peerErr == nil && frameErr == nil && pid > 0 && hello.Magic == controlMagic && len(hello.Token) == len(token) && subtle.ConstantTimeCompare(hello.Token, token) == 1 {
			return connection, pid, nil
		}
		_ = connection.Close()
	}
	return nil, 0, fmt.Errorf("more than %d control peers failed authentication", maxControlPeers)
}

func probeSSH(ctx context.Context, host string, signer ssh.Signer, hostKey ssh.PublicKey) error {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	address := net.JoinHostPort(host, strconv.Itoa(lockedPort))
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(probeCtx, "tcp", address)
	if err != nil {
		return err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	configuration := &ssh.ClientConfig{
		User:              lockedUsername,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
	}
	configuration.HostKeyCallback = func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		if !bytes.Equal(presented.Marshal(), hostKey.Marshal()) {
			return errors.New("host key mismatch")
		}
		return nil
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, address, configuration)
	if err != nil {
		return err
	}
	client := ssh.NewClient(clientConnection, channels, requests)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	return session.Run(encodeCanonicalTokens([]string{remoteShell, "-c", "true"}))
}

func requireOldKeyRejected(ctx context.Context, session *bridgeSession) error {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := probeSSH(probeCtx, session.containerIP, session.clientSigner, session.hostKey); err == nil {
		return errors.New("old OpenClaw SSH key still connects after bridge Stop")
	}
	return nil
}

func waitListenerAbsent(ctx context.Context, host string) error {
	address := net.JoinHostPort(host, strconv.Itoa(lockedPort))
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = connection.Close()
		select {
		case <-ctx.Done():
			return fmt.Errorf("confirm OpenClaw SSH listener absence: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func removeServerBinary(ctx context.Context, sandbox bridgeSandbox) error {
	result, err := sandbox.Exec(ctx, core.Command{Path: trustedExecHelper, Args: []string{"--remove-file", serverContainerPath}, Timeout: 5 * time.Second})
	if err != nil || result.ExitCode != 0 {
		return commandFailure("remove and confirm OpenClaw SSH server deletion", result, err)
	}
	return nil
}

func verifyWorkspaceThroughTrustedHelper(ctx context.Context, sandbox bridgeSandbox, workspaceRoot, runtimeID string) error {
	workspace := filepath.Join(workspaceRoot, runtimeID, "workspace")
	result, err := sandbox.Exec(ctx, core.Command{
		Path:    trustedExecHelper,
		Args:    []string{"--verify-workspace", sandbox.Workdir(), workspace},
		Timeout: 5 * time.Second,
	})
	if err != nil || result.ExitCode != 0 {
		return commandFailure("verify evaluator and OpenClaw workspace identity", result, err)
	}
	return nil
}

func generateSessionKeys() ([]byte, []byte, ssh.PublicKey, ssh.Signer, []byte, error) {
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate SSH host key: %w", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("create SSH host signer: %w", err)
	}
	_, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("generate SSH client key: %w", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("create SSH client signer: %w", err)
	}
	hostDER, err := x509.MarshalPKCS8PrivateKey(hostPrivate)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("marshal SSH host key: %w", err)
	}
	hostPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: hostDER})
	clientPEM, err := marshalEd25519PrivateKey(clientPrivate)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("marshal SSH client key: %w", err)
	}
	return hostPEM, clientPEM, hostSigner.PublicKey(), clientSigner, ssh.MarshalAuthorizedKey(clientSigner.PublicKey()), nil
}

func marshalEd25519PrivateKey(private ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func stageExecutable(source, destination string) error {
	fd, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	input := os.NewFile(uintptr(fd), source)
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0o111 == 0 {
		return errors.New("helper source must be a regular executable")
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		output.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	copied, err := io.Copy(output, input)
	if err != nil {
		return err
	}
	after, err := input.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != copied || after.Size() != copied || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("helper source changed while being staged")
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func writeExclusivePrivate(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
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

func commandFailure(operation string, result core.CommandResult, err error) error {
	message := fmt.Sprintf("%s: exit %d", operation, result.ExitCode)
	if stderr := strings.TrimSpace(result.Stderr); stderr != "" {
		message += ": " + stderr
	}
	if err != nil {
		return fmt.Errorf("%s: %w", message, err)
	}
	return errors.New(message)
}
