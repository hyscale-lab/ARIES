package hermesgrpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc/sandboxv1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testSandbox is copied from pkg/bridge/hermesssh/bridge_test.go:23-77. It
// implements bridgeSandbox with no transport dependency, including the block
// channel an in-flight cancellation test needs.
type testSandbox struct {
	mu       sync.Mutex
	commands []core.Command
	stdins   [][]byte
	result   core.CommandResult
	block    chan struct{}
}

func (sandbox *testSandbox) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	command.Args = append([]string(nil), command.Args...)
	command.Env = maps.Clone(command.Env)
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	sandbox.commands = append(sandbox.commands, command)
	return sandbox.result, nil
}

func (sandbox *testSandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	content, err := io.ReadAll(stdin)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	if sandbox.block != nil {
		select {
		case <-sandbox.block:
		case <-ctx.Done():
			return core.CommandResult{ExitCode: -1}, ctx.Err()
		}
	}
	sandbox.mu.Lock()
	sandbox.stdins = append(sandbox.stdins, content)
	sandbox.mu.Unlock()
	result, err := sandbox.Exec(ctx, command)
	if err == nil {
		_, _ = io.WriteString(stdout, result.Stdout)
		_, _ = io.WriteString(stderr, result.Stderr)
	}
	return result, err
}

func (*testSandbox) Upload(context.Context, string, string) error   { return nil }
func (*testSandbox) Download(context.Context, string, string) error { return nil }
func (*testSandbox) ContainerID() string                            { return "sandbox-container-id" }
func (*testSandbox) ContainerName() string                          { return "sandbox-container-name" }
func (*testSandbox) NetworkName() string                            { return "sandbox-network-name" }
func (*testSandbox) NetworkGateway(context.Context) (string, error) { return "127.0.0.1", nil }
func (*testSandbox) Workdir() string                                { return "/app" }
func (*testSandbox) RunID() string                                  { return "test-run" }
func (*testSandbox) TaskID() string                                 { return "test-task" }

func (sandbox *testSandbox) snapshot() []core.Command {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	return append([]core.Command(nil), sandbox.commands...)
}

// Payloads are Hermes-shaped because the bridge enforces Hermes's grammar:
// the client sends what OpenSSH would have put on the wire, and the bridge
// decodes it. A bare script is not a valid request.
const (
	agentPayload   = "bash -c 'echo hi'"
	catPayload     = "bash -c cat"
	falsePayload   = "bash -c false"
	sleepPayload   = "bash -c sleep"
	syncPayload    = "mkdir -p /root/.hermes /root/.hermes/credentials"
	garbagePayload = "curl http://example.invalid"
)

func fakeClient(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aries-grpc")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o555); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestManager(t *testing.T, outputDir string) *Manager {
	t.Helper()
	manager, err := New(Options{
		OutputDir: outputDir, CleanupTimeout: 5 * time.Second, ClientPath: fakeClient(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// dial goes through the shipped client's own credential loader, so the tests
// exercise the trust decision the staged binary actually makes rather than a
// permissive stand-in.
func dial(t *testing.T, endpoint core.ToolEndpoint) (sandboxv1.SandboxClient, func()) {
	t.Helper()
	transport, err := clientCredentials(endpoint.IdentitySourceFile, endpoint.KnownHostsSourceFile)
	if err != nil {
		t.Fatalf("load staged client credentials: %v", err)
	}
	connection, err := grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(transport))
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	return sandboxv1.NewSandboxClient(connection), func() { _ = connection.Close() }
}

func readToolCalls(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("tool-calls line is not JSON: %v", err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func startBridge(t *testing.T, sandbox *testSandbox) (*Manager, core.ToolEndpoint) {
	t.Helper()
	manager := newTestManager(t, t.TempDir())
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	return manager, endpoint
}

// TestBridgeProxiesCommandsAndRetainsEvidence mirrors the SSH bridge's
// equivalent: the script must reach the sandbox as /bin/bash -c in the
// sandbox workdir, output and exit code must propagate, and exactly one
// replayable record must be written.
func TestBridgeProxiesCommandsAndRetainsEvidence(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 7, Stdout: "out", Stderr: "err"}}
	manager, endpoint := startBridge(t, sandbox)

	if endpoint.Protocol != "grpc" || endpoint.Username != lockedUsername {
		t.Fatalf("endpoint = %#v", endpoint)
	}
	if endpoint.Network != "sandbox-network-name" {
		t.Fatalf("endpoint network = %q", endpoint.Network)
	}
	if endpoint.ClientCommand != clientContainerPath || endpoint.ClientSourceFile == "" {
		t.Fatalf("bridge did not advertise its staged client: %#v", endpoint)
	}
	if info, err := os.Stat(endpoint.ClientSourceFile); err != nil || info.Mode().Perm() != 0o555 {
		t.Fatalf("staged client = %v, %v", info, err)
	}

	client, closeClient := dial(t, endpoint)
	defer closeClient()
	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: agentPayload})
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if response.GetExitCode() != 7 {
		t.Fatalf("exit code = %d, want 7", response.GetExitCode())
	}
	if string(response.GetStdout()) != "out" || string(response.GetStderr()) != "err" {
		t.Fatalf("stdout = %q, stderr = %q", response.GetStdout(), response.GetStderr())
	}
	if response.GetReason() != sandboxv1.Reason_REASON_COMPLETED {
		t.Fatalf("reason = %v", response.GetReason())
	}

	commands := sandbox.snapshot()
	if len(commands) != 1 {
		t.Fatalf("sandbox saw %d commands", len(commands))
	}
	if commands[0].Path != remoteShellPath || commands[0].Dir != "/app" {
		t.Fatalf("command = %#v", commands[0])
	}
	if len(commands[0].Args) != 2 || commands[0].Args[0] != "-c" || commands[0].Args[1] != "echo hi" {
		t.Fatalf("args = %#v", commands[0].Args)
	}
	if commands[0].Env != nil {
		t.Fatalf("client environment reached the sandbox: %#v", commands[0].Env)
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	record := records[0]
	for field, want := range map[string]any{
		"operation_class": kindAgent, "status": "completed", "path": remoteShellPath,
		"workdir": "/app", "command": agentPayload,
		"run_id": "test-run", "task_id": "test-task",
		"container_id": "sandbox-container-id", "container_name": "sandbox-container-name",
	} {
		if record[field] != want {
			t.Fatalf("record[%q] = %v, want %v", field, record[field], want)
		}
	}
	if record["exit_code"] != float64(7) {
		t.Fatalf("record exit_code = %v", record["exit_code"])
	}
	// Command output must never enter the audit; only byte counts.
	if record["stdout_bytes"] != float64(3) || record["stderr_bytes"] != float64(3) {
		t.Fatalf("byte counts = %v/%v", record["stdout_bytes"], record["stderr_bytes"])
	}
	for _, forbidden := range []string{"stdout", "stderr"} {
		if _, present := record[forbidden]; present {
			t.Fatalf("record carries %q", forbidden)
		}
	}
}

func TestBridgePassesStdinThrough(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	_, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{
		Script: catPayload, Stdin: []byte("piped-input"),
	}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if len(sandbox.stdins) != 1 || string(sandbox.stdins[0]) != "piped-input" {
		t.Fatalf("stdin = %q", sandbox.stdins)
	}
}

// TestNonZeroExitIsNotAnRPCError pins the distinction the audit depends on:
// a command that failed is not a bridge that failed.
func TestNonZeroExitIsNotAnRPCError(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 42}}
	_, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: falsePayload})
	if err != nil {
		t.Fatalf("non-zero exit surfaced as an RPC error: %v", err)
	}
	if response.GetExitCode() != 42 || response.GetReason() != sandboxv1.Reason_REASON_COMPLETED {
		t.Fatalf("response = %#v", response)
	}
}

func TestStopRevokesAndIsIdempotent(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager := newTestManager(t, t.TempDir())
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 3 {
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatalf("Stop() attempt %d error = %v", attempt+1, err)
		}
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !os.IsNotExist(err) {
		t.Fatalf("client identity survived revocation: %v", err)
	}
	// The server certificate is retained as evidence of what the harness was
	// told to trust; removing the client identity is revocation.
	if _, err := os.Stat(endpoint.KnownHostsSourceFile); err != nil {
		t.Fatalf("certificate evidence was removed: %v", err)
	}
}

// TestStopCancelsInFlightCall pins that a blocked command is terminated
// rather than awaited, and that Stop still confirms revocation.
func TestStopCancelsInFlightCall(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}, block: make(chan struct{})}
	manager := newTestManager(t, t.TempDir())
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: sleepPayload})
	}()
	// Let the call reach the blocked sandbox before revoking.
	time.Sleep(200 * time.Millisecond)

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-callDone:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight call was awaited rather than terminated")
	}
}

// TestRevokedSessionRefusedAtTheGuard exercises the per-call session check
// directly. Going through the transport would prove nothing: Stop tears the
// server down, so the call fails at the connection before ever reaching the
// guard — which is exactly how an earlier version of this test passed while
// the guard was disabled.
func TestRevokedSessionRefusedAtTheGuard(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)

	manager.mu.Lock()
	session := manager.active
	manager.mu.Unlock()

	if err := session.authorize(agentPayload); err != nil {
		t.Fatalf("a live session refused a call: %v", err)
	}

	// revoke is idempotent, so a later Stop is still safe.
	session.revoke()

	if err := session.authorize(agentPayload); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoked session = %v, want FailedPrecondition", err)
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 {
		t.Fatalf("refusals recorded = %d, want 1: %#v", len(records), records)
	}
	if records[0]["status"] != "rejected" || records[0]["command"] != agentPayload {
		t.Fatalf("record = %#v", records[0])
	}
}

// TestRevokedTransportIsRefused covers the outer layer: after Stop the
// endpoint accepts nothing at all.
func TestRevokedTransportIsRefused(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager := newTestManager(t, t.TempDir())
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	callCtx := context.Background()
	client, closeClient := dial(t, endpoint)
	defer closeClient()
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec(callCtx, &sandboxv1.ExecRequest{Script: agentPayload}); err == nil {
		t.Fatal("a revoked session accepted a call")
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a revoked call reached the sandbox")
	}
}

// TestFileSyncIsDeniedAndRecorded is the policy gate the migration must not
// lose: Hermes's ~/.hermes sync carries harness scaffold and the credential
// files iter_sync_files collects, and the container it targets is the one the
// verifier later inspects. The refusal is enforced on the server, so a client
// that skipped the shim could not route around it.
func TestFileSyncIsDeniedAndRecorded(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	_, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: syncPayload})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("file sync = %v, want PermissionDenied", err)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a denied sync reached the sandbox")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	// The verbatim payload must survive. The SSH bridge keeps only a hash here
	// and puts the bytes in ssh_raw.log, which this bridge does not write.
	for field, want := range map[string]any{
		"operation_class": kindSync, "status": "denied", "command": syncPayload,
	} {
		if records[0][field] != want {
			t.Fatalf("record[%q] = %v, want %v", field, records[0][field], want)
		}
	}
}

// TestUndecodablePayloadIsRejectedAndRecorded covers the other refusal class.
func TestUndecodablePayloadIsRejectedAndRecorded(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	_, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: garbagePayload})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("undecodable payload = %v, want InvalidArgument", err)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("an undecodable payload reached the sandbox")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	if records[0]["operation_class"] != kindUnknown || records[0]["status"] != "rejected" {
		t.Fatalf("record = %#v", records[0])
	}
	if records[0]["command"] != garbagePayload {
		t.Fatalf("refused payload was not retained: %#v", records[0])
	}
}

// TestBootstrapProbeRunsThroughPOSIXShell pins that the two probes replay
// literally through /bin/sh, so `echo $HOME` reports the sandbox's own home
// rather than a value ARIES invents.
func TestBootstrapProbeRunsThroughPOSIXShell(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0, Stdout: "/root"}}
	_, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: remoteHomePayload}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	commands := sandbox.snapshot()
	if len(commands) != 1 || commands[0].Path != bootstrapShell {
		t.Fatalf("command = %#v", commands)
	}
	if len(commands[0].Args) != 2 || commands[0].Args[1] != remoteHomePayload {
		t.Fatalf("args = %#v", commands[0].Args)
	}
}

// TestBinaryStdinIsRetainedInTheRecord covers what the dropped raw log used to
// hold. JSON cannot carry arbitrary bytes, so they are base64 in their own
// field and the note names it rather than claiming nothing was kept.
func TestBinaryStdinIsRetainedInTheRecord(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	binary := []byte{0x00, 0x01, 0x02, 0xff}
	if _, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{
		Script: catPayload, Stdin: binary,
	}); err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 {
		t.Fatalf("records = %#v", records)
	}
	if records[0]["stdin_encoding"] != "binary-omitted" {
		t.Fatalf("stdin_encoding = %v", records[0]["stdin_encoding"])
	}
	raw, _ := records[0]["stdin_raw"].(string)
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("stdin_raw is not base64: %v (%q)", err, raw)
	}
	if !bytes.Equal(decoded, binary) {
		t.Fatalf("stdin_raw = % x, want % x", decoded, binary)
	}
	note, _ := records[0]["stdin"].(string)
	if !strings.Contains(note, "stdin_raw") {
		t.Fatalf("note does not name the field holding the bytes: %q", note)
	}
}

// TestOutputBeyondTheLimitTruncates exercises the bound. The shipped limit is
// unlimited, so the path is driven through Options rather than left untested
// until a number is chosen from real runs.
func TestOutputBeyondTheLimitTruncates(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0, Stdout: "0123456789", Stderr: "abcdefghij"}}
	manager, err := New(Options{
		OutputDir: t.TempDir(), CleanupTimeout: 5 * time.Second,
		ClientPath: fakeClient(t), OutputLimit: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := manager.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	response, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{Script: agentPayload})
	if err != nil {
		t.Fatalf("output past the bound failed the RPC instead of truncating: %v", err)
	}
	if !response.GetTruncated() {
		t.Fatal("truncated was not reported")
	}
	if string(response.GetStdout()) != "0123" || string(response.GetStderr()) != "abcd" {
		t.Fatalf("stdout = %q, stderr = %q", response.GetStdout(), response.GetStderr())
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	// The counts report everything the sandbox produced, not what was kept.
	if records[0]["stdout_bytes"] != float64(10) || records[0]["truncated"] != true {
		t.Fatalf("record = %#v", records[0])
	}
}

func TestStartRejectsSecondSessionAndNonDockerSandbox(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	if _, err := manager.Start(context.Background(), &testSandbox{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	if _, err := manager.Start(context.Background(), &testSandbox{}); err == nil {
		t.Fatal("a second Start was accepted")
	}

	// A sandbox that satisfies only runner.Sandbox must be refused, since the
	// bridge needs the local capability set. The SSH package names this case
	// but never exercises it.
	other := newTestManager(t, t.TempDir())
	if _, err := other.Start(context.Background(), plainSandbox{}); err == nil {
		t.Fatal("a sandbox without the local capability was accepted")
	}
}

type plainSandbox struct{}

func (plainSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}
func (plainSandbox) Upload(context.Context, string, string) error   { return nil }
func (plainSandbox) Download(context.Context, string, string) error { return nil }

func TestNewRequiresOutputDirectoryAndClient(t *testing.T) {
	if _, err := New(Options{ClientPath: fakeClient(t)}); err == nil {
		t.Fatal("a blank output directory was accepted")
	}
	if _, err := New(Options{OutputDir: t.TempDir()}); err == nil {
		t.Fatal("a blank client path was accepted")
	}
	if _, err := New(Options{
		OutputDir: filepath.Join(t.TempDir(), "nested"), ClientPath: fakeClient(t),
	}); err != nil {
		t.Fatalf("New() error = %v", err)
	}
}

// These certificates carry the RFC 5280 no-expiration date. Nothing consults a
// validity window — pinning replaces chain validation on both sides — so any
// finite lifetime would be decorative today and a live failure for long tasks
// the moment anyone enabled standard verification. This guards against
// reintroducing one; the credential's real bound is Stop removing the identity.
func TestGeneratedCertificatesDoNotExpire(t *testing.T) {
	material, err := generateSessionCertificates("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	for name, pem := range map[string][]byte{"server": material.trusted, "client": material.identity} {
		certificate, err := parseCertificate(pem)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !certificate.NotAfter.Equal(noExpiry) {
			t.Fatalf("%s certificate expires at %s, want the RFC 5280 no-expiration date %s",
				name, certificate.NotAfter, noExpiry)
		}
		// The zero time encodes as year one, which reads as long expired rather
		// than as never expiring.
		if certificate.NotBefore.IsZero() || certificate.NotBefore.After(time.Now()) {
			t.Fatalf("%s certificate is not yet valid: NotBefore = %s", name, certificate.NotBefore)
		}
	}
}

// The transport cap must never fire before truncation does. If it did, a
// command would run to completion and then have its reply rejected — which is
// precisely what grpc-go's 4 MiB receive default would have caused, and what
// the truncation path exists to avoid. Both streams can be full at once, so
// the backstop has to clear twice the retained bound plus framing.
func TestTruncationBindsBeforeTheTransportCap(t *testing.T) {
	worstCaseReply := 2 * defaultOutputLimit
	if int64(maxMessageBytes) <= worstCaseReply {
		t.Fatalf("maxMessageBytes = %d does not clear two truncated streams (%d)",
			maxMessageBytes, worstCaseReply)
	}
	// Framing, the exit code and the reason are small, but the margin should be
	// comfortable rather than exact.
	if margin := int64(maxMessageBytes) - worstCaseReply; margin < defaultOutputLimit {
		t.Fatalf("margin above the worst-case reply is only %d bytes", margin)
	}
}

// Oversized stdin must be refused before the command runs. The SSH bridge can
// only notice mid-stream, after the sandbox has already executed, and latches
// an audit error that blocks revocation; here the whole input is in hand up
// front, so nothing runs and the refusal is recorded instead.
func TestOversizedStdinIsRefusedBeforeExecution(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	_, err := client.Exec(context.Background(), &sandboxv1.ExecRequest{
		Script: catPayload, Stdin: make([]byte, maxRecordedInputBytes+1),
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("oversized stdin = %v, want ResourceExhausted", err)
	}
	if commands := sandbox.snapshot(); len(commands) != 0 {
		t.Fatalf("the command ran anyway: %#v", commands)
	}
	// Revocation must still confirm: this is a refused request, not a failure
	// to retain evidence.
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["status"] != "rejected" {
		t.Fatalf("refusal was not recorded: %#v", records)
	}
}
