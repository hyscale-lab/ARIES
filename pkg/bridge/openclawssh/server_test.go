package openclawssh

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type testSSHServer struct {
	hostKey       ssh.PublicKey
	clientKey     ssh.Signer
	configuration *ssh.ServerConfig
	workspace     string
	ctx           context.Context
	cancel        context.CancelFunc
	wait          sync.WaitGroup
}

type memoryPipeState struct {
	mu     sync.Mutex
	ready  *sync.Cond
	buffer bytes.Buffer
	closed bool
}

type memoryConn struct {
	read, write *memoryPipeState
}

type memoryAddress string

type queuedListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func newQueuedListener() *queuedListener {
	return &queuedListener{connections: make(chan net.Conn, maxConcurrentConnections+2), closed: make(chan struct{})}
}

func (listener *queuedListener) Accept() (net.Conn, error) {
	select {
	case connection := <-listener.connections:
		return connection, nil
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}

func (listener *queuedListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}

func (*queuedListener) Addr() net.Addr { return memoryAddress("queued") }

func newMemoryPipe() (net.Conn, net.Conn) {
	leftToRight := &memoryPipeState{}
	rightToLeft := &memoryPipeState{}
	leftToRight.ready = sync.NewCond(&leftToRight.mu)
	rightToLeft.ready = sync.NewCond(&rightToLeft.mu)
	return &memoryConn{read: rightToLeft, write: leftToRight}, &memoryConn{read: leftToRight, write: rightToLeft}
}

func (connection *memoryConn) Read(content []byte) (int, error) {
	connection.read.mu.Lock()
	defer connection.read.mu.Unlock()
	for connection.read.buffer.Len() == 0 && !connection.read.closed {
		connection.read.ready.Wait()
	}
	if connection.read.buffer.Len() == 0 {
		return 0, io.EOF
	}
	return connection.read.buffer.Read(content)
}

func (connection *memoryConn) Write(content []byte) (int, error) {
	connection.write.mu.Lock()
	defer connection.write.mu.Unlock()
	if connection.write.closed {
		return 0, net.ErrClosed
	}
	written, err := connection.write.buffer.Write(content)
	connection.write.ready.Broadcast()
	return written, err
}

func (connection *memoryConn) Close() error {
	for _, state := range []*memoryPipeState{connection.read, connection.write} {
		state.mu.Lock()
		state.closed = true
		state.ready.Broadcast()
		state.mu.Unlock()
	}
	return nil
}

func (*memoryConn) LocalAddr() net.Addr              { return memoryAddress("local") }
func (*memoryConn) RemoteAddr() net.Addr             { return memoryAddress("remote") }
func (*memoryConn) SetDeadline(time.Time) error      { return nil }
func (*memoryConn) SetReadDeadline(time.Time) error  { return nil }
func (*memoryConn) SetWriteDeadline(time.Time) error { return nil }
func (address memoryAddress) Network() string        { return "memory" }
func (address memoryAddress) String() string         { return string(address) }

func startTestSSHServer(t *testing.T, workspace string) *testSSHServer {
	t.Helper()
	hostPEM, _, hostKey, clientSigner, authorizedBytes, err := generateSessionKeys()
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.ParsePrivateKey(hostPEM)
	if err != nil {
		t.Fatal(err)
	}
	authorized, _, _, _, err := ssh.ParseAuthorizedKey(authorizedBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	server := &testSSHServer{
		hostKey: hostKey, clientKey: clientSigner,
		configuration: newSSHServerConfig(hostSigner, authorized),
		workspace:     workspace, ctx: ctx, cancel: cancel,
	}
	t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() {
			server.wait.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("test SSH server did not stop")
		}
	})
	return server
}

func (server *testSSHServer) connect(t *testing.T) *ssh.Client {
	t.Helper()
	configuration := &ssh.ClientConfig{
		User: lockedUsername,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(server.clientKey)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if !bytes.Equal(key.Marshal(), server.hostKey.Marshal()) {
				return errors.New("host key mismatch")
			}
			return nil
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           2 * time.Second,
	}
	client, err := server.dial(configuration)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (server *testSSHServer) dial(configuration *ssh.ClientConfig) (*ssh.Client, error) {
	clientSide, serverSide := newMemoryPipe()
	server.wait.Add(1)
	go func() {
		defer server.wait.Done()
		serveSSHConnection(server.ctx, serverSide, server.configuration, server.workspace)
	}()
	connection, channels, requests, err := ssh.NewClientConn(clientSide, "pipe", configuration)
	if err != nil {
		_ = clientSide.Close()
		return nil, err
	}
	return ssh.NewClient(connection, channels, requests), nil
}

func TestSSHServerPreservesStreamsStdinAndExitStatus(t *testing.T) {
	workspace := t.TempDir()
	server := startTestSSHServer(t, workspace)
	client := server.connect(t)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader("stdin-bytes")
	session.Stdout = &stdout
	session.Stderr = &stderr
	remote := encodeCanonicalTokens([]string{remoteEnv, "LANG=ok", remoteShell, "-c", "cat; printf %s \"$LANG\"; printf err >&2; exit 7"})
	err = session.Run(remote)
	var exitError *ssh.ExitError
	if !errors.As(err, &exitError) || exitError.ExitStatus() != 7 {
		t.Fatalf("Run() error = %T %#v", err, err)
	}
	if stdout.String() != "stdin-bytesok" || stderr.String() != "err" {
		t.Fatalf("streams = stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestSSHServerReportsZeroExitWithoutStdin(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	client := server.connect(t)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Run(encodeCanonicalTokens([]string{remoteShell, "-c", "true"})); err != nil {
		t.Fatalf("zero-exit command error = %T %v", err, err)
	}
}

type controlledBlockingReader struct{ done <-chan struct{} }

func (reader controlledBlockingReader) Read([]byte) (int, error) {
	<-reader.done
	return 0, io.EOF
}

func TestRemoteProcessExitDoesNotWaitForAnUnclosedSSHInputStream(t *testing.T) {
	remote, err := decodeRemoteCommand(encodeCanonicalTokens([]string{remoteShell, "-c", "true"}))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	result := make(chan int, 1)
	workspace := t.TempDir()
	go func() {
		result <- runRemoteProcess(context.Background(), remote, workspace, controlledBlockingReader{done: done}, io.Discard, io.Discard)
	}()
	select {
	case exitCode := <-result:
		close(done)
		if exitCode != 0 {
			t.Fatalf("exit code = %d", exitCode)
		}
	case <-time.After(time.Second):
		close(done)
		t.Fatal("command exit waited for an unclosed SSH input stream")
	}
}

func TestSSHServerRejectsWrongAuthenticationAndHostKey(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	wrongPublic, wrongPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = wrongPublic
	wrongSigner, err := ssh.NewSignerFromKey(wrongPrivate)
	if err != nil {
		t.Fatal(err)
	}
	tests := []ssh.ClientConfig{
		{User: lockedUsername, Auth: []ssh.AuthMethod{ssh.Password("bad")}, HostKeyCallback: ssh.InsecureIgnoreHostKey()},
		{User: lockedUsername, Auth: []ssh.AuthMethod{ssh.PublicKeys(wrongSigner)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()},
		{User: "other", Auth: []ssh.AuthMethod{ssh.PublicKeys(server.clientKey)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()},
		{User: lockedUsername, Auth: []ssh.AuthMethod{ssh.PublicKeys(server.clientKey)}, HostKeyCallback: func(string, net.Addr, ssh.PublicKey) error { return errors.New("mismatch") }},
	}
	for index := range tests {
		tests[index].Timeout = time.Second
		client, err := server.dial(&tests[index])
		if err == nil {
			client.Close()
			t.Errorf("authentication case %d unexpectedly connected", index)
		}
	}
}

func TestSSHServerRejectsEveryNonExecRequestAndForwardingChannel(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())

	for _, requestType := range []string{
		"pty-req", "env", "shell", "subsystem", "auth-agent-req@openssh.com", "x11-req", "signal",
	} {
		client := server.connect(t)
		channel, requests, err := client.OpenChannel("session", nil)
		if err != nil {
			t.Fatal(err)
		}
		go ssh.DiscardRequests(requests)
		accepted, err := channel.SendRequest(requestType, true, nil)
		_ = channel.Close()
		_ = client.Close()
		if err != nil {
			t.Fatalf("request %q transport error = %v", requestType, err)
		}
		if accepted {
			t.Errorf("request %q was accepted", requestType)
		}
	}
	for _, channelType := range []string{"direct-tcpip", "forwarded-tcpip", "auth-agent@openssh.com", "x11"} {
		client := server.connect(t)
		channel, _, err := client.OpenChannel(channelType, nil)
		if err == nil {
			channel.Close()
			t.Errorf("channel %q was accepted", channelType)
		}
		_ = client.Close()
	}
	for _, requestType := range []string{"tcpip-forward", "cancel-tcpip-forward", "keepalive@openssh.com"} {
		client := server.connect(t)
		accepted, _, err := client.SendRequest(requestType, true, nil)
		if err != nil {
			t.Fatalf("global request %q transport error = %v", requestType, err)
		}
		if accepted {
			t.Errorf("global request %q was accepted", requestType)
		}
		_ = client.Close()
	}
}

func TestSSHServerRejectsMalformedAndSecretExecBeforeProcessStart(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	for _, command := range []string{
		"/bin/sh -c true",
		encodeCanonicalTokens([]string{remoteEnv, "DEEPSEEK_API_KEY=secret", remoteShell, "-c", "true"}),
		encodeCanonicalTokens([]string{"/bin/bash", "-c", "true"}),
	} {
		client := server.connect(t)
		channel, requests, err := client.OpenChannel("session", nil)
		if err != nil {
			t.Fatal(err)
		}
		go ssh.DiscardRequests(requests)
		accepted, err := channel.SendRequest("exec", true, ssh.Marshal(struct{ Command string }{command}))
		_ = channel.Close()
		_ = client.Close()
		if err != nil {
			t.Fatalf("malformed exec transport error = %v", err)
		}
		if accepted {
			t.Errorf("malformed exec %q was accepted", command)
		}
	}
}

func testWorkspaceOwnerToken() []byte {
	return bytes.Repeat([]byte{0x5a}, workspaceOwnerTokenBytes)
}

func TestWorkspaceAliasPreservesIdentityAndRecordsOwnership(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "task-workdir")
	workspaceRoot := filepath.Join(base, "openclaw")
	ownerToken := testWorkspaceOwnerToken()
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "state"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(workdir, workspaceRoot, lockedRuntimeID, ownerToken); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(workspaceRoot, lockedRuntimeID, "workspace")
	if err := verifyWorkspaceAlias(workdir, workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "state"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workdir, "state"))
	if err != nil || string(content) != "after" {
		t.Fatalf("original path bytes = %q, %v", content, err)
	}
	marker := filepath.Join(workspaceRoot, workspaceOwnerMarker)
	markerInfo, err := os.Lstat(marker)
	if err != nil || !markerInfo.Mode().IsRegular() || markerInfo.Mode().Perm() != 0o600 {
		t.Fatalf("ownership marker = %v, %v", markerInfo, err)
	}
	markerContent, err := os.ReadFile(marker)
	if err != nil || !bytes.Equal(markerContent, ownerToken) {
		t.Fatalf("ownership marker content = %x, %v", markerContent, err)
	}
}

func TestWorkspacePreparationFallsBackToReverseAliasOnCrossDeviceRename(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "task-workdir")
	workspaceRoot := filepath.Join(base, "openclaw")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "state"), []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	rename := func(string, string) error { return syscall.EXDEV }
	if err := prepareWorkspaceWithRename(workdir, workspaceRoot, lockedRuntimeID, testWorkspaceOwnerToken(), rename); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(workspaceRoot, lockedRuntimeID, "workspace")
	workInfo, err := os.Lstat(workdir)
	if err != nil || !workInfo.IsDir() || workInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("cross-device workdir = %v, %v", workInfo, err)
	}
	workspaceInfo, err := os.Lstat(workspace)
	if err != nil || workspaceInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cross-device workspace = %v, %v", workspaceInfo, err)
	}
	if err := verifyWorkspaceIdentity(workdir, workspace); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspacePreparationFailsOnIncompatibleExistingPaths(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	root := filepath.Join(base, "root")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(workdir, root, lockedRuntimeID, testWorkspaceOwnerToken()); err == nil {
		t.Fatal("existing workspace root was accepted")
	}
	if err := prepareWorkspace(workdir, filepath.Join(base, "other"), "changed-runtime", testWorkspaceOwnerToken()); err == nil {
		t.Fatal("changed runtime ID was accepted")
	}
}

func TestPrepareReportsDistinctPreexistingRootWithoutChangingIt(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	root := filepath.Join(base, "root")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := ServerMain([]string{"prepare", "--workdir", workdir, "--workspace-root", root, "--runtime-id", lockedRuntimeID}, bytes.NewReader(testWorkspaceOwnerToken()), io.Discard, &stderr)
	if code != workspaceRootExistsExit {
		t.Fatalf("prepare exit = %d stderr %q", code, stderr.String())
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		t.Fatalf("pre-existing root changed: %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(root, workspaceOwnerMarker)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign root received an ownership marker: %v", err)
	}
}

func TestPrepareRejectsMissingOwnershipTokenBeforeCreatingWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	workdir := filepath.Join(base, "work")
	root := filepath.Join(base, "root")
	if err := os.Mkdir(workdir, 0o700); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := ServerMain([]string{"prepare", "--workdir", workdir, "--workspace-root", root, "--runtime-id", lockedRuntimeID}, strings.NewReader(""), io.Discard, &stderr)
	if code != 125 || !strings.Contains(stderr.String(), "ownership token") {
		t.Fatalf("prepare exit = %d stderr %q", code, stderr.String())
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tokenless prepare created workspace root: %v", err)
	}
}

func TestWorkspaceOwnerMarkerNeverOverwritesForeignEntry(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, workspaceOwnerMarker)
	foreign := bytes.Repeat([]byte{0x33}, workspaceOwnerTokenBytes)
	if err := os.WriteFile(marker, foreign, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceOwnerMarker(root, testWorkspaceOwnerToken()); err == nil {
		t.Fatal("foreign ownership marker was accepted")
	}
	content, err := os.ReadFile(marker)
	if err != nil || !bytes.Equal(content, foreign) {
		t.Fatalf("foreign ownership marker changed: %x, %v", content, err)
	}
}

func TestWorkspacePreparationRejectsSymlinkedAncestorsAndResolvedAliases(t *testing.T) {
	t.Run("workdir ancestor", func(t *testing.T) {
		base := t.TempDir()
		realParent := filepath.Join(base, "real")
		aliasParent := filepath.Join(base, "alias")
		if err := os.MkdirAll(filepath.Join(realParent, "work"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, aliasParent); err != nil {
			t.Fatal(err)
		}
		if err := prepareWorkspace(filepath.Join(aliasParent, "work"), filepath.Join(base, "root"), lockedRuntimeID, testWorkspaceOwnerToken()); err == nil {
			t.Fatal("symlinked workdir ancestor was accepted")
		}
	})
	t.Run("workspace ancestor", func(t *testing.T) {
		base := t.TempDir()
		workdir := filepath.Join(base, "work")
		realParent := filepath.Join(base, "real")
		aliasParent := filepath.Join(base, "alias")
		if err := os.Mkdir(workdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(realParent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(realParent, aliasParent); err != nil {
			t.Fatal(err)
		}
		if err := prepareWorkspace(workdir, filepath.Join(aliasParent, "root"), lockedRuntimeID, testWorkspaceOwnerToken()); err == nil {
			t.Fatal("symlinked workspace ancestor was accepted")
		}
	})
	t.Run("resolved containment", func(t *testing.T) {
		base := t.TempDir()
		workdir := filepath.Join(base, "work")
		if err := os.Mkdir(workdir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := prepareWorkspace(workdir, filepath.Join(workdir, "future"), lockedRuntimeID, testWorkspaceOwnerToken()); err == nil {
			t.Fatal("workspace root lexically contained by workdir was accepted")
		}
	})
}

func TestSSHServerHandshakeDeadlineClosesSlowPeer(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	clientSide, serverSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		serveSSHConnectionWithTimeout(server.ctx, serverSide, server.configuration, server.workspace, 20*time.Millisecond)
		close(done)
	}()
	defer clientSide.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow pre-auth peer survived the handshake deadline")
	}
}

func TestSSHServerAllowsExactlyOneSessionChannelAndExecRequest(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	client := server.connect(t)
	defer client.Close()
	first, firstRequests, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(firstRequests)
	if second, _, err := client.OpenChannel("session", nil); err == nil {
		second.Close()
		t.Fatal("second session channel was accepted")
	}
	_ = first.Close()

	client = server.connect(t)
	defer client.Close()
	channel, requests, err := client.OpenChannel("session", nil)
	if err != nil {
		t.Fatal(err)
	}
	go ssh.DiscardRequests(requests)
	command := ssh.Marshal(struct{ Command string }{encodeCanonicalTokens([]string{remoteShell, "-c", "sleep 30"})})
	accepted, err := channel.SendRequest("exec", true, command)
	if err != nil || !accepted {
		t.Fatalf("first exec = %v, %v", accepted, err)
	}
	accepted, err = channel.SendRequest("exec", true, command)
	if err == nil && accepted {
		t.Fatal("second exec request was accepted")
	}
	_ = channel.Close()
}

func TestSSHServerCancellationStopsCommandAndReleasesGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	workspace := t.TempDir()
	server := startTestSSHServer(t, workspace)
	client := server.connect(t)
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- session.Run(encodeCanonicalTokens([]string{remoteShell, "-c", "sleep 2; printf leaked > after-cancel"}))
	}()
	time.Sleep(20 * time.Millisecond)
	server.cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server cancellation did not stop the SSH command")
	}
	_ = session.Close()
	_ = client.Close()
	time.Sleep(20 * time.Millisecond)
	if _, err := os.Lstat(filepath.Join(workspace, "after-cancel")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled command wrote after shutdown: %v", err)
	}
	if got := runtime.NumGoroutine(); got > baseline+6 {
		t.Fatalf("goroutines after cancellation = %d, baseline %d", got, baseline)
	}
}

func TestSSHServerClosesConnectionsAtTheConcurrentCap(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	listener := newQueuedListener()
	ctx, cancel := context.WithCancel(server.ctx)
	done := make(chan error, 1)
	go func() { done <- serveSSH(ctx, listener, server.configuration, server.workspace) }()
	clients := make([]net.Conn, 0, maxConcurrentConnections+1)
	for range maxConcurrentConnections + 1 {
		client, peer := net.Pipe()
		clients = append(clients, client)
		listener.connections <- peer
	}
	last := clients[len(clients)-1]
	_ = last.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := last.Read(one[:]); !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("saturated connection was not closed: %v", err)
	}
	cancel()
	_ = listener.Close()
	for _, client := range clients {
		_ = client.Close()
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded server did not release accepted connections")
	}
}

func TestSSHServerBoundsGlobalRequestsAbovePinnedKeepaliveNeeds(t *testing.T) {
	server := startTestSSHServer(t, t.TempDir())
	client := server.connect(t)
	defer client.Close()
	for index := 0; index < maxGlobalRequests; index++ {
		accepted, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		if err != nil {
			t.Fatalf("global request %d failed before the cap: %v", index, err)
		}
		if accepted {
			t.Fatalf("global request %d was accepted", index)
		}
	}
	if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err == nil {
		t.Fatal("connection remained open beyond the global-request cap")
	}
}

func TestControlFramesRejectUnknownAndTrailingJSON(t *testing.T) {
	var buffer bytes.Buffer
	if err := writeControlFrame(&buffer, map[string]any{"magic": controlMagic, "token": []byte("token"), "extra": true}); err != nil {
		t.Fatal(err)
	}
	var hello controlHello
	if err := readControlFrame(&buffer, &hello); err == nil {
		t.Fatal("unknown control field was accepted")
	}

	buffer.Reset()
	content := []byte(`{"magic":"x","token":"eA=="} {}`)
	if err := writeRawFrame(&buffer, content); err != nil {
		t.Fatal(err)
	}
	if err := readControlFrame(&buffer, &hello); err == nil {
		t.Fatal("trailing control JSON was accepted")
	}
}

func writeRawFrame(writer io.Writer, content []byte) error {
	if err := binaryWriteUint32(writer, uint32(len(content))); err != nil {
		return err
	}
	_, err := writer.Write(content)
	return err
}

func binaryWriteUint32(writer io.Writer, value uint32) error {
	var content [4]byte
	content[0] = byte(value >> 24)
	content[1] = byte(value >> 16)
	content[2] = byte(value >> 8)
	content[3] = byte(value)
	_, err := writer.Write(content[:])
	return err
}
