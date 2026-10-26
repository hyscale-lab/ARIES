package grpc

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClientRejectsMalformedInvocations pins the argv contract the plugin relies on:
// a known mode, one operand, nothing sent otherwise.
func TestClientRejectsMalformedInvocations(t *testing.T) {
	executor := newFakeExecutor(nil)
	_, _, endpoint := startBridge(t, executor, Options{})
	t.Setenv(targetEnv, endpoint.Address)

	for _, args := range [][]string{
		nil,
		{"aries@host", "bash", "-c", "true"},
		{"exec"},
		{"exec", "true", "extra"},
		{"exec", "--unknown", "true"},
		{"file", "read", "--offset", "1", "/app/f"},
	} {
		var stdout, stderr bytes.Buffer
		if code := ClientMain(args, strings.NewReader(""), &stdout, &stderr); code != transportFailureExit {
			t.Fatalf("ClientMain(%q) = %d, want %d", args, code, transportFailureExit)
		}
	}
	if commands, _, fileOps := executor.snapshot(); len(commands) != 0 || fileOps != 0 {
		t.Fatal("a malformed invocation reached the sandbox")
	}
}

// TestClientMainRunsThroughTheBridge drives the staged client against a live
// bridge, so the call, the stdin and the exit code are all exercised as the
// container will exercise them.
func TestClientMainRunsThroughTheBridge(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.exitCode, executor.stdout, executor.stderr = 7, []byte("out"), []byte("err")
	_, _, endpoint := startBridge(t, executor, Options{})
	t.Setenv(targetEnv, endpoint.Address)

	var stdout, stderr bytes.Buffer
	code := ClientMain([]string{"exec", "--", "echo hi"}, strings.NewReader("piped"), &stdout, &stderr)

	if code != executor.exitCode {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, executor.exitCode, stderr.String())
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
	commands, stdins, _ := executor.snapshot()
	if len(commands) != 1 || strings.Join(commands[0].Args, " ") != "-c echo hi" {
		t.Fatalf("commands = %#v", commands)
	}
	if len(stdins) != 1 || string(stdins[0]) != "piped" {
		t.Fatalf("stdin = %q", stdins)
	}
}

// TestClientRunsLoginScriptsUnderALoginShell pins the flag Hermes's session
// bootstrap needs: the script must reach the sandbox as `bash -l -c`.
func TestClientRunsLoginScriptsUnderALoginShell(t *testing.T) {
	executor := newFakeExecutor(nil)
	_, _, endpoint := startBridge(t, executor, Options{})
	t.Setenv(targetEnv, endpoint.Address)

	var stdout, stderr bytes.Buffer
	if code := ClientMain([]string{"exec", "--login", "--", "pwd -P"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d (stderr: %s)", code, stderr.String())
	}
	commands, _, _ := executor.snapshot()
	if len(commands) != 1 || strings.Join(commands[0].Args, " ") != "-l -c pwd -P" {
		t.Fatalf("commands = %#v", commands)
	}
}

// TestClientMainReportsRevocationAsTransportFailure pins that a call after the
// bridge is revoked reaches Hermes as exit 255, not as a command exit code.
func TestClientMainReportsRevocationAsTransportFailure(t *testing.T) {
	executor := newFakeExecutor(nil)
	manager, _, endpoint := startBridge(t, executor, Options{})
	stop(t, manager)
	t.Setenv(targetEnv, endpoint.Address)

	var stdout, stderr bytes.Buffer
	if code := ClientMain([]string{"exec", "true"}, strings.NewReader(""), &stdout, &stderr); code != transportFailureExit {
		t.Fatalf("exit code = %d, want %d", code, transportFailureExit)
	}
	if commands, _, _ := executor.snapshot(); len(commands) != 0 {
		t.Fatal("a call after revocation reached the sandbox")
	}
}

// TestClientSendsTheToolCallID pins that every client operation sends the id
// from the environment, and sends none when the variable is not set.
func TestClientSendsTheToolCallID(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{"app/f": &fstest.MapFile{Data: []byte("x\n"), Mode: 0o644}})
	manager, _, endpoint := startBridge(t, executor, Options{})
	t.Setenv(targetEnv, endpoint.Address)
	t.Setenv(toolCallIDEnv, testToolCallID)
	operations := [][]string{
		{"exec", "true"},
		{"file", "stat", "/app/f"},
		{"file", "read", "/app/f"},
		{"file", "lines", "--first", "1", "--max", "1", "/app/f"},
		{"file", "write", "--size", "1", "/app/f"},
	}
	runAll := func() {
		for _, args := range operations {
			var stdout, stderr bytes.Buffer
			if code := ClientMain(args, strings.NewReader("x"), &stdout, &stderr); code != 0 {
				t.Fatalf("ClientMain(%q) = %d (stderr: %s)", args, code, stderr.String())
			}
		}
	}
	runAll()
	if err := os.Unsetenv(toolCallIDEnv); err != nil {
		t.Fatal(err)
	}
	runAll()
	stop(t, manager)

	// One round with the variable and one without.
	const rounds = 2
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != rounds*len(operations) {
		t.Fatalf("records = %d, want %d", len(records), rounds*len(operations))
	}
	for index, record := range records {
		id, present := record["tool_call_id"]
		if index < len(operations) && id != testToolCallID || index >= len(operations) && present {
			t.Fatalf("record %d tool_call_id = %v (present %t)", index, id, present)
		}
	}
}

// TestClientFileModesRoundTripBytes drives every file mode as the plugin will:
// payload on stdout, one metadata line on stderr, statuses as fixed exit codes.
func TestClientFileModesRoundTripBytes(t *testing.T) {
	executor := newFakeExecutor(fstest.MapFS{"app/dir": &fstest.MapFile{Mode: fs.ModeDir | 0o755}})
	_, _, endpoint := startBridge(t, executor, Options{})
	t.Setenv(targetEnv, endpoint.Address)
	run := func(stdin string, args ...string) (int, string, string) {
		var stdout, stderr bytes.Buffer
		code := ClientMain(args, strings.NewReader(stdin), &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	binary := "a\x00b\nline two\n\xff"
	size := strconv.Itoa(len(binary))
	if code, out, meta := run(binary, "file", "write", "--size", size, "/app/f"); code != 0 || out != "" || meta != fmt.Sprintf(`{"bytes_written":%s,"created":true}`+"\n", size) {
		t.Fatalf("write: %d %q %q", code, out, meta)
	}
	// Several chunks each way: the client never holds the whole file.
	large := strings.Repeat("0123456789abcdef", 3*chunkBytes/16+5)
	if code, _, meta := run(large, "file", "write", "--size", strconv.Itoa(len(large)), "/app/large"); code != 0 {
		t.Fatalf("large write: %d %q", code, meta)
	}
	if code, out, _ := run("", "file", "read", "/app/large"); code != 0 || out != large {
		t.Fatalf("large read: %d, %d bytes, want %d", code, len(out), len(large))
	}
	if code, out, meta := run("", "file", "read", "/app/f"); code != 0 || out != binary || meta != fmt.Sprintf(`{"size":%s,"truncated":false}`+"\n", size) {
		t.Fatalf("read: %d %q %q", code, out, meta)
	}
	if code, out, meta := run("", "file", "read", "--max-bytes", "2", "/app/f"); code != 0 || out != "a\x00" || meta != fmt.Sprintf(`{"size":%s,"truncated":true}`+"\n", size) {
		t.Fatalf("read head: %d %q %q", code, out, meta)
	}
	if code, out, meta := run("", "file", "lines", "--first", "2", "--max", "1", "--max-line-bytes", "4", "/app/f"); code != 0 || out != "line\n" ||
		meta != fmt.Sprintf(`{"ends_with_newline":false,"more":false,"size":%s,"total_lines":2}`+"\n", size) {
		t.Fatalf("lines: %d %q %q", code, out, meta)
	}
	if code, out, meta := run("", "file", "stat", "/app/dir"); code != 0 || out != `{"exists":true,"mode":"0755","size":0,"type":"directory"}`+"\n" || meta != "" {
		t.Fatalf("stat: %d %q %q", code, out, meta)
	}

	for _, test := range []struct {
		args []string
		want int
	}{
		{[]string{"file", "read", "/app/none"}, fileExit[codes.NotFound]},
		{[]string{"file", "stat", "app/relative"}, fileExit[codes.InvalidArgument]},
		{[]string{"file", "read", "/app/dir"}, fileExit[codes.FailedPrecondition]},
		{[]string{"file", "write", "--size", "3", "/app/f"}, fileExit[codes.InvalidArgument]},
		{[]string{"file", "write", "/app/f"}, transportFailureExit},
		{[]string{"file", "copy", "/app/f"}, transportFailureExit},
	} {
		if code, _, message := run("four", test.args...); code != test.want || message == "" {
			t.Fatalf("%q: exit %d, want %d (stderr %q)", test.args, code, test.want, message)
		}
	}
}

// summarylessClient answers ReadLines with a window and no summary, which a
// well-behaved bridge never sends.
type summarylessClient struct{ sandboxv1.SandboxClient }

type summarylessStream struct {
	grpc.ServerStreamingClient[sandboxv1.ReadLinesResponse]
	sent bool
}

func (summarylessClient) ReadLines(context.Context, *sandboxv1.ReadLinesRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[sandboxv1.ReadLinesResponse], error) {
	return &summarylessStream{}, nil
}

func (stream *summarylessStream) Recv() (*sandboxv1.ReadLinesResponse, error) {
	if stream.sent {
		return nil, io.EOF
	}
	stream.sent = true
	return &sandboxv1.ReadLinesResponse{Part: &sandboxv1.ReadLinesResponse_Chunk{Chunk: []byte("x\n")}}, nil
}

// Without its summary a window cannot be paginated, so the call fails with a
// code that is not a file status: the plugin sees 255 and discards stdout.
func TestClientRefusesALineStreamWithoutASummary(t *testing.T) {
	_, err := receiveLines(context.Background(), summarylessClient{}, &sandboxv1.ReadLinesRequest{}, io.Discard)
	if _, known := fileExit[status.Code(err)]; err == nil || known {
		t.Fatalf("receiveLines = %v, want a failure outside the file statuses", err)
	}
}
