package sshbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestClientRemoteExitDoesNotWaitForCallerInput(t *testing.T) {
	for _, test := range []struct {
		name, command string
		keepalive     time.Duration
	}{
		{"OpenClaw", "'/bin/sh' '-c' 'true'", 15 * time.Second},
		{"Codex", "aries-codex-exec-server-v1", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newClientServer(t, test.command, func(channel ssh.Channel) error { return sendClientExit(channel, 0) })
			server.config.KeepaliveInterval = test.keepalive
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			finished := make(chan clientResult, 1)
			go func() {
				code, err := RunClient(ctx, server.config, test.command, struct{ io.Reader }{input}, io.Discard, io.Discard)
				finished <- clientResult{code, err}
			}()
			got := awaitClient(t, finished)
			if got.code != 0 || got.err != nil {
				t.Fatalf("RunClient = (%d, %v)", got.code, got.err)
			}
		})
	}
}

func TestClientForwardsStreamsAndExitStatus(t *testing.T) {
	t.Setenv("ARIES_CLIENT_TEST_SECRET", "must-not-be-forwarded")
	const command = "'/bin/sh' '-c' 'cat'"
	input := []byte("first\x00\xff\nsecond\n")
	server := newClientServer(t, command, func(channel ssh.Channel) error {
		read, err := io.ReadAll(channel)
		if err != nil {
			return err
		}
		if !bytes.Equal(read, input) {
			return fmt.Errorf("stdin=%q", read)
		}
		if _, err := channel.Write(read); err != nil {
			return err
		}
		if _, err := channel.Stderr().Write([]byte("diagnostic\x00\n")); err != nil {
			return err
		}
		return sendClientExit(channel, 42)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code, err := RunClient(ctx, server.config, command, bytes.NewReader(input), &stdout, &stderr)
	if err != nil || code != 42 || !bytes.Equal(stdout.Bytes(), input) || stderr.String() != "diagnostic\x00\n" {
		t.Fatalf("RunClient=(%d,%v), stdout=%q stderr=%q", code, err, stdout.Bytes(), stderr.Bytes())
	}
	if err := <-server.done; err != nil {
		t.Fatal(err)
	}
}

func TestClientInputFailurePreservesRemoteExitPriority(t *testing.T) {
	for _, test := range []struct {
		name       string
		remoteCode uint32
		wantCode   int
		wantError  bool
	}{
		{name: "successful remote", wantCode: 255, wantError: true},
		{name: "failed remote", remoteCode: 23, wantCode: 23},
	} {
		t.Run(test.name, func(t *testing.T) {
			const partial = "partial stdin before read failure"
			server := newClientServer(t, "command", func(channel ssh.Channel) error {
				content, err := io.ReadAll(channel)
				if err != nil {
					return err
				}
				if string(content) != partial {
					return fmt.Errorf("stdin before failure = %q", content)
				}
				return sendClientExit(channel, test.remoteCode)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			input := io.MultiReader(strings.NewReader(partial), failedClientInput{})
			code, err := RunClient(ctx, server.config, "command", input, io.Discard, io.Discard)
			if code != test.wantCode || (test.wantError && !errors.Is(err, syscall.EIO)) || (!test.wantError && err != nil) {
				t.Fatalf("RunClient=(%d,%v), want code %d, input error %v", code, err, test.wantCode, test.wantError)
			}
			if err := <-server.done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestClientNilInputClosesRemoteStdin(t *testing.T) {
	server := newClientServer(t, "command", func(channel ssh.Channel) error {
		content, err := io.ReadAll(channel)
		if err != nil {
			return err
		}
		if len(content) != 0 {
			return fmt.Errorf("nil stdin forwarded %q", content)
		}
		return sendClientExit(channel, 0)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if code, err := RunClient(ctx, server.config, "command", nil, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatalf("RunClient=(%d,%v), want successful empty input", code, err)
	}
	if err := <-server.done; err != nil {
		t.Fatal(err)
	}
}

type failedClientInput struct{}

func (failedClientInput) Read([]byte) (int, error) { return 0, syscall.EIO }

func TestClientCancellationClosesSessionWithBlockedInput(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	server := newClientServer(t, "command", func(channel ssh.Channel) error {
		close(started)
		_, err := io.Copy(io.Discard, channel)
		close(closed)
		return err
	})
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan clientResult, 1)
	go func() {
		code, err := RunClient(ctx, server.config, "command", struct{ io.Reader }{input}, io.Discard, io.Discard)
		finished <- clientResult{code, err}
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("SSH command did not start")
	}
	cancel()
	got := awaitClient(t, finished)
	if got.code != 255 || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("RunClient=(%d,%v), want cancellation", got.code, got.err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancellation left session open")
	}
}

func TestClientCancellationInterruptsHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if connection, err := listener.Accept(); err == nil {
			accepted <- connection
		}
	}()
	configuration := ClientConfig{Address: listener.Addr().String(), User: "aries", Signer: clientSigner(t), HostKey: clientSigner(t).PublicKey(), ConnectTimeout: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan clientResult, 1)
	go func() {
		code, err := RunClient(ctx, configuration, "command", strings.NewReader(""), io.Discard, io.Discard)
		finished <- clientResult{code, err}
	}()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("client did not connect")
	}
	cancel()
	got := awaitClient(t, finished)
	if got.code != 255 || !errors.Is(got.err, context.Canceled) {
		t.Fatalf("RunClient=(%d,%v), want cancellation", got.code, got.err)
	}
}

func TestClientRejectsUntrustedHostKey(t *testing.T) {
	server := newClientServer(t, "command", func(ssh.Channel) error { return errors.New("untrusted session opened") })
	server.config.HostKey = clientSigner(t).PublicKey()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, err := RunClient(ctx, server.config, "command", strings.NewReader(""), io.Discard, io.Discard)
	if code != 255 || err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("RunClient=(%d,%v), want host key rejection", code, err)
	}
}

func TestClientSendsConfiguredKeepalive(t *testing.T) {
	received := make(chan struct{}, 1)
	server := newClientServer(t, "command", func(channel ssh.Channel) error {
		select {
		case <-received:
			return sendClientExit(channel, 0)
		case <-time.After(time.Second):
			return errors.New("keepalive was not sent")
		}
	})
	server.config.KeepaliveInterval = 10 * time.Millisecond
	go func() {
		select {
		case <-server.keepalives:
			received <- struct{}{}
		case <-time.After(2 * time.Second):
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, err := RunClient(ctx, server.config, "command", strings.NewReader(""), io.Discard, io.Discard)
	if code != 0 || err != nil {
		t.Fatalf("RunClient=(%d,%v)", code, err)
	}
	if err := <-server.done; err != nil {
		t.Fatal(err)
	}
}

type clientResult struct {
	code int
	err  error
}

func awaitClient(t *testing.T, finished <-chan clientResult) clientResult {
	t.Helper()
	select {
	case result := <-finished:
		return result
	case <-time.After(time.Second):
		t.Fatal("client did not finish promptly")
		return clientResult{}
	}
}

type clientServer struct {
	config     ClientConfig
	done       chan error
	keepalives chan struct{}
}

func newClientServer(t *testing.T, command string, serve func(ssh.Channel) error) *clientServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, identity := clientSigner(t), clientSigner(t)
	server := &clientServer{config: ClientConfig{Address: listener.Addr().String(), User: "aries", Signer: identity, HostKey: host.PublicKey(), ConnectTimeout: 5 * time.Second}, done: make(chan error, 1), keepalives: make(chan struct{}, 1)}
	configuration := &ssh.ServerConfig{PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if metadata.User() != "aries" || !bytes.Equal(key.Marshal(), identity.PublicKey().Marshal()) {
			return nil, errors.New("unexpected credentials")
		}
		return nil, nil
	}}
	configuration.AddHostKey(host)
	connections := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			server.done <- err
			return
		}
		connections <- connection
		defer connection.Close()
		sshConnection, channels, requests, err := ssh.NewServerConn(connection, configuration)
		if err != nil {
			server.done <- err
			return
		}
		defer sshConnection.Close()
		go func() {
			for request := range requests {
				valid := request.Type == "keepalive@openssh.com" && request.WantReply && len(request.Payload) == 0
				_ = request.Reply(valid, nil)
				if valid {
					select {
					case server.keepalives <- struct{}{}:
					default:
					}
				}
			}
		}()
		newChannel, ok := <-channels
		if !ok {
			server.done <- errors.New("client closed before session")
			return
		}
		if newChannel.ChannelType() != "session" || len(newChannel.ExtraData()) != 0 {
			_ = newChannel.Reject(ssh.UnknownChannelType, "session only")
			server.done <- errors.New("unexpected channel")
			return
		}
		channel, sessionRequests, err := newChannel.Accept()
		if err != nil {
			server.done <- err
			return
		}
		defer channel.Close()
		request, ok := <-sessionRequests
		var payload struct{ Command string }
		if !ok || request.Type != "exec" || !request.WantReply || ssh.Unmarshal(request.Payload, &payload) != nil || payload.Command != command {
			if ok {
				_ = request.Reply(false, nil)
			}
			server.done <- errors.New("unexpected session request or changed command")
			return
		}
		if err := request.Reply(true, nil); err != nil {
			server.done <- err
			return
		}
		server.done <- serve(channel)
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case connection := <-connections:
			_ = connection.Close()
		default:
		}
	})
	return server
}

func clientSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
func sendClientExit(channel ssh.Channel, code uint32) error {
	_, err := channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	return err
}
