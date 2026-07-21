package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hyscale-lab/aries/pkg/sandbox/docker/execproto"
)

func TestRunExecutesArgvDirectlyAndReportsStreams(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("test sandbox forbids Unix listeners: %v", err)
		}
		t.Fatal(err)
	}
	defer listener.Close()
	wantArgs := append([]string(nil), os.Args...)
	os.Args = []string{"aries-exec-helper", socket, "/bin/sh", "-c", "cat; printf err >&2; exit 7"}
	defer func() { os.Args = wantArgs }()

	resultChannel := make(chan execproto.Result, 1)
	errorChannel := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			errorChannel <- acceptErr
			return
		}
		defer connection.Close()
		if protocolErr := execproto.ReadHello(connection); protocolErr != nil {
			errorChannel <- protocolErr
			return
		}
		if protocolErr := execproto.WriteInput(connection, []byte("stdin")); protocolErr != nil {
			errorChannel <- protocolErr
			return
		}
		result, protocolErr := execproto.ReadResult(connection, maxIO)
		if protocolErr != nil {
			errorChannel <- protocolErr
			return
		}
		resultChannel <- result
	}()
	if err := run(); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	select {
	case err := <-errorChannel:
		t.Fatal(err)
	case result := <-resultChannel:
		if result.ExitCode != 7 || string(result.Stdout) != "stdin" || string(result.Stderr) != "err" {
			t.Fatalf("result = %#v", result)
		}
	}
}

func TestLimitedBufferConsumesButDoesNotRetainBeyondSharedLimit(t *testing.T) {
	limit := &sharedLimit{remaining: 3}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	if _, err := stdout.Write([]byte("123")); err != nil {
		t.Fatal(err)
	}
	if written, err := stderr.Write([]byte("45")); err != nil || written != 2 || !limit.exceeded || stderr.buffer.Len() != 0 {
		t.Fatal("shared output limit was not enforced")
	}
}
