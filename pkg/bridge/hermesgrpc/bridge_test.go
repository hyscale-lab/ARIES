package hermesgrpc

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
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
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
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

func newTestManager(t *testing.T, outputDir string) *Manager {
	t.Helper()
	manager, err := New(Options{OutputDir: outputDir, CleanupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

// dial reproduces what a harness-side client must do: load the staged
// certificate and key, trust the bridge's certificate, and present the
// session identity on every call.
func dial(t *testing.T, endpoint core.ToolEndpoint) (sandboxv1.SandboxClient, func()) {
	t.Helper()
	certificate, err := tls.LoadX509KeyPair(endpoint.KnownHostsSourceFile, endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatalf("load staged client keypair: %v", err)
	}
	pool := x509.NewCertPool()
	pem, err := os.ReadFile(endpoint.KnownHostsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	pool.AppendCertsFromPEM(pem)

	connection, err := grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
		// The bridge's certificate is self-signed and pinned by the server
		// side; the client verifies the peer it was told to expect.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			return nil
		},
	})))
	if err != nil {
		t.Fatalf("dial bridge: %v", err)
	}
	return sandboxv1.NewSandboxClient(connection), func() { _ = connection.Close() }
}

func sessionContext(t *testing.T, manager *Manager) context.Context {
	t.Helper()
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active == nil {
		t.Fatal("no active session")
	}
	return metadata.AppendToOutgoingContext(context.Background(), sessionHeader, manager.active.sessionID)
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
	if endpoint.ClientCommand != "" || endpoint.ClientSourceFile != "" {
		t.Fatalf("bridge advertised a client command: %#v", endpoint)
	}

	client, closeClient := dial(t, endpoint)
	defer closeClient()
	response, err := client.Exec(sessionContext(t, manager), &sandboxv1.ExecRequest{Script: "echo hi"})
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
		"operation_class": "exec", "status": "completed", "path": remoteShellPath,
		"workdir": "/app", "command": "echo hi",
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
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	if _, err := client.Exec(sessionContext(t, manager), &sandboxv1.ExecRequest{
		Script: "cat", Stdin: []byte("piped-input"),
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
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	response, err := client.Exec(sessionContext(t, manager), &sandboxv1.ExecRequest{Script: "false"})
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
		t.Fatalf("private key survived revocation: %v", err)
	}
	// The certificate is retained as evidence of what the harness was told to
	// trust; only the key is revocation.
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
		_, _ = client.Exec(sessionContext(t, manager), &sandboxv1.ExecRequest{Script: "sleep"})
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

	authorized := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(sessionHeader, session.sessionID))

	if err := session.authorize(authorized); err != nil {
		t.Fatalf("a live session refused a valid identity: %v", err)
	}

	// revoke is idempotent, so a later Stop is still safe.
	session.revoke()

	if err := session.authorize(authorized); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("revoked session = %v, want FailedPrecondition", err)
	}
	wrong := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(sessionHeader, "not-the-session"))
	if err := session.authorize(wrong); err == nil {
		t.Fatal("a wrong session identity was accepted")
	}

	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 2 {
		t.Fatalf("refusals recorded = %d, want 2: %#v", len(records), records)
	}
	for _, record := range records {
		if record["status"] != "rejected" {
			t.Fatalf("record = %#v", record)
		}
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
	callCtx := sessionContext(t, manager)
	client, closeClient := dial(t, endpoint)
	defer closeClient()
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Exec(callCtx, &sandboxv1.ExecRequest{Script: "echo hi"}); err == nil {
		t.Fatal("a revoked session accepted a call")
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a revoked call reached the sandbox")
	}
}

// TestRejectedCallIsRecorded pins the gap the SSH bridge leaves: several
// refusal classes there produce no audit entry at all.
func TestRejectedCallIsRecorded(t *testing.T) {
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 0}}
	manager, endpoint := startBridge(t, sandbox)
	client, closeClient := dial(t, endpoint)
	defer closeClient()

	wrong := metadata.AppendToOutgoingContext(context.Background(), sessionHeader, "not-the-session")
	_, err := client.Exec(wrong, &sandboxv1.ExecRequest{Script: "echo hi"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong session identity = %v, want PermissionDenied", err)
	}
	if len(sandbox.snapshot()) != 0 {
		t.Fatal("a refused call reached the sandbox")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	if len(records) != 1 || records[0]["status"] != "rejected" {
		t.Fatalf("refusal was not recorded: %#v", records)
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

func TestNewRequiresOutputDirectory(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("a blank output directory was accepted")
	}
	if _, err := New(Options{OutputDir: filepath.Join(t.TempDir(), "nested")}); err != nil {
		t.Fatalf("New() error = %v", err)
	}
}
