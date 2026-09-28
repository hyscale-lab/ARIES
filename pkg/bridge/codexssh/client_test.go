package codexssh

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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestRunClientStreamsNativeProtocolAndPreservesStatus(t *testing.T) {
	t.Setenv("ARIES_CLIENT_TEST_SECRET", "must-not-be-forwarded")
	ready := make(chan struct{})
	wantInput := []byte("{\"method\":\"initialize\"}\n\x00\xfflate input\n")
	server := newClientTestServer(t, func(channel ssh.Channel, requests <-chan *ssh.Request) error {
		if err := clientTestAcceptExec(requests); err != nil {
			return err
		}
		if _, err := channel.Write([]byte("ready\x00\n")); err != nil {
			return err
		}
		close(ready)
		input, err := io.ReadAll(channel)
		if err != nil {
			return err
		}
		if !bytes.Equal(input, wantInput) {
			return fmt.Errorf("stdin = %q, want %q", input, wantInput)
		}
		if _, err := channel.Write(input); err != nil {
			return err
		}
		if _, err := channel.Stderr().Write([]byte("executor stderr\x00\n")); err != nil {
			return err
		}
		return clientTestExit(channel, 42)
	})
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan clientTestResult, 1)
	go func() {
		code, err := RunClient(ctx, server.args, input, &stdout, &stderr)
		result <- clientTestResult{code, err}
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("client did not start the executor before stdin arrived")
	}
	if _, err := writer.Write(wantInput); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got := clientTestAwait(t, result)
	if got.code != 42 || got.err != nil {
		t.Fatalf("RunClient = (%d, %v), want (42, nil)", got.code, got.err)
	}
	if want := append([]byte("ready\x00\n"), wantInput...); !bytes.Equal(stdout.Bytes(), want) {
		t.Fatalf("stdout = %q, want %q", stdout.Bytes(), want)
	}
	if got := stderr.String(); got != "executor stderr\x00\n" {
		t.Fatalf("stderr = %q", got)
	}
	if err := <-server.done; err != nil {
		t.Fatal(err)
	}
}

func TestRunClientRejectsArgumentsAndUnsafeCredentialFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, []string) []string
	}{
		{"extra command", func(_ *testing.T, args []string) []string { return append(args, "sh") }},
		{"unknown flag", func(_ *testing.T, args []string) []string { return append(args, "--forward-agent") }},
		{"missing argument", func(_ *testing.T, args []string) []string { return args[:6] }},
		{"wrong user", func(_ *testing.T, args []string) []string { args[3] = "root"; return args }},
		{"missing host", func(_ *testing.T, args []string) []string { args[1] = ":22"; return args }},
		{"invalid port", func(_ *testing.T, args []string) []string { args[1] = "localhost:65536"; return args }},
		{"identity symlink", func(t *testing.T, args []string) []string {
			link := filepath.Join(filepath.Dir(args[5]), "identity-link")
			if err := os.Symlink(args[5], link); err != nil {
				t.Fatal(err)
			}
			args[5] = link
			return args
		}},
		{"public identity", func(t *testing.T, args []string) []string {
			if err := os.Chmod(args[5], 0o644); err != nil {
				t.Fatal(err)
			}
			return args
		}},
		{"extra host key", func(t *testing.T, args []string) []string {
			content, err := os.ReadFile(args[7])
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(args[7], append(content, content...), 0o600); err != nil {
				t.Fatal(err)
			}
			return args
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := clientTestArguments(t, "127.0.0.1:1", clientTestSigner(t).PublicKey())
			args = test.mutate(t, args)
			code, err := RunClient(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard)
			if code != 255 || err == nil || strings.Contains(err.Error(), "connect") {
				t.Fatalf("RunClient = (%d, %v), want rejection before connect", code, err)
			}
		})
	}
}

type clientTestResult struct {
	code int
	err  error
}

func clientTestAwait(t *testing.T, result <-chan clientTestResult) clientTestResult {
	t.Helper()
	select {
	case got := <-result:
		return got
	case <-time.After(time.Second):
		t.Fatal("client did not complete promptly")
		return clientTestResult{}
	}
}

type clientTestServer struct {
	address string
	args    []string
	done    chan error
}

func newClientTestServer(t *testing.T, serve func(ssh.Channel, <-chan *ssh.Request) error) *clientTestServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := clientTestSigner(t)
	server := &clientTestServer{
		address: listener.Addr().String(),
		args:    clientTestArguments(t, listener.Addr().String(), host.PublicKey()),
		done:    make(chan error, 1),
	}
	identity, err := os.ReadFile(server.args[5])
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	configuration := &ssh.ServerConfig{
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != "aries" || !bytes.Equal(key.Marshal(), signer.PublicKey().Marshal()) {
				return nil, errors.New("unexpected SSH credentials")
			}
			return nil, nil
		},
	}
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
		go ssh.DiscardRequests(requests)
		newChannel, ok := <-channels
		if !ok {
			server.done <- errors.New("client closed before opening a session")
			return
		}
		if newChannel.ChannelType() != "session" || len(newChannel.ExtraData()) != 0 {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only a session is supported")
			server.done <- errors.New("unexpected SSH channel")
			return
		}
		channel, sessionRequests, err := newChannel.Accept()
		if err != nil {
			server.done <- err
			return
		}
		defer channel.Close()
		server.done <- serve(channel, sessionRequests)
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

func clientTestAcceptExec(requests <-chan *ssh.Request) error {
	request, ok := <-requests
	if !ok {
		return errors.New("client closed before sending exec")
	}
	var payload struct{ Command string }
	if request.Type != "exec" || !request.WantReply || ssh.Unmarshal(request.Payload, &payload) != nil || payload.Command != "aries-codex-exec-server-v1" {
		_ = request.Reply(false, nil)
		return fmt.Errorf("unexpected SSH request %q: %q", request.Type, request.Payload)
	}
	return request.Reply(true, nil)
}

func clientTestExit(channel ssh.Channel, code uint32) error {
	_, err := channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
	return err
}

func clientTestSigner(t *testing.T) ssh.Signer {
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

func clientTestArguments(t *testing.T, address string, host ssh.PublicKey) []string {
	t.Helper()
	directory := t.TempDir()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	identity := filepath.Join(directory, "identity")
	if err := os.WriteFile(identity, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(knownhosts.Line([]string{address}, host)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--address", address, "--user", "aries", "--identity", identity, "--known-hosts", knownHosts}
}
