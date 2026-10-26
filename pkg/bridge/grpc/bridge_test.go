package grpc

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"testing/fstest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestExecRunsBashInTheSandboxWorkdir pins the exact argv: the script reaches
// the sandbox as /bin/bash -c, or -l -c for a login shell, in the sandbox
// workdir. Output and exit code propagate, and one record is written per call.
func TestExecRunsBashInTheSandboxWorkdir(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.exitCode, executor.stdout, executor.stderr = 7, []byte("out"), []byte("err")
	manager, client, endpoint := startBridge(t, executor, Options{})

	if endpoint.Protocol != "grpc" || endpoint.Workdir != testWorkdir || endpoint.Username != "aries" {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	for _, login := range []bool{false, true} {
		response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "echo hi", Login: login})
		if err != nil {
			t.Fatalf("Exec(login=%v) error = %v", login, err)
		}
		if response.GetExitCode() != 7 || response.GetReason() != sandboxv1.Reason_REASON_COMPLETED {
			t.Fatalf("response = %#v", response)
		}
		if string(response.GetStdout()) != "out" || string(response.GetStderr()) != "err" {
			t.Fatalf("stdout = %q, stderr = %q", response.GetStdout(), response.GetStderr())
		}
	}
	commands, _, _ := executor.snapshot()
	want := [][]string{{"-c", "echo hi"}, {"-l", "-c", "echo hi"}}
	if len(commands) != len(want) {
		t.Fatalf("sandbox saw %d commands", len(commands))
	}
	for index, command := range commands {
		if command.Path != bridgekit.RemoteShellPath || command.Dir != testWorkdir || !slices.Equal(command.Args, want[index]) || command.Env != nil {
			t.Fatalf("command %d = %#v", index, command)
		}
	}

	stop(t, manager)
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != len(want) {
		t.Fatalf("records = %d, want %d", len(records), len(want))
	}
	for field, value := range map[string]any{
		"operation_class": "agent", "status": "completed", "path": bridgekit.RemoteShellPath,
		"workdir": testWorkdir, "command": "echo hi", "command_hash": bridgekit.CommandHash("echo hi"),
		"run_id": "test-run", "task_id": "test-task", "sandbox_id": "test-sandbox",
		"container_id": "sandbox-container-id", "container_name": "sandbox-container-name",
		"exit_code": float64(7), "stdout_bytes": float64(3), "stderr_bytes": float64(3),
	} {
		if records[1][field] != value {
			t.Fatalf("record[%q] = %v, want %v", field, records[1][field], value)
		}
	}
	argv, _ := records[1]["argv"].([]any)
	if len(argv) != 4 || argv[1] != "-l" {
		t.Fatalf("record argv = %#v", records[1]["argv"])
	}
	// Command output never enters the audit; only byte counts.
	for _, forbidden := range []string{"stdout", "stderr", "truncated"} {
		if _, present := records[1][forbidden]; present {
			t.Fatalf("record carries %q", forbidden)
		}
	}
}

func TestExecPassesStdinThrough(t *testing.T) {
	executor := newFakeExecutor(nil)
	_, client, _ := startBridge(t, executor, Options{})
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "cat", Stdin: []byte("piped-input")}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if _, stdins, _ := executor.snapshot(); len(stdins) != 1 || string(stdins[0]) != "piped-input" {
		t.Fatalf("stdin = %q", stdins)
	}
}

// TestNonZeroExitIsNotAnRPCError pins the distinction the audit depends on:
// a command that failed is not a bridge that failed.
func TestNonZeroExitIsNotAnRPCError(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.exitCode = 42
	_, client, _ := startBridge(t, executor, Options{})
	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "false"})
	if err != nil {
		t.Fatalf("non-zero exit surfaced as an RPC error: %v", err)
	}
	if response.GetExitCode() != 42 || response.GetReason() != sandboxv1.Reason_REASON_COMPLETED {
		t.Fatalf("response = %#v", response)
	}
}

// Oversized stdin is refused before the command runs: the whole input is in
// hand up front, so nothing runs and the refusal is the evidence.
func TestOversizedStdinIsRefusedBeforeExecution(t *testing.T) {
	executor := newFakeExecutor(nil)
	manager, client, endpoint := startBridge(t, executor, Options{})
	_, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "cat", Stdin: make([]byte, bridgekit.MaxRecordedInputBytes+1)})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized stdin = %v, want ResourceExhausted", err)
	}
	if commands, _, _ := executor.snapshot(); len(commands) != 0 {
		t.Fatalf("the command ran anyway: %#v", commands)
	}
	// Revocation still confirms: this is a refused request, not a failure to
	// retain evidence.
	stop(t, manager)
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["status"] != "rejected" || records[0]["command"] != "cat" || records[0]["exit_code"] != float64(-1) {
		t.Fatalf("refusal was not recorded: %#v", records)
	}
}

// A workdir that fails the Hermes rule is refused before anything runs.
func TestExecRefusesAWorkdirThatIsNotShellNeutral(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.workdir = "/app dir"
	manager, client, endpoint := startBridge(t, executor, Options{})
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "true"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Exec() = %v, want FailedPrecondition", err)
	}
	if commands, _, _ := executor.snapshot(); len(commands) != 0 {
		t.Fatalf("the command ran anyway: %#v", commands)
	}
	stop(t, manager)
	if records := readToolCalls(t, endpoint.LogPaths[0]); len(records) != 1 || records[0]["status"] != "rejected" {
		t.Fatalf("refusal was not recorded: %#v", records)
	}
}

// Output past the bound is cut, not failed: the reply holds the bound, and the
// record keeps the true byte counts.
func TestOutputBeyondTheLimitTruncates(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.stdout = bytes.Repeat([]byte{'o'}, defaultOutputLimit+1)
	executor.stderr = []byte("err")
	manager, client, endpoint := startBridge(t, executor, Options{})
	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "yes"})
	if err != nil {
		t.Fatalf("output past the bound failed the RPC instead of truncating: %v", err)
	}
	if !response.GetTruncated() || len(response.GetStdout()) != defaultOutputLimit || string(response.GetStderr()) != "err" {
		t.Fatalf("truncated = %v, stdout = %d bytes, stderr = %q", response.GetTruncated(), len(response.GetStdout()), response.GetStderr())
	}
	stop(t, manager)
	records := readToolCalls(t, endpoint.LogPaths[0])
	if records[0]["stdout_bytes"] != float64(defaultOutputLimit+1) || records[0]["truncated"] != true {
		t.Fatalf("record = %#v", records[0])
	}
}

// The transport cap must never fire before truncation does. Both streams can
// be full at once, so the cap has to clear twice the bound plus framing.
func TestTruncationBindsBeforeTheTransportCap(t *testing.T) {
	worstCaseReply := 2 * defaultOutputLimit
	if margin := maxMessageBytes - worstCaseReply; margin < defaultOutputLimit {
		t.Fatalf("maxMessageBytes = %d leaves only %d bytes above two truncated streams", maxMessageBytes, margin)
	}
}

// TestRevokedSessionRefusesEveryRPC marks the session revoked but keeps the
// transport up, so each call reaches the guard. Through a stopped server a
// call fails at the connection and proves nothing about the guard.
func TestRevokedSessionRefusesEveryRPC(t *testing.T) {
	executor := newFakeExecutor(nil)
	manager, client, endpoint := startBridge(t, executor, Options{})
	manager.active.revoked.Store(true)

	ctx := context.Background()
	_, execErr := client.Exec(ctx, &sandboxv1.ExecRequest{Script: "true"})
	_, statErr := client.Stat(ctx, &sandboxv1.StatRequest{Path: "/app/f"})
	_, _, readErr := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/f"})
	_, _, linesErr := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/f", FirstLine: 1, MaxLines: 1})
	_, writeErr := writeFile(client, "/app/f", 1, []byte("x"))
	for name, err := range map[string]error{"Exec": execErr, "Stat": statErr, "ReadFile": readErr, "ReadLines": linesErr, "WriteFile": writeErr} {
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("%s after revocation = %v, want Unavailable", name, err)
		}
	}
	if commands, _, fileOps := executor.snapshot(); len(commands) != 0 || fileOps != 0 {
		t.Fatalf("a revoked call reached the sandbox: %d commands, %d file operations", len(commands), fileOps)
	}

	stop(t, manager)
	records := readToolCalls(t, endpoint.LogPaths[0])
	var classes []string
	for _, record := range records {
		if record["status"] != "rejected" || record["error"] != "session revoked" {
			t.Fatalf("record = %#v", record)
		}
		classes = append(classes, record["operation_class"].(string))
	}
	if !slices.Equal(classes, []string{"agent", "Stat", "ReadFile", "ReadLines", "WriteFile"}) {
		t.Fatalf("refusals recorded = %v", classes)
	}
}

// After Stop the endpoint accepts nothing at all.
func TestStoppedTransportIsRefused(t *testing.T) {
	executor := newFakeExecutor(nil)
	manager, client, endpoint := startBridge(t, executor, Options{})
	stop(t, manager)
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "true"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("Exec() after Stop = %v, want Unavailable", err)
	}
	if connection, err := net.DialTimeout("tcp", endpoint.Address, time.Second); err == nil {
		_ = connection.Close()
		t.Fatal("bridge listener still accepts connections after Stop")
	}
	if commands, _, _ := executor.snapshot(); len(commands) != 0 {
		t.Fatal("a call after Stop reached the sandbox")
	}
}

// A call that passes its deadline ends on the client with DeadlineExceeded
// and is recorded as canceled, as any canceled call.
func TestExecPastItsDeadlineIsRecordedAsCanceled(t *testing.T) {
	const deadline = 50 * time.Millisecond
	executor := newFakeExecutor(nil)
	executor.block = true
	manager, client, endpoint := startBridge(t, executor, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if _, err := client.Exec(ctx, &sandboxv1.ExecRequest{Script: "sleep infinity"}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Exec = %v, want DeadlineExceeded", err)
	}
	stop(t, manager)
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["status"] != "canceled" || records[0]["exit_code"] != float64(255) {
		t.Fatalf("records = %#v", records)
	}
}

// terminationLag is long against a local handler return and short against
// the test timeout.
const terminationLag = 200 * time.Millisecond

// Stop cancels a blocked call rather than awaiting it, and waits for its
// handler before it seals the audit, so the canceled call is still recorded.
func TestStopCancelsInFlightCallAndRecordsIt(t *testing.T) {
	executor := newFakeExecutor(nil)
	executor.block = true
	// The lag keeps the handler running after cancellation, so a Stop that
	// sealed the audit first would lose this record.
	executor.lag = terminationLag
	manager, client, endpoint := startBridge(t, executor, Options{})

	callDone := make(chan error, 1)
	go func() {
		_, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: "sleep infinity"})
		callDone <- err
	}()
	for {
		if commands, _, _ := executor.snapshot(); len(commands) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop(t, manager)
	select {
	case <-callDone:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call was awaited rather than terminated")
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["status"] != "canceled" || records[0]["exit_code"] != float64(255) {
		t.Fatalf("records = %#v", records)
	}
}

func TestStopIsIdempotentAndTheManagerIsNotReused(t *testing.T) {
	manager, _, _ := startBridge(t, newFakeExecutor(nil), Options{})
	for attempt := range 3 {
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatalf("Stop() attempt %d error = %v", attempt+1, err)
		}
	}
	if _, err := manager.StartTarget(context.Background(), newFakeExecutor(nil)); err == nil {
		t.Fatal("a revoked manager started again")
	}

	early := newTestManager(t, Options{})
	stop(t, early)
	if _, err := early.StartTarget(context.Background(), newFakeExecutor(nil)); err == nil {
		t.Fatal("a manager stopped before start was used")
	}
}

func TestStartTargetRefusesASecondTarget(t *testing.T) {
	manager, _, _ := startBridge(t, newFakeExecutor(nil), Options{})
	if _, err := manager.StartTarget(context.Background(), newFakeExecutor(nil)); err == nil {
		t.Fatal("a second StartTarget was accepted")
	}
}

func TestNewRequiresItsInputs(t *testing.T) {
	valid := Options{SandboxID: "sandbox", ResolveListen: loopbackListen, OutputDir: filepath.Join(t.TempDir(), "nested")}
	if _, err := New(valid); err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for name, change := range map[string]func(*Options){
		"sandbox ID": func(options *Options) { options.SandboxID = "" },
		"listen":     func(options *Options) { options.ResolveListen = nil },
		"output":     func(options *Options) { options.OutputDir = " " },
	} {
		options := valid
		change(&options)
		if _, err := New(options); err == nil {
			t.Fatalf("New() accepted a missing %s", name)
		}
	}
}

// The raw log exists only when retained and is private. It keeps the Exec
// script, stdin and written content, never read content.
func TestRawLogIsRetainedOnlyWhenAsked(t *testing.T) {
	for _, omit := range []bool{true, false} {
		executor := newFakeExecutor(nil)
		manager, client, endpoint := startBridge(t, executor, Options{OmitRawLog: omit})
		ctx := context.Background()
		if _, err := client.Exec(ctx, &sandboxv1.ExecRequest{Script: "cat > /tmp/x", Stdin: []byte("exec-stdin"), Login: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := writeFile(client, "/app/f", int64(len("written-content")), []byte("written-content")); err != nil {
			t.Fatal(err)
		}
		executor.files["app/secret"] = &fstest.MapFile{Data: []byte("read-content"), Mode: 0o644}
		if _, _, err := readFile(client, &sandboxv1.ReadFileRequest{Path: "/app/secret"}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := readWindow(client, &sandboxv1.ReadLinesRequest{Path: "/app/secret", FirstLine: 1, MaxLines: 1}); err != nil {
			t.Fatal(err)
		}
		stop(t, manager)

		rawPath := filepath.Join(filepath.Dir(endpoint.LogPaths[0]), "grpc_raw.log")
		info, err := os.Stat(rawPath)
		if omit {
			if !os.IsNotExist(err) || len(endpoint.LogPaths) != 1 {
				t.Fatalf("omitted raw log: stat = %v, log paths = %v", err, endpoint.LogPaths)
			}
			continue
		}
		if err != nil || info.Mode().Perm() != 0o600 || !slices.Equal(endpoint.LogPaths, []string{endpoint.LogPaths[0], rawPath}) {
			t.Fatalf("raw log: info = %v, err = %v, log paths = %v", info, err, endpoint.LogPaths)
		}
		content, err := os.ReadFile(rawPath)
		if err != nil {
			t.Fatal(err)
		}
		records := decodeRawGRPCRecords(t, content)
		if len(records) != 4 {
			t.Fatalf("raw records = %#v", records)
		}
		exec, write, read, lines := records[0], records[1], records[2], records[3]
		if exec["operation_class"] != "agent" || exec["script"] != "cat > /tmp/x" || exec["login"] != "true" || exec["stdin"] != "exec-stdin" || exec["stdin_bytes"] != strconv.Itoa(len("exec-stdin")) {
			t.Fatalf("raw exec = %#v", exec)
		}
		if write["operation_class"] != "WriteFile" || write["path"] != "/app/f" || write["stdin"] != "written-content" || write["sha256"] == "" {
			t.Fatalf("raw write = %#v", write)
		}
		for _, record := range []map[string]string{read, lines} {
			if record["path"] != "/app/secret" || record["read_bytes"] != strconv.Itoa(len("read-content")) || record["sha256"] == "" || record["stdin"] != "" {
				t.Fatalf("raw read = %#v", record)
			}
		}
		if bytes.Contains(content, []byte("read-content")) {
			t.Fatalf("raw log holds read content: %s", content)
		}
		if exec["sequence"] != "1" || exec["sandbox_id"] != "test-sandbox" || exec["container_id"] != "sandbox-container-id" || exec["status"] != "completed" {
			t.Fatalf("raw exec identity = %#v", exec)
		}
	}
}

func decodeRawGRPCRecords(t *testing.T, content []byte) []map[string]string {
	t.Helper()
	const begin = "--- ARIES GRPC CALL BEGIN ---\n"
	const end = "--- ARIES GRPC CALL END ---\n"
	fields := []string{"sequence", "sandbox_id", "timestamp", "operation_class", "status", "run_id", "task_id", "container_id", "path", "login", "script", "stdin_bytes", "stdin", "read_bytes", "sha256"}
	var records []map[string]string
	for len(content) > 0 {
		rest, ok := bytes.CutPrefix(content, []byte(begin))
		if !ok {
			t.Fatalf("raw log missing begin delimiter: %q", content)
		}
		record := make(map[string]string, len(fields))
		for _, field := range fields {
			line, after, ok := bytes.Cut(rest, []byte{'\n'})
			value, found := bytes.CutPrefix(line, []byte(field+"="))
			if !ok || !found {
				t.Fatalf("raw log field order: got %q want %s", line, field)
			}
			record[field] = string(value)
			rest = after
		}
		if content, ok = bytes.CutPrefix(rest, []byte(end)); !ok {
			t.Fatalf("raw log missing end delimiter: %q", rest)
		}
		records = append(records, record)
	}
	return records
}
