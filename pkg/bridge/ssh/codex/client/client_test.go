package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

func TestMainForwardsNativeStreamUnderTheFixedRemoteCommand(t *testing.T) {
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
	defer listener.Close()
	commands := make(chan string, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		config := &gossh.ServerConfig{NoClientAuth: true}
		config.AddHostKey(signer)
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
			for request := range channelRequests {
				var payload struct{ Command string }
				_ = gossh.Unmarshal(request.Payload, &payload)
				commands <- payload.Command
				_ = request.Reply(true, nil)
				input, _ := io.ReadAll(channel)
				_, _ = channel.Write(append([]byte("echo:"), input...))
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{3}))
				_ = channel.Close()
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := Main(ctx, []string{"--address", listener.Addr().String()}, io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0"}`)), &stdout, &stderr)
	if code != 3 || stdout.String() != `echo:{"jsonrpc":"2.0"}` || stderr.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if command := <-commands; command != RemoteCommand {
		t.Fatalf("remote command = %q", command)
	}
}

func TestMainRejectsAnyOtherInvocation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"--address"},
		{"--address", "127.0.0.1:2222", "extra"},
		{"--host", "127.0.0.1:2222"},
		{"--address", "127.0.0.1"},
		{"--address", ":2222"},
		{"--address", "127.0.0.1:0"},
		{"--address", "127.0.0.1:70000"},
		{"--address", "bad host:22"},
	} {
		var stderr bytes.Buffer
		if code := Main(context.Background(), args, io.NopCloser(strings.NewReader("")), io.Discard, &stderr); code != 255 || !strings.Contains(stderr.String(), "aries-codex-ssh:") {
			t.Fatalf("args %q: code=%d stderr=%q", args, code, stderr.String())
		}
	}
}
