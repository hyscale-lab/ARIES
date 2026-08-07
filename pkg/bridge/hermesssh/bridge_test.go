package hermesssh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
)

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

// clientConfig mirrors how Hermes actually authenticates: the generated
// identity, and TOFU on the host key because Hermes forces
// StrictHostKeyChecking=accept-new with no way to preload known_hosts.
func clientConfig(t *testing.T, endpoint core.ToolEndpoint) *ssh.ClientConfig {
	t.Helper()
	identity, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	return &ssh.ClientConfig{
		User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback:   ssh.InsecureIgnoreHostKey(),
		Timeout:           5 * time.Second,
	}
}

// runExec reproduces what OpenSSH sends: an `env` request on the channel
// followed by the `exec` request. The env request is the one that would kill
// every Hermes command if the server closed the channel on a non-exec request.
func runExec(t *testing.T, client *ssh.Client, payload string, stdin string) (string, string, error) {
	t.Helper()
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	_ = session.Setenv("LANG", "C.UTF-8")
	if stdin != "" {
		session.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr
	err = session.Run(payload)
	return stdout.String(), stderr.String(), err
}

// recordsOfType selects one request type from the audit. OpenSSH sends an `env`
// request before every exec and each one is recorded, so tests that care about
// commands filter rather than index into the whole stream.
func recordsOfType(records []map[string]any, requestType string) []map[string]any {
	var selected []map[string]any
	for _, record := range records {
		if record["request_type"] == requestType {
			selected = append(selected, record)
		}
	}
	return selected
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

func TestBridgeProxiesHermesCommandsAndRetainsEvidence(t *testing.T) {
	outputDir := t.TempDir()
	manager := newTestManager(t, outputDir)
	sandbox := &testSandbox{result: core.CommandResult{ExitCode: 7, Stdout: "tool-output", Stderr: "tool-diagnostic"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	// Hermes runs its own OpenSSH, so the bridge must not advertise a helper.
	if endpoint.ClientCommand != "" || endpoint.ClientSourceFile != "" {
		t.Fatalf("bridge advertised a client helper: %+v", endpoint)
	}
	if endpoint.Protocol != "ssh" || endpoint.Username != "aries" || endpoint.IdentitySourceFile == "" {
		t.Fatalf("endpoint = %+v", endpoint)
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.1" || port == "" || port == "22" {
		t.Fatalf("endpoint address = %q", endpoint.Address)
	}

	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	stdout, stderr, runErr := runExec(t, client, capturedAgentPayload, "")
	var exitErr *ssh.ExitError
	if runErr == nil {
		t.Fatal("expected the sandbox exit status to propagate")
	}
	if !errors.As(runErr, &exitErr) || exitErr.ExitStatus() != 7 {
		t.Fatalf("run error = %v", runErr)
	}
	if stdout != "tool-output" || stderr != "tool-diagnostic" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}

	commands := sandbox.snapshot()
	if len(commands) != 1 {
		t.Fatalf("commands = %#v", commands)
	}
	command := commands[0]
	if command.Path != remoteShellPath || len(command.Args) != 2 || command.Args[0] != "-c" {
		t.Fatalf("command = %#v", command)
	}
	if command.Dir != "/app" {
		t.Fatalf("command ran in %q, want the sandbox workdir", command.Dir)
	}
	if !strings.Contains(command.Args[1], "eval 'echo hello-from-agent && pwd'") {
		t.Fatalf("script was not decoded: %q", command.Args[1])
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	records := readToolCalls(t, filepath.Join(outputDir, "test-task", "bridge", "tool-calls.jsonl"))
	execs := recordsOfType(records, "exec")
	if len(execs) != 1 {
		t.Fatalf("exec records = %#v", records)
	}
	record := execs[0]
	if record["status"] != "completed" || record["operation_class"] != kindAgent || record["exit_code"].(float64) != 7 {
		t.Fatalf("record = %#v", record)
	}
	if record["container_id"] != "sandbox-container-id" || record["run_id"] != "test-run" || record["task_id"] != "test-task" {
		t.Fatalf("record identity = %#v", record)
	}
	// The audit is only lossless if the requests ARIES refuses appear too.
	envs := recordsOfType(records, "env")
	if len(envs) == 0 {
		t.Fatalf("no env request was recorded; audit dropped it: %#v", records)
	}
	for _, env := range envs {
		if env["status"] != "unsupported" || env["operation_class"] != kindUnknown {
			t.Fatalf("env record = %#v", env)
		}
	}
	raw, err := os.ReadFile(filepath.Join(outputDir, "test-task", "bridge", "ssh_raw.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "wire_command=") || !strings.Contains(string(raw), "ARIES SSH CALL BEGIN") {
		t.Fatalf("raw log is missing wire evidence:\n%s", raw)
	}
}

// The bootstrap probes must succeed: _establish_connection raises if its echo
// fails, and Hermes would never issue a single command.
func TestBridgeAnswersBootstrapProbes(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	sandbox := &testSandbox{result: core.CommandResult{Stdout: "/root\n"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)
	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, payload := range []string{connectionProbePayload, remoteHomePayload} {
		if _, _, err := runExec(t, client, payload, ""); err != nil {
			t.Fatalf("bootstrap %q failed: %v", payload, err)
		}
	}
	commands := sandbox.snapshot()
	if len(commands) != 2 {
		t.Fatalf("commands = %#v", commands)
	}
	for _, command := range commands {
		if command.Path != bootstrapShell || len(command.Args) != 2 || command.Args[0] != "-c" {
			t.Fatalf("bootstrap command = %#v", command)
		}
	}
	if commands[1].Args[1] != remoteHomePayload {
		t.Fatalf("remote home probe was rewritten: %q", commands[1].Args[1])
	}
}

// The core policy decision: file sync is refused, the sandbox never sees it,
// and the refusal is recorded as policy rather than a protocol error.
func TestBridgeDeniesFileSyncAndRecordsItAsPolicy(t *testing.T) {
	outputDir := t.TempDir()
	manager := newTestManager(t, outputDir)
	sandbox := &testSandbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if _, _, err := runExec(t, client, "mkdir -p /root/.hermes /root/.hermes/credentials", ""); err == nil {
		t.Fatal("file-sync mkdir was accepted")
	}
	if _, _, err := runExec(t, client, "tar xf - --no-overwrite-dir -C /root/.hermes", "archive-bytes"); err == nil {
		t.Fatal("file-sync tar was accepted")
	}
	if commands := sandbox.snapshot(); len(commands) != 0 {
		t.Fatalf("denied payloads still reached the sandbox: %#v", commands)
	}
	client.Close()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	records := recordsOfType(readToolCalls(t, filepath.Join(outputDir, "test-task", "bridge", "tool-calls.jsonl")), "exec")
	if len(records) != 2 {
		t.Fatalf("records = %#v", records)
	}
	for _, record := range records {
		if record["status"] != "denied" || !strings.Contains(record["error"].(string), "file sync is denied") {
			t.Fatalf("record = %#v", record)
		}
		// A refused sync must not be filed as an agent command; the evidence has
		// to distinguish ARIES policy from a command the agent actually ran.
		if record["operation_class"] != kindSync {
			t.Fatalf("denied sync recorded as %q, want %q", record["operation_class"], kindSync)
		}
	}
}

// The private identity is revoked on Stop, but known_hosts holds only the
// ephemeral host public key and is the evidence of what Hermes pinned.
func TestStopRevokesIdentityAndRetainsKnownHosts(t *testing.T) {
	outputDir := t.TempDir()
	manager := newTestManager(t, outputDir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, &testSandbox{})
	if err != nil {
		t.Fatal(err)
	}
	bridgeDir := filepath.Join(outputDir, "test-task", "bridge")
	knownHosts := filepath.Join(bridgeDir, "known_hosts")
	before, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("known_hosts is empty during the run")
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatalf("known_hosts did not survive Stop: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("known_hosts changed across Stop: %q -> %q", before, after)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private identity survived Stop: %v", err)
	}
}

// Hermes multiplexes every command onto one ControlMaster connection, so the
// server must serve many session channels concurrently on a single conn.
func TestBridgeServesConcurrentChannelsOnOneConnection(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	sandbox := &testSandbox{result: core.CommandResult{Stdout: "ok"}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)
	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	const parallel = 12
	var wait sync.WaitGroup
	errs := make(chan error, parallel)
	for index := range parallel {
		wait.Add(1)
		go func() {
			defer wait.Done()
			payload := "bash -c " + shlexQuote("echo command-"+string(rune('a'+index)))
			if _, _, err := runExec(t, client, payload, ""); err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent exec failed: %v", err)
	}
	if commands := sandbox.snapshot(); len(commands) != parallel {
		t.Fatalf("commands = %d, want %d", len(commands), parallel)
	}
}

func TestBridgePassesStdinThrough(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	sandbox := &testSandbox{result: core.CommandResult{Stdout: "read"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)
	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, _, err := runExec(t, client, "bash -c "+shlexQuote("cat > out"), "piped-input"); err != nil {
		t.Fatal(err)
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if len(sandbox.stdins) != 1 || string(sandbox.stdins[0]) != "piped-input" {
		t.Fatalf("stdin = %q", sandbox.stdins)
	}
}

func TestStopRevokesListenerAndIsIdempotent(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	sandbox := &testSandbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	// Capture the client config first: revocation deletes the identity file.
	configuration := clientConfig(t, endpoint)
	for attempt := range 3 {
		if err := manager.Stop(ctx); err != nil {
			t.Fatalf("stop %d: %v", attempt, err)
		}
	}
	if _, err := ssh.Dial("tcp", endpoint.Address, configuration); err == nil {
		t.Fatal("bridge still accepts connections after revocation")
	}
	// Private credential material must not survive revocation.
	if _, err := os.Stat(endpoint.IdentitySourceFile); !os.IsNotExist(err) {
		t.Fatalf("identity survived revocation: %v", err)
	}
}

// Revocation must terminate an in-flight tool call rather than wait for it.
func TestStopCancelsInFlightCommand(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	sandbox := &testSandbox{block: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ssh.Dial("tcp", endpoint.Address, clientConfig(t, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		_, _, _ = runExec(t, client, "bash -c "+shlexQuote("sleep forever"), "")
	}()
	<-started
	time.Sleep(200 * time.Millisecond)
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("stop during in-flight command: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight command was not terminated by revocation")
	}
}

func TestStartRejectsSecondSessionAndNonDockerSandbox(t *testing.T) {
	manager := newTestManager(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.Start(ctx, &testSandbox{}); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(ctx)
	if _, err := manager.Start(ctx, &testSandbox{}); err == nil {
		t.Fatal("second Start was accepted")
	}
}

func TestNewRequiresOutputDirectory(t *testing.T) {
	if _, err := New(Options{OutputDir: "  "}); err == nil {
		t.Fatal("blank output directory was accepted")
	}
}
