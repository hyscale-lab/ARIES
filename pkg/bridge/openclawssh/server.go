package openclawssh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	lockedRuntimeID          = "openclaw-ssh-shared-8198076c"
	defaultWorkspaceRoot     = "/aries/openclaw"
	maxControlFrame          = 128 << 10
	controlTokenBytes        = 32
	serverCommandTimeout     = 20 * time.Minute
	serverShutdownTimeout    = 5 * time.Second
	serverHandshakeTimeout   = 5 * time.Second
	maxConcurrentConnections = 8
	maxGlobalRequests        = 96
	controlMagic             = "ARIES-SSH-CONTROL-1"
	workspaceRootExistsExit  = 73
	workspaceOwnerTokenBytes = 32
	workspaceOwnerMarker     = ".aries-workspace-owner-v1"
)

type workspaceRootExistsError struct{}

func (workspaceRootExistsError) Error() string { return "workspace root already exists" }

type serverArguments struct {
	control   string
	listen    string
	workspace string
}

type controlHello struct {
	Magic string `json:"magic"`
	Token []byte `json:"token"`
}

type controlBootstrap struct {
	HostPrivateKey []byte `json:"host_private_key"`
	AuthorizedKey  []byte `json:"authorized_key"`
	Username       string `json:"username"`
	Listen         string `json:"listen"`
	Workspace      string `json:"workspace"`
}

type controlReady struct {
	Address string `json:"address"`
}

// ServerMain runs the static server helper's prepare, spawn, or serve mode.
// Spawn never invokes a shell and passes only a random control token to the
// detached child.
func ServerMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "aries-ssh-server: a mode is required")
		return 125
	}
	var err error
	switch args[0] {
	case "prepare":
		var workdir, workspaceRoot, runtimeID string
		flags := flag.NewFlagSet("prepare", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&workdir, "workdir", "", "")
		flags.StringVar(&workspaceRoot, "workspace-root", "", "")
		flags.StringVar(&runtimeID, "runtime-id", "", "")
		err = flags.Parse(args[1:])
		if err == nil && flags.NArg() == 0 {
			var ownerToken []byte
			ownerToken, err = readBounded(stdin, workspaceOwnerTokenBytes)
			if err == nil && len(ownerToken) != workspaceOwnerTokenBytes {
				err = fmt.Errorf("workspace ownership token has %d bytes, want %d", len(ownerToken), workspaceOwnerTokenBytes)
			}
			if err == nil {
				err = prepareWorkspace(workdir, workspaceRoot, runtimeID, ownerToken)
			}
		} else if err == nil {
			err = errors.New("prepare received positional arguments")
		}
	case "spawn", "serve":
		parsed, parseErr := parseServerArguments(args[0], args[1:])
		if parseErr != nil {
			err = parseErr
		} else if args[0] == "spawn" {
			err = spawnServer(parsed, stdin)
		} else {
			err = runServer(parsed, stdin)
		}
	default:
		err = fmt.Errorf("unsupported mode %q", args[0])
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh-server: %v\n", err)
		var preexisting workspaceRootExistsError
		if errors.As(err, &preexisting) {
			return workspaceRootExistsExit
		}
		return 125
	}
	return 0
}

func parseServerArguments(mode string, args []string) (serverArguments, error) {
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var parsed serverArguments
	flags.StringVar(&parsed.control, "control", "", "")
	flags.StringVar(&parsed.listen, "listen", "", "")
	flags.StringVar(&parsed.workspace, "workspace", "", "")
	if err := flags.Parse(args); err != nil {
		return serverArguments{}, err
	}
	if flags.NArg() != 0 {
		return serverArguments{}, errors.New("server received positional arguments")
	}
	if parsed.control == "" || strings.ContainsRune(parsed.control, 0) || !filepath.IsAbs(parsed.control) || filepath.Clean(parsed.control) != parsed.control {
		return serverArguments{}, errors.New("server control socket must be an absolute clean path")
	}
	if parsed.listen != net.JoinHostPort("0.0.0.0", strconv.Itoa(lockedPort)) {
		return serverArguments{}, errors.New("server listen address does not match the locked task-network endpoint")
	}
	if parsed.workspace == "" || !filepath.IsAbs(parsed.workspace) || filepath.Clean(parsed.workspace) != parsed.workspace || strings.ContainsRune(parsed.workspace, 0) {
		return serverArguments{}, errors.New("server workspace must be an absolute clean path")
	}
	return parsed, nil
}

func spawnServer(arguments serverArguments, input io.Reader) error {
	token, err := readBounded(input, controlTokenBytes)
	if err != nil {
		return fmt.Errorf("read control token: %w", err)
	}
	if len(token) != controlTokenBytes {
		return fmt.Errorf("control token has %d bytes, want %d", len(token), controlTokenBytes)
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate server executable: %w", err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create server bootstrap pipe: %w", err)
	}
	if _, err := writer.Write(token); err != nil {
		reader.Close()
		writer.Close()
		return fmt.Errorf("fill server bootstrap pipe: %w", err)
	}
	if err := writer.Close(); err != nil {
		reader.Close()
		return fmt.Errorf("close server bootstrap writer: %w", err)
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		reader.Close()
		return fmt.Errorf("open null device: %w", err)
	}
	defer null.Close()
	command := exec.Command(executable,
		"serve",
		"--control", arguments.control,
		"--listen", arguments.listen,
		"--workspace", arguments.workspace,
	)
	command.Stdin = reader
	command.Stdout = null
	command.Stderr = null
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		reader.Close()
		return fmt.Errorf("start detached SSH server: %w", err)
	}
	reader.Close()
	if err := command.Process.Release(); err != nil {
		return fmt.Errorf("release detached SSH server: %w", err)
	}
	return nil
}

func runServer(arguments serverArguments, input io.Reader) error {
	token, err := readBounded(input, controlTokenBytes)
	if err != nil {
		return fmt.Errorf("read control token: %w", err)
	}
	if len(token) != controlTokenBytes {
		return fmt.Errorf("control token has %d bytes, want %d", len(token), controlTokenBytes)
	}
	control, err := net.DialTimeout("unix", arguments.control, lockedConnectTimeout)
	if err != nil {
		return fmt.Errorf("connect private control socket: %w", err)
	}
	defer control.Close()
	if err := writeControlFrame(control, controlHello{Magic: controlMagic, Token: token}); err != nil {
		return fmt.Errorf("send control hello: %w", err)
	}
	var bootstrap controlBootstrap
	if err := readControlFrame(control, &bootstrap); err != nil {
		return fmt.Errorf("read control bootstrap: %w", err)
	}
	if bootstrap.Username != lockedUsername || bootstrap.Listen != arguments.listen || bootstrap.Workspace != arguments.workspace {
		return errors.New("control bootstrap does not match locked server arguments")
	}
	hostSigner, err := ssh.ParsePrivateKey(bootstrap.HostPrivateKey)
	if err != nil || hostSigner.PublicKey().Type() != ssh.KeyAlgoED25519 {
		return errors.New("control bootstrap host key is not Ed25519")
	}
	authorized, comment, options, rest, err := ssh.ParseAuthorizedKey(bootstrap.AuthorizedKey)
	if err != nil || len(rest) != 0 || len(options) != 0 || comment != "" || authorized.Type() != ssh.KeyAlgoED25519 {
		return errors.New("control bootstrap authorized key is not one canonical Ed25519 key")
	}
	listener, err := net.Listen("tcp4", arguments.listen)
	if err != nil {
		return fmt.Errorf("listen for SSH: %w", err)
	}
	serverConfig := newSSHServerConfig(hostSigner, authorized)
	if err := writeControlFrame(control, controlReady{Address: listener.Addr().String()}); err != nil {
		listener.Close()
		return fmt.Errorf("report SSH readiness: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveSSH(ctx, listener, serverConfig, arguments.workspace) }()
	_, controlErr := io.CopyN(io.Discard, control, 1)
	cancel()
	_ = listener.Close()
	shutdown := time.NewTimer(serverShutdownTimeout)
	defer shutdown.Stop()
	select {
	case serveErr := <-serveDone:
		if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
			return serveErr
		}
	case <-shutdown.C:
		return errors.New("SSH server did not stop within the shutdown deadline")
	}
	if controlErr != nil && !errors.Is(controlErr, io.EOF) && !errors.Is(controlErr, net.ErrClosed) {
		return fmt.Errorf("read control shutdown: %w", controlErr)
	}
	return nil
}

func newSSHServerConfig(hostSigner ssh.Signer, authorized ssh.PublicKey) *ssh.ServerConfig {
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

func serveSSH(ctx context.Context, listener net.Listener, configuration *ssh.ServerConfig, workspace string) error {
	var wait sync.WaitGroup
	defer wait.Wait()
	connections := make(chan struct{}, maxConcurrentConnections)
	for {
		connection, err := listener.Accept()
		if err != nil {
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			_ = connection.Close()
			continue
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer func() { <-connections }()
			closed := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					_ = connection.Close()
				case <-closed:
				}
			}()
			serveSSHConnection(ctx, connection, configuration, workspace)
			close(closed)
		}()
	}
}

func serveSSHConnection(ctx context.Context, connection net.Conn, configuration *ssh.ServerConfig, workspace string) {
	serveSSHConnectionWithTimeout(ctx, connection, configuration, workspace, serverHandshakeTimeout)
}

func serveSSHConnectionWithTimeout(ctx context.Context, connection net.Conn, configuration *ssh.ServerConfig, workspace string, handshakeTimeout time.Duration) {
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	serverConnection, channels, requests, err := ssh.NewServerConn(connection, configuration)
	if err != nil {
		return
	}
	defer serverConnection.Close()
	if err := connection.SetDeadline(time.Time{}); err != nil {
		return
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	globalLimit := make(chan struct{}, 1)
	go rejectGlobalRequests(requests, globalLimit)
	var sessionDone <-chan struct{}
	sessionOpened := false
	for {
		select {
		case <-connectionCtx.Done():
			return
		case <-globalLimit:
			return
		case <-sessionDone:
			return
		case channel, ok := <-channels:
			if !ok {
				return
			}
			if channel.ChannelType() != "session" || sessionOpened {
				_ = channel.Reject(ssh.Prohibited, "only one session channel is allowed")
				return
			}
			stream, channelRequests, err := channel.Accept()
			if err != nil {
				return
			}
			sessionOpened = true
			done := make(chan struct{})
			sessionDone = done
			go func() {
				handleSession(connectionCtx, stream, channelRequests, workspace)
				close(done)
			}()
		}
	}
}

func rejectGlobalRequests(requests <-chan *ssh.Request, limit chan<- struct{}) {
	count := 0
	for request := range requests {
		count++
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
		if count >= maxGlobalRequests {
			select {
			case limit <- struct{}{}:
			default:
			}
			return
		}
	}
}

func handleSession(parent context.Context, channel ssh.Channel, requests <-chan *ssh.Request, workspace string) {
	defer channel.Close()
	var request *ssh.Request
	select {
	case <-parent.Done():
		return
	case received, ok := <-requests:
		if !ok {
			return
		}
		request = received
	}
	if request.Type != "exec" {
		if request.WantReply {
			_ = request.Reply(false, nil)
		}
		return
	}
	var payload struct{ Command string }
	if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
		_ = request.Reply(false, nil)
		return
	}
	remote, err := decodeRemoteCommand(payload.Command)
	if err != nil {
		_ = request.Reply(false, nil)
		return
	}
	if err := request.Reply(true, nil); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, serverCommandTimeout)
	defer cancel()
	exitCode := make(chan int, 1)
	go func() { exitCode <- runRemoteProcess(ctx, remote, workspace, channel, channel, channel.Stderr()) }()
	for {
		select {
		case status := <-exitCode:
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: uint32(status)}))
			return
		case repeated, ok := <-requests:
			if ok && repeated.WantReply {
				_ = repeated.Reply(false, nil)
			}
			cancel()
			<-exitCode
			return
		case <-parent.Done():
			cancel()
			<-exitCode
			return
		}
	}
}

func runRemoteProcess(ctx context.Context, remote remoteCommand, workspace string, stdin io.Reader, stdout, stderr io.Writer) int {
	tokens := remote.argv
	shellIndex := 0
	assignments := make(map[string]string)
	if tokens[0] == remoteEnv {
		shellIndex = 1
		for tokens[shellIndex] != remoteShell {
			name, value, _ := strings.Cut(tokens[shellIndex], "=")
			assignments[name] = value
			shellIndex++
		}
	}
	commandEnvironment := make([]string, 0, len(os.Environ())+len(assignments))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, replaced := assignments[name]; !replaced {
			commandEnvironment = append(commandEnvironment, value)
		}
	}
	if shellIndex > 0 {
		for _, token := range tokens[1:shellIndex] {
			commandEnvironment = append(commandEnvironment, token)
		}
	}
	command := exec.Command(tokens[shellIndex], tokens[shellIndex+1:]...)
	command.Dir = workspace
	command.Env = commandEnvironment
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdinPipe, err := command.StdinPipe()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh-server: create command stdin: %v\n", err)
		return 125
	}
	if err := command.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "aries-ssh-server: start command: %v\n", err)
		return 127
	}
	go func() {
		_, _ = io.Copy(stdinPipe, stdin)
		_ = stdinPipe.Close()
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		_ = stdinPipe.Close()
		if err == nil {
			return 0
		}
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			status := exitError.ExitCode()
			if status >= 0 && status <= 255 {
				return status
			}
		}
		return 125
	case <-ctx.Done():
		_ = stdinPipe.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-wait
		return 124
	}
}

func prepareWorkspace(workdir, workspaceRoot, runtimeID string, ownerToken []byte) error {
	return prepareWorkspaceWithRename(workdir, workspaceRoot, runtimeID, ownerToken, os.Rename)
}

func prepareWorkspaceWithRename(workdir, workspaceRoot, runtimeID string, ownerToken []byte, rename func(string, string) error) error {
	workspace, runtimeRoot, err := validateWorkspacePaths(workdir, workspaceRoot, runtimeID)
	if err != nil {
		return err
	}
	if len(ownerToken) != workspaceOwnerTokenBytes {
		return errors.New("workspace ownership token has an invalid length")
	}
	if err := validateWorkspaceContainment(workdir, workspaceRoot, true); err != nil {
		return err
	}
	if err := ensureRealDirectoryChain(filepath.Dir(workspaceRoot)); err != nil {
		return fmt.Errorf("prepare workspace parent chain: %w", err)
	}
	if err := os.Mkdir(workspaceRoot, 0o700); errors.Is(err, os.ErrExist) {
		return workspaceRootExistsError{}
	} else if err != nil {
		return fmt.Errorf("atomically acquire workspace root: %w", err)
	}
	if err := writeWorkspaceOwnerMarker(workspaceRoot, ownerToken); err != nil {
		removeRootErr := os.Remove(workspaceRoot)
		return errors.Join(fmt.Errorf("create workspace ownership marker: %w", err), wrapIfError("remove empty unmarked workspace root", removeRootErr))
	}
	if err := validateWorkspaceContainment(workdir, workspaceRoot, true); err != nil {
		return err
	}
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		return fmt.Errorf("create OpenClaw runtime root: %w", err)
	}
	if err := rename(workdir, workspace); errors.Is(err, syscall.EXDEV) {
		if err := os.Symlink(workdir, workspace); err != nil {
			return fmt.Errorf("alias cross-device task workdir into OpenClaw runtime: %w", err)
		}
		if err := verifyWorkspaceIdentity(workdir, workspace); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("move task workdir into OpenClaw runtime: %w", err)
	}
	if err := validateWorkspaceContainment(workdir, workspaceRoot, false); err != nil {
		return err
	}
	if err := os.Symlink(workspace, workdir); err != nil {
		return fmt.Errorf("alias original task workdir: %w", err)
	}
	if err := verifyWorkspaceIdentity(workdir, workspace); err != nil {
		return err
	}
	return nil
}

func ensureRealDirectoryChain(path string) error {
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator)) {
		current = filepath.Join(current, component)
		if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace parent component %q is not one real directory", current)
		}
	}
	return nil
}

func writeWorkspaceOwnerMarker(workspaceRoot string, ownerToken []byte) error {
	path := filepath.Join(workspaceRoot, workspaceOwnerMarker)
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(ownerToken); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}

func validateWorkspacePaths(workdir, workspaceRoot, runtimeID string) (string, string, error) {
	for name, value := range map[string]string{"workdir": workdir, "workspace root": workspaceRoot} {
		if value == "" || strings.ContainsRune(value, 0) || !filepath.IsAbs(value) || filepath.Clean(value) != value || value == "/" {
			return "", "", fmt.Errorf("%s must be an absolute clean non-root path", name)
		}
	}
	if runtimeID != lockedRuntimeID {
		return "", "", errors.New("runtime ID does not match the pinned OpenClaw shared-session formula")
	}
	if pathWithin(workdir, workspaceRoot) || pathWithin(workspaceRoot, workdir) {
		return "", "", errors.New("task workdir and OpenClaw workspace root must be disjoint")
	}
	runtimeRoot := filepath.Join(workspaceRoot, runtimeID)
	return filepath.Join(runtimeRoot, "workspace"), runtimeRoot, nil
}

func validateWorkspaceContainment(workdir, workspaceRoot string, workdirMustExist bool) error {
	if err := requireRealAncestors(workdir, workdirMustExist); err != nil {
		return fmt.Errorf("validate task workdir ancestry: %w", err)
	}
	if err := requireRealAncestors(workspaceRoot, false); err != nil {
		return fmt.Errorf("validate workspace-root ancestry: %w", err)
	}
	resolvedWorkdir, err := resolveExistingOrFuturePath(workdir)
	if err != nil {
		return fmt.Errorf("resolve task workdir: %w", err)
	}
	resolvedRoot, err := resolveExistingOrFuturePath(workspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	if pathWithin(resolvedWorkdir, resolvedRoot) || pathWithin(resolvedRoot, resolvedWorkdir) {
		return errors.New("resolved task workdir and workspace root must be disjoint")
	}
	return nil
}

func requireRealAncestors(path string, leafMustExist bool) error {
	components := strings.Split(strings.TrimPrefix(filepath.Clean(path), string(filepath.Separator)), string(filepath.Separator))
	current := string(filepath.Separator)
	for _, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if leafMustExist {
				return fmt.Errorf("required path component %q is absent", current)
			}
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("path component %q is not one real directory", current)
		}
	}
	return nil
}

func resolveExistingOrFuturePath(path string) (string, error) {
	missing := make([]string, 0, 4)
	current := filepath.Clean(path)
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("no existing path ancestor")
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func wrapIfError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func verifyWorkspaceAlias(workdir, workspace string) error {
	linkInfo, err := os.Lstat(workdir)
	if err != nil || linkInfo.Mode()&os.ModeSymlink == 0 {
		return errors.New("original workdir is not a symbolic link")
	}
	target, err := os.Readlink(workdir)
	if err != nil || target != workspace {
		return errors.New("original workdir does not point to the exact runtime workspace")
	}
	originalInfo, err := os.Stat(workdir)
	if err != nil {
		return err
	}
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		return err
	}
	if !originalInfo.IsDir() || !os.SameFile(originalInfo, workspaceInfo) {
		return errors.New("original workdir and runtime workspace are not the same directory")
	}
	return nil
}

func verifyWorkspaceIdentity(workdir, workspace string) error {
	workInfo, workErr := os.Lstat(workdir)
	workspaceInfo, workspaceErr := os.Lstat(workspace)
	if workErr != nil || workspaceErr != nil {
		return errors.New("OpenClaw workspace identity path is missing")
	}
	workLink := workInfo.Mode()&os.ModeSymlink != 0
	workspaceLink := workspaceInfo.Mode()&os.ModeSymlink != 0
	if workLink == workspaceLink {
		return errors.New("OpenClaw workspace identity requires exactly one symbolic link")
	}
	alias, target := workdir, workspace
	if workspaceLink {
		alias, target = workspace, workdir
	}
	linked, err := os.Readlink(alias)
	if err != nil || linked != target {
		return errors.New("OpenClaw workspace alias has an unexpected target")
	}
	resolvedAlias, err := os.Stat(alias)
	if err != nil || !resolvedAlias.IsDir() {
		return errors.New("OpenClaw workspace alias does not resolve to a directory")
	}
	resolvedTarget, err := os.Stat(target)
	if err != nil || !resolvedTarget.IsDir() || !os.SameFile(resolvedAlias, resolvedTarget) {
		return errors.New("OpenClaw and task workspaces are not the same directory")
	}
	return nil
}

func pathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && (relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func writeControlFrame(writer io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > maxControlFrame {
		return errors.New("control frame is too large")
	}
	if err := binary.Write(writer, binary.BigEndian, uint32(len(encoded))); err != nil {
		return err
	}
	_, err = writer.Write(encoded)
	return err
}

func readControlFrame(reader io.Reader, value any) error {
	var size uint32
	if err := binary.Read(reader, binary.BigEndian, &size); err != nil {
		return err
	}
	if size == 0 || size > maxControlFrame {
		return errors.New("control frame has an invalid size")
	}
	content := make([]byte, int(size))
	if _, err := io.ReadFull(reader, content); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("control frame contains trailing JSON")
		}
		return err
	}
	return nil
}

func readBounded(reader io.Reader, exact int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(reader, exact+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > exact {
		return nil, errors.New("input exceeds its exact bound")
	}
	return content, nil
}
