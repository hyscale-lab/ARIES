package client

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
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func testSigner(t *testing.T) gossh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func serveClientTest(t *testing.T, handle func(gossh.Channel, <-chan *gossh.Request)) Config {
	t.Helper()
	host, identity := testSigner(t), testSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		config := &gossh.ServerConfig{PublicKeyCallback: func(_ gossh.ConnMetadata, key gossh.PublicKey) (*gossh.Permissions, error) {
			if !bytes.Equal(key.Marshal(), identity.PublicKey().Marshal()) {
				return nil, errors.New("wrong identity")
			}
			return nil, nil
		}}
		config.AddHostKey(host)
		conn, channels, requests, err := gossh.NewServerConn(raw, config)
		if err != nil {
			return
		}
		defer conn.Close()
		go gossh.DiscardRequests(requests)
		for offered := range channels {
			channel, requests, err := offered.Accept()
			if err != nil {
				return
			}
			handle(channel, requests)
			_ = channel.Close()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	return Config{Address: listener.Addr().String(), User: "aries", Identity: identity, HostKey: host.PublicKey()}
}

func TestRunForwardsExactCommandAndBinaryStreamsThroughEOF(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	remote := "printf 'quotes \" ; touch " + marker + " ; $(touch " + marker + ")\nUnicode: λ\n\t$HOME `false`"
	payload := []byte{0, 255, 'a', '\n', 128}
	seen := make(chan string, 1)
	config := serveClientTest(t, func(channel gossh.Channel, requests <-chan *gossh.Request) {
		request := <-requests
		var command struct{ Command string }
		if request == nil || request.Type != "exec" || gossh.Unmarshal(request.Payload, &command) != nil {
			t.Error("missing exec")
			return
		}
		seen <- command.Command
		_ = request.Reply(true, nil)
		input, err := io.ReadAll(channel)
		if err != nil {
			t.Error(err)
			return
		}
		if !bytes.Equal(input, payload) {
			t.Errorf("input = %v", input)
		}
		// Output produced only after EOF proves that half-close does not lose output.
		_, _ = channel.Write(input)
		_, _ = channel.Stderr().Write([]byte{255, 0, 'e'})
		_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{7}))
	})
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := Run(ctx, config, remote, io.NopCloser(bytes.NewReader(payload)), &stdout, &stderr)
	if err != nil || code != 7 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if got := <-seen; got != remote {
		t.Fatalf("command changed: %q != %q", got, remote)
	}
	if !bytes.Equal(stdout.Bytes(), payload) || !bytes.Equal(stderr.Bytes(), []byte{255, 0, 'e'}) {
		t.Fatalf("streams: %v %v", stdout.Bytes(), stderr.Bytes())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote command executed locally: %v", err)
	}
}

func TestRunRejectsWrongHostKey(t *testing.T) {
	config := serveClientTest(t, func(gossh.Channel, <-chan *gossh.Request) { t.Error("exec admitted with wrong host key") })
	config.HostKey = testSigner(t).PublicKey()
	code, err := Run(context.Background(), config, "true", io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
	if code != 255 || err == nil || !strings.Contains(err.Error(), "host-key") {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestRunCancellationAndEarlyExitJoinBlockedInput(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		name := "remote-exit"
		if cancelRun {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			admitted := make(chan struct{})
			config := serveClientTest(t, func(channel gossh.Channel, requests <-chan *gossh.Request) {
				request := <-requests
				if request == nil {
					return
				}
				_ = request.Reply(true, nil)
				close(admitted)
				if cancelRun {
					_, _ = io.Copy(io.Discard, channel)
					return
				}
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{0}))
			})
			input, writer := io.Pipe()
			defer writer.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			var code int
			var err error
			go func() { code, err = Run(ctx, config, "not parsed", input, io.Discard, io.Discard); close(done) }()
			select {
			case <-admitted:
			case <-time.After(5 * time.Second):
				t.Fatal("not admitted")
			}
			if cancelRun {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("client hung on blocked stdin")
			}
			if cancelRun {
				if code != 255 || !errors.Is(err, context.Canceled) {
					t.Fatalf("code=%d err=%v", code, err)
				}
			} else if code != 0 || err != nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
			if _, err := writer.Write([]byte("closed")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("stdin not closed: %v", err)
			}
		})
	}
}

func TestRunTransportFailures(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "missing-exit-status"
		if reject {
			name = "exec-rejected"
		}
		t.Run(name, func(t *testing.T) {
			config := serveClientTest(t, func(channel gossh.Channel, requests <-chan *gossh.Request) {
				request := <-requests
				if request != nil {
					_ = request.Reply(!reject, nil)
				}
			})
			code, err := Run(context.Background(), config, "unchanged", io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
			if code != 255 || err == nil {
				t.Fatalf("code=%d err=%v", code, err)
			}
		})
	}
}

func TestRunCancellationDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	connected := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(connected)
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan error, 1)
	key := testSigner(t)
	go func() {
		_, err := Run(ctx, Config{Address: listener.Addr().String(), User: "aries", Identity: key, HostKey: key.PublicKey()}, "x", io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
		returned <- err
	}()
	<-connected
	cancel()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("cancel succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("handshake ignored cancellation")
	}
	<-done
}

type failedInput struct{ err error }

func (r failedInput) Read([]byte) (int, error) { return 0, r.err }
func (r failedInput) Close() error             { return nil }

func TestRunReportsInputFailureEvenIfRemoteExitsZero(t *testing.T) {
	failure := errors.New("input source failed")
	config := serveClientTest(t, func(channel gossh.Channel, requests <-chan *gossh.Request) {
		request := <-requests
		if request == nil {
			return
		}
		_ = request.Reply(true, nil)
		_, _ = io.Copy(io.Discard, channel)
		_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{0}))
	})
	code, err := Run(context.Background(), config, "x", failedInput{failure}, io.Discard, io.Discard)
	if code != 255 || !errors.Is(err, failure) {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

// CLI stdin is an *os.File, whose interrupted Read returns os.ErrClosed rather
// than the io.ErrClosedPipe exercised by the in-memory pipe regression.
func TestRunEarlyExitClosesOSPipeWithoutReplacingRemoteStatus(t *testing.T) {
	config := serveClientTest(t, func(channel gossh.Channel, requests <-chan *gossh.Request) {
		request := <-requests
		if request == nil {
			return
		}
		_ = request.Reply(true, nil)
		_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{9}))
	})
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := Run(ctx, config, "early-exit", input, io.Discard, io.Discard)
	if err != nil || code != 9 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := input.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdin still owned: %v", err)
	}
}

func TestRunPreservesSourceClosedErrorBeforeOwnedClosure(t *testing.T) {
	config := serveClientTest(t, func(channel gossh.Channel, requests <-chan *gossh.Request) {
		request := <-requests
		if request == nil {
			return
		}
		_ = request.Reply(true, nil)
		_, _ = io.Copy(io.Discard, channel)
		_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{0}))
	})
	code, err := Run(context.Background(), config, "x", failedInput{os.ErrClosed}, io.Discard, io.Discard)
	if code != 255 || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("code=%d err=%v", code, err)
	}
}
