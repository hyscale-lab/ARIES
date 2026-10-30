package grpc

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The generated stub sends the script, input, and login flag to the live bridge.
func TestStubRunsThroughTheBridge(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.exitCode, executor.stdout, executor.stderr = 7, []byte("out"), []byte("err")
	_, client, _ := startBridge(t, executor, Options{})
	for _, login := range []bool{false, true} {
		response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "echo hi", Stdin: []byte("piped"), Login: login})
		if err != nil || response.GetExitCode() != 7 || string(response.GetStdout()) != "out" || string(response.GetStderr()) != "err" {
			t.Fatalf("Exec(login=%t) = %v, %v", login, response, err)
		}
	}
	commands, stdins, _ := executor.snapshot()
	if len(commands) != 2 || strings.Join(commands[0].Args, " ") != "-c echo hi" || strings.Join(commands[1].Args, " ") != "-l -c echo hi" || len(stdins) != 2 || string(stdins[0]) != "piped" || string(stdins[1]) != "piped" {
		t.Fatalf("commands = %#v, stdin = %q", commands, stdins)
	}
}

// A stopped bridge cannot let a new call reach the sandbox.
func TestStubReportsRevocation(t *testing.T) {
	executor := newFakeExecutor(nil)
	manager, client, _ := startBridge(t, executor, Options{})
	stop(t, manager)
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "true"}); err == nil {
		t.Fatal("a stopped bridge accepted an RPC")
	}
	if commands, _, _ := executor.snapshot(); len(commands) != 0 {
		t.Fatal("a call after revocation reached the sandbox")
	}
}

// A sandbox failure keeps its reason and does not report a completed command.
func TestStubReportsAnExecThatDidNotComplete(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.execErr = errors.New("backend failed in test")
	_, client, _ := startBridge(t, executor, Options{})
	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "true"})
	if err != nil || response.GetReason() != sandboxv1.Reason_REASON_SANDBOX_ERROR {
		t.Fatalf("Exec = %v, %v", response, err)
	}
}

// File procedures preserve binary content and report exact stream metadata.
func TestStubFileModesRoundTripBytes(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{})
	_, client, _ := startBridge(t, executor, Options{})
	binary := []byte("a\x00b\nline two\n\xff")
	written, err := writeFile(client, "/app/f", int64(len(binary)), binary)
	if err != nil || written.GetBytesWritten() != int64(len(binary)) || !written.GetCreated() {
		t.Fatalf("WriteFile = %v, %v", written, err)
	}
	content, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/f"})
	if err != nil || !bytes.Equal(content, binary) || header.GetSize() != int64(len(binary)) || header.GetTruncated() {
		t.Fatalf("ReadFile = %q, %v, %v", content, header, err)
	}
	part, header, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/f", MaxBytes: 2})
	if err != nil || !bytes.Equal(part, binary[:2]) || !header.GetTruncated() {
		t.Fatalf("bounded ReadFile = %q, %v, %v", part, header, err)
	}
	lines, summary, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/f", FirstLine: 2, MaxLines: 1, MaxLineBytes: 4})
	if err != nil || string(lines) != "line\n" || summary.GetTotalLines() != 2 || summary.GetMore() {
		t.Fatalf("ReadLines = %q, %v, %v", lines, summary, err)
	}
	if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/none"}); status.Code(err) != codes.NotFound {
		t.Fatalf("missing file status = %v", err)
	}
}
