package sshbridge

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestServerRevokeBeforeStartClosesOwnedListener(t *testing.T) {
	var absent *Server
	absent.Revoke()
	if err := absent.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	server, config := newServerFixture(t, nil)
	server.Revoke()
	server.Start(func(context.Context, ssh.Channel, <-chan *ssh.Request) { t.Error("revoked server admitted a handler") }, nil)
	server.Revoke()
	if err := server.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if connection, err := net.DialTimeout("tcp", server.Addr().String(), config.Timeout); err == nil {
		_ = connection.Close()
		t.Fatal("revoked listener still accepts connections")
	}
}

func TestServerClosesConnectionAcceptedDuringRevocation(t *testing.T) {
	accepted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server, _ := newServerFixture(t, func(listener net.Listener) net.Listener {
		return &pausedAcceptListener{Listener: listener, accepted: accepted, release: release}
	})
	server.Start(func(context.Context, ssh.Channel, <-chan *ssh.Request) { t.Error("revoked server admitted a handler") }, nil)
	connection, err := net.DialTimeout("tcp", server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	awaitServerSignal(t, accepted, "listener did not accept connection")
	server.Revoke()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := server.Wait(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait before accept returned = %v", err)
	}
	unblock()
	waitForServer(t, server)
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Read(make([]byte, 1)); err == nil {
		t.Fatal("untracked accepted connection survived revocation")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("revocation left the accepted connection open")
	}
}

func TestServerRequiresAriesAndExactPublicKey(t *testing.T) {
	server, config := newServerFixture(t, nil)
	server.Start(func(context.Context, ssh.Channel, <-chan *ssh.Request) {}, nil)
	for _, test := range []struct {
		name string
		user string
		auth []ssh.AuthMethod
	}{
		{name: "different user", user: "root", auth: config.Auth},
		{name: "different key", user: "aries", auth: []ssh.AuthMethod{ssh.PublicKeys(clientSigner(t))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := *config
			invalid.User, invalid.Auth = test.user, test.auth
			if client, err := ssh.Dial("tcp", server.Addr().String(), &invalid); err == nil {
				_ = client.Close()
				t.Fatal("server accepted incorrect identity")
			}
		})
	}
	client := dialServer(t, server, config)
	_ = client.Close()
}

func TestServerKeepsSessionAndGlobalRequestBoundaries(t *testing.T) {
	server, config := newServerFixture(t, nil)
	forwarded := make(chan *ssh.Request, 2)
	server.Start(func(_ context.Context, _ ssh.Channel, requests <-chan *ssh.Request) {
		request, ok := <-requests
		if !ok {
			return
		}
		forwarded <- request
		_ = request.Reply(true, nil)
	}, nil)
	client := dialServer(t, server, config)
	for _, request := range []struct {
		name    string
		payload []byte
		want    bool
	}{
		{name: "keepalive@openssh.com", want: true},
		{name: "keepalive@openssh.com", payload: []byte("unexpected")},
		{name: "unknown@aries"},
	} {
		if ok, _, err := client.SendRequest(request.name, true, request.payload); err != nil || ok != request.want {
			t.Fatalf("global request %q = (%v, %v), want %v", request.name, ok, err, request.want)
		}
	}
	for _, invalid := range []struct {
		kind string
		data []byte
	}{
		{kind: "direct-tcpip"},
		{kind: "session", data: []byte("unexpected")},
	} {
		if channel, _, err := client.OpenChannel(invalid.kind, invalid.data); err == nil {
			_ = channel.Close()
			t.Fatalf("server accepted channel %q with data %q", invalid.kind, invalid.data)
		}
	}
	// Reuse the connection, and leave request grammar entirely to the adapter.
	for _, kind := range []string{"adapter-specific@aries", "exec"} {
		channel, _, err := client.OpenChannel("session", nil)
		if err != nil {
			t.Fatal(err)
		}
		payload := []byte{0, 255, '\n'}
		if ok, err := channel.SendRequest(kind, true, payload); err != nil || !ok {
			_ = channel.Close()
			t.Fatalf("session request = (%v, %v)", ok, err)
		}
		_ = channel.Close()
		select {
		case request := <-forwarded:
			if request.Type != kind || !request.WantReply || !bytes.Equal(request.Payload, payload) {
				t.Fatalf("adapter received altered request: %#v", request)
			}
		case <-time.After(time.Second):
			t.Fatal("request did not reach adapter")
		}
	}
}

func TestServerDisconnectCancelsHandlerContext(t *testing.T) {
	server, config := newServerFixture(t, nil)
	started, canceled := make(chan struct{}), make(chan struct{})
	server.Start(func(ctx context.Context, _ ssh.Channel, _ <-chan *ssh.Request) {
		close(started)
		<-ctx.Done()
		close(canceled)
	}, nil)
	client := dialServer(t, server, config)
	if _, _, err := client.OpenChannel("session", nil); err != nil {
		t.Fatal(err)
	}
	awaitServerSignal(t, started, "handler did not start")
	_ = client.Close()
	awaitServerSignal(t, canceled, "disconnected handler context remains live")
}

func TestServerWaitRequiresAllCanceledHandlersToFinish(t *testing.T) {
	server, config := newServerFixture(t, nil)
	started, canceled, release := make(chan struct{}, 2), make(chan struct{}, 2), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	server.Start(func(ctx context.Context, _ ssh.Channel, _ <-chan *ssh.Request) {
		started <- struct{}{}
		<-ctx.Done()
		canceled <- struct{}{}
		<-release
	}, nil)
	client := dialServer(t, server, config)
	for range 2 {
		if _, _, err := client.OpenChannel("session", nil); err != nil {
			t.Fatal(err)
		}
		awaitServerSignal(t, started, "handler did not start")
	}
	var revokers sync.WaitGroup
	for range 4 {
		revokers.Add(1)
		go func() { defer revokers.Done(); server.Revoke() }()
	}
	revokers.Wait()
	for range 2 {
		awaitServerSignal(t, canceled, "revocation did not cancel every handler")
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	for range 3 {
		if err := server.Wait(expired); !errors.Is(err, context.Canceled) {
			t.Fatalf("Wait accepted unfinished handlers: %v", err)
		}
	}
	unblock()
	waitForServer(t, server)
	if err := server.Wait(expired); err != nil {
		t.Fatalf("finished server Wait = %v", err)
	}
}

func TestServerBoundsHandshakeAndClearsDeadlineForSessions(t *testing.T) {
	deadlines := make(chan time.Time, 4)
	server, config := newServerFixture(t, func(listener net.Listener) net.Listener {
		return &deadlineListener{Listener: listener, deadlines: deadlines}
	})
	server.Start(func(context.Context, ssh.Channel, <-chan *ssh.Request) {}, nil)
	before := time.Now()
	_ = dialServer(t, server, config)
	for index := range 2 {
		select {
		case deadline := <-deadlines:
			if index == 0 && (deadline.Before(before.Add(4*time.Second)) || deadline.After(time.Now().Add(5*time.Second))) {
				t.Fatalf("handshake deadline = %v, want five-second bound", deadline)
			}
			if index == 1 && !deadline.IsZero() {
				t.Fatalf("handshake deadline leaked into sessions: %v", deadline)
			}
		case <-time.After(time.Second):
			t.Fatal("server did not set and clear handshake deadline")
		}
	}
	stalled, err := net.DialTimeout("tcp", server.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	select {
	case <-deadlines:
	case <-time.After(time.Second):
		t.Fatal("stalled connection was not admitted to handshake")
	}
	server.Revoke()
	waitForServer(t, server)
}

func newServerFixture(t *testing.T, wrap func(net.Listener) net.Listener) (*Server, *ssh.ClientConfig) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if wrap != nil {
		listener = wrap(listener)
	}
	host, identity := clientSigner(t), clientSigner(t)
	server := NewServer(listener, host, identity.PublicKey())
	t.Cleanup(func() { server.Revoke(); waitForServer(t, server) })
	return server, &ssh.ClientConfig{
		User: "aries", Auth: []ssh.AuthMethod{ssh.PublicKeys(identity)},
		HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), Timeout: time.Second,
	}
}

func dialServer(t *testing.T, server *Server, config *ssh.ClientConfig) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", server.Addr().String(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func waitForServer(t *testing.T, server *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Wait(ctx); err != nil {
		t.Fatalf("server did not finish: %v", err)
	}
}

func awaitServerSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

type pausedAcceptListener struct {
	net.Listener
	accepted chan struct{}
	release  <-chan struct{}
}

func (listener *pausedAcceptListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err == nil {
		close(listener.accepted)
		<-listener.release
	}
	return connection, err
}

type deadlineListener struct {
	net.Listener
	deadlines chan<- time.Time
}

func (listener *deadlineListener) Accept() (net.Conn, error) {
	connection, err := listener.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &deadlineConnection{Conn: connection, deadlines: listener.deadlines}, nil
}

type deadlineConnection struct {
	net.Conn
	deadlines chan<- time.Time
}

func (connection *deadlineConnection) SetDeadline(deadline time.Time) error {
	connection.deadlines <- deadline
	return connection.Conn.SetDeadline(deadline)
}
