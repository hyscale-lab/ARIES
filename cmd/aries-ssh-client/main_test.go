package main

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"crypto/ed25519"
	"crypto/rand"
	sshclient "github.com/hyscale-lab/aries/pkg/bridge/ssh/openclaw/client"
	gossh "golang.org/x/crypto/ssh"
)

func TestInheritedStdinCanCloseWhileHarnessKeepsInputOpen(t *testing.T) {
	if os.Getenv("ARIES_STDIN_SUBPROCESS") == "1" {
		input, err := openStdin()
		if err != nil {
			t.Fatal(err)
		}
		code, err := sshclient.Run(context.Background(), sshclient.Config{Address: os.Getenv("ARIES_STDIN_SERVER"), User: "aries"}, "exit without consuming stdin", input, io.Discard, io.Discard)
		if err != nil || code != 9 {
			t.Fatalf("early remote exit: code=%d err=%v", code, err)
		}
		return
	}
	for _, kind := range []string{"pipe", "node-socketpair"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInheritedStdinCanCloseWhileHarnessKeepsInputOpen$")
			command.Env = append(os.Environ(), "ARIES_STDIN_SUBPROCESS=1")
			address := earlyExitServer(t, ctx)
			command.Env = append(command.Env, "ARIES_STDIN_SERVER="+address)
			var input, writer *os.File
			if kind == "pipe" {
				var err error
				input, writer, err = os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				sockets, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
				if err != nil {
					t.Fatal(err)
				}
				input = os.NewFile(uintptr(sockets[0]), "child-input")
				writer = os.NewFile(uintptr(sockets[1]), "harness-input")
			}
			defer input.Close()
			defer writer.Close()
			command.Stdin = input
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("inherited stdin failed to close: %v (%s)", err, output)
			}
		})
	}
}

func earlyExitServer(t *testing.T, ctx context.Context) string {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		defer stop()
		config := &gossh.ServerConfig{NoClientAuth: true}
		config.AddHostKey(signer)
		conn, channels, global, err := gossh.NewServerConn(raw, config)
		if err != nil {
			return
		}
		defer conn.Close()
		go gossh.DiscardRequests(global)
		for offer := range channels {
			channel, requests, err := offer.Accept()
			if err != nil {
				return
			}
			request := <-requests
			if request != nil {
				_ = request.Reply(true, nil)
				_, _ = channel.SendRequest("exit-status", false, gossh.Marshal(struct{ Status uint32 }{9}))
			}
			_ = channel.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("SSH fixture did not stop")
		}
	})
	return listener.Addr().String()
}
