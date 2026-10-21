package executor

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// sshFixture accepts unauthenticated clients like the bridge and serves each
// exec request with a fresh fake native server.
type sshFixture struct {
	address     string
	connections atomic.Int32
	mu          sync.Mutex
	commands    []string
}

func newSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	fixture := &sshFixture{address: listener.Addr().String()}
	config := &gossh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			fixture.connections.Add(1)
			go fixture.serveConnection(raw, config)
		}
	}()
	return fixture
}

func (f *sshFixture) serveConnection(raw net.Conn, config *gossh.ServerConfig) {
	defer raw.Close()
	conn, channels, requests, err := gossh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer conn.Close()
	go gossh.DiscardRequests(requests)
	for offered := range channels {
		channel, channelRequests, err := offered.Accept()
		if err != nil {
			return
		}
		go func() {
			defer channel.Close()
			for request := range channelRequests {
				var payload struct{ Command string }
				_ = gossh.Unmarshal(request.Payload, &payload)
				f.mu.Lock()
				f.commands = append(f.commands, payload.Command)
				f.mu.Unlock()
				_ = request.Reply(true, nil)
				(&fakeServer{}).serve(channel, channel)
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{0}))
				return
			}
		}()
	}
}

func TestMainOpensOneSSHExecPerCallOverOneConnection(t *testing.T) {
	fixture := newSSHFixture(t)
	codexIn, toMain := io.Pipe()
	fromMain, codexOut := io.Pipe()
	done := make(chan int, 1)
	go func() {
		done <- Main(context.Background(), []string{"--address", fixture.address}, codexIn, codexOut, io.Discard)
		_ = codexOut.Close()
	}()
	replies := bufio.NewScanner(fromMain)
	call := func(line string) message {
		t.Helper()
		if _, err := io.WriteString(toMain, line+"\n"); err != nil {
			t.Fatal(err)
		}
		if !replies.Scan() {
			t.Fatal("no reply")
		}
		var m message
		if err := json.Unmarshal(replies.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := call(`{"id":1,"method":"initialize","params":{"clientName":"codex-environment"}}`); string(m.ID) != "1" {
		t.Fatalf("initialize = %+v", m)
	}
	for id := 2; id <= 4; id++ {
		if m := call(`{"id":` + strconv.Itoa(id) + `,"method":"fs/getMetadata","params":{"path":"file:///app"}}`); len(m.Error) != 0 {
			t.Fatalf("metadata = %+v", m)
		}
	}
	if m := call(`{"id":9,"method":"process/start","params":{"processId":"p","argv":["exit-now"]}}`); string(m.ID) != "9" {
		t.Fatalf("start = %+v", m)
	}
	for _, want := range []string{"process/exited", "process/closed"} {
		if !replies.Scan() || !strings.Contains(replies.Text(), want) {
			t.Fatalf("want %s, got %q", want, replies.Text())
		}
	}
	_ = toMain.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Main did not exit after Codex closed its input")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.connections.Load() != 1 || len(fixture.commands) != 5 {
		t.Fatalf("connections=%d execs=%d, want 1 connection and one exec per call", fixture.connections.Load(), len(fixture.commands))
	}
	for _, command := range fixture.commands {
		if command != RemoteCommand {
			t.Fatalf("exec command = %q", command)
		}
	}
}
