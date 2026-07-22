package openclawssh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const (
	contractStdin  = "tool-stdin-secret"
	contractStdout = "tool-stdout-secret"
	contractStderr = "tool-stderr-secret"
	contractEnv    = "tool-env-secret"
)

type contractSandbox struct {
	mu           sync.Mutex
	acceptTools  bool
	preparations []core.Command
	toolCommands []core.Command
	result       core.CommandResult
}

func (sandbox *contractSandbox) Exec(_ context.Context, command core.Command) (core.CommandResult, error) {
	command.Args = append([]string(nil), command.Args...)
	command.Stdin = append([]byte(nil), command.Stdin...)
	command.Env = maps.Clone(command.Env)
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if !sandbox.acceptTools {
		sandbox.preparations = append(sandbox.preparations, command)
		return core.CommandResult{}, nil
	}
	sandbox.toolCommands = append(sandbox.toolCommands, command)
	return sandbox.result, nil
}

func (sandbox *contractSandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	content, err := io.ReadAll(stdin)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	command.Stdin = content
	result, err := sandbox.Exec(ctx, command)
	if err == nil {
		_, _ = io.WriteString(stdout, result.Stdout)
		_, _ = io.WriteString(stderr, result.Stderr)
	}
	return result, err
}

func (*contractSandbox) Upload(context.Context, string, string) error   { return nil }
func (*contractSandbox) Download(context.Context, string, string) error { return nil }
func (*contractSandbox) ContainerID() string                            { return "sandbox-container-id" }
func (*contractSandbox) ContainerName() string                          { return "sandbox-container-name" }
func (*contractSandbox) NetworkName() string                            { return "sandbox-network-name" }
func (*contractSandbox) NetworkGateway(context.Context) (string, error) {
	return "127.0.0.1", nil
}
func (*contractSandbox) Workdir() string { return "/workspace" }
func (*contractSandbox) RunID() string   { return "contract-run" }
func (*contractSandbox) TaskID() string  { return "contract-task" }

func (sandbox *contractSandbox) enableToolCalls() {
	sandbox.mu.Lock()
	sandbox.acceptTools = true
	sandbox.mu.Unlock()
}

func (sandbox *contractSandbox) snapshot() []core.Command {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	return append([]core.Command(nil), sandbox.toolCommands...)
}

func TestManagerProxiesSSHExecToSandboxAndRetainsRedactedToolLog(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	sandbox := &contractSandbox{result: core.CommandResult{
		ExitCode: 42,
		Stdout:   contractStdout,
		Stderr:   contractStderr,
		Duration: 15 * time.Millisecond,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	assertDynamicLoopbackEndpoint(t, endpoint)
	configuration := bridgeClientConfig(t, endpoint)
	sandbox.enableToolCalls()

	client, err := ssh.Dial("tcp", endpoint.Address, configuration)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	session.Stdin = strings.NewReader(contractStdin)
	session.Stdout = &stdout
	session.Stderr = &stderr
	encoded := encodeCanonicalTokens([]string{remoteEnv, "LANG=" + contractEnv, remoteShell, "-c", "ignored-by-fake"})
	err = session.Run(encoded)
	_ = client.Close()
	var exitError *ssh.ExitError
	if !errors.As(err, &exitError) || exitError.ExitStatus() != 42 {
		t.Fatalf("SSH Run() error = %v, want exit 42", err)
	}
	if stdout.String() != contractStdout || stderr.String() != contractStderr {
		t.Fatalf("SSH streams = stdout %q stderr %q", stdout.String(), stderr.String())
	}

	commands := sandbox.snapshot()
	if len(commands) != 1 {
		t.Fatalf("sandbox tool Exec calls = %d, want exactly one: %#v", len(commands), commands)
	}
	want := core.Command{
		Path: remoteShell, Args: []string{"-c", "ignored-by-fake"}, Dir: "/workspace",
		Env: map[string]string{"LANG": contractEnv}, Stdin: []byte(contractStdin),
	}
	assertCommandIgnoringTimeout(t, commands[0], want)

	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if client, err := ssh.Dial("tcp", endpoint.Address, configuration); err == nil {
		_ = client.Close()
		t.Fatal("bridge listener still accepts the old task identity after Stop")
	}

	records, content := readToolCallRecords(t, outputDir)
	if len(records) != 1 {
		t.Fatalf("tool log records = %d, want one: %s", len(records), content)
	}
	record := records[0]
	assertLogString(t, record, "status", "completed")
	assertLogString(t, record, "operation_class", "exec")
	assertLogString(t, record, "run_id", sandbox.RunID())
	assertLogString(t, record, "task_id", sandbox.TaskID())
	assertLogString(t, record, "container_id", sandbox.ContainerID())
	assertLogString(t, record, "container_name", sandbox.ContainerName())
	assertLogString(t, record, "path", remoteShell)
	assertLogString(t, record, "workdir", sandbox.Workdir())
	assertLogNumber(t, record, "exit_code", 42)
	assertLogNumber(t, record, "stdin_bytes", len(contractStdin))
	assertLogNumber(t, record, "stdout_bytes", len(contractStdout))
	assertLogNumber(t, record, "stderr_bytes", len(contractStderr))
	if _, ok := record["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms missing or not numeric: %#v", record)
	}
	for _, secret := range []string{contractStdin, contractStdout, contractStderr, contractEnv} {
		if bytes.Contains(content, []byte(secret)) {
			t.Fatalf("tool log contains secret %q: %s", secret, content)
		}
	}
}

func TestManagerRejectsMalformedSSHExecWithoutSandboxExecution(t *testing.T) {
	outputDir := t.TempDir()
	manager := newContractManager(t, outputDir)
	sandbox := &contractSandbox{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	configuration := bridgeClientConfig(t, endpoint)
	sandbox.enableToolCalls()
	client, err := ssh.Dial("tcp", endpoint.Address, configuration)
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	const malformed = "rejection-command-secret"
	if err := session.Run(malformed); err == nil {
		t.Fatal("malformed SSH exec unexpectedly succeeded")
	}
	_ = client.Close()
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	commands := sandbox.snapshot()
	if len(commands) != 0 {
		t.Fatalf("malformed SSH exec reached sandbox: %#v", commands)
	}
	records, content := readToolCallRecords(t, outputDir)
	if len(records) != 1 {
		t.Fatalf("tool log records = %d, want one rejection: %s", len(records), content)
	}
	assertLogString(t, records[0], "status", "rejected")
	assertLogString(t, records[0], "container_id", sandbox.ContainerID())
	assertLogString(t, records[0], "container_name", sandbox.ContainerName())
	if message, _ := records[0]["error"].(string); message == "" {
		t.Fatalf("rejection log has no error metadata: %#v", records[0])
	}
	if bytes.Contains(content, []byte(malformed)) {
		t.Fatalf("rejection log contains raw malformed command: %s", content)
	}
}

func newContractManager(t *testing.T, outputDir string) *Manager {
	t.Helper()
	clientPath := filepath.Join(t.TempDir(), "aries-ssh")
	if err := os.WriteFile(clientPath, []byte("test client helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := New(Options{OutputDir: outputDir, ClientPath: clientPath, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func bridgeClientConfig(t *testing.T, endpoint core.ToolEndpoint) *ssh.ClientConfig {
	t.Helper()
	identity, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(identity)
	if err != nil {
		t.Fatal(err)
	}
	hostKeyCallback, err := knownhosts.New(endpoint.KnownHostsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	return &ssh.ClientConfig{
		User: endpoint.Username, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, HostKeyCallback: hostKeyCallback,
		Timeout: time.Second,
	}
}

func assertDynamicLoopbackEndpoint(t *testing.T, endpoint core.ToolEndpoint) {
	t.Helper()
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		t.Fatalf("endpoint address %q: %v", endpoint.Address, err)
	}
	if host != "127.0.0.1" || port == "" || port == "2222" {
		t.Fatalf("endpoint address = %q, want dynamic 127.0.0.1 port", endpoint.Address)
	}
	if endpoint.Protocol != "ssh" || endpoint.Username == "" || endpoint.IdentitySourceFile == "" || endpoint.KnownHostsSourceFile == "" {
		t.Fatalf("incomplete SSH endpoint: %#v", endpoint)
	}
}

func readToolCallRecords(t *testing.T, outputDir string) ([]map[string]any, []byte) {
	t.Helper()
	var matches []string
	err := filepath.WalkDir(outputDir, func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "tool-calls.jsonl" {
			matches = append(matches, path)
		}
		return err
	})
	if err != nil || len(matches) != 1 {
		t.Fatalf("find retained tool-calls.jsonl: matches=%v err=%v", matches, err)
	}
	content, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(matches[0]); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("tool-calls.jsonl mode = %v, %v", info, err)
	}
	var records []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		var record map[string]any
		if len(line) == 0 || json.Unmarshal(line, &record) != nil {
			t.Fatalf("invalid tool-calls.jsonl line %q", line)
		}
		records = append(records, record)
	}
	return records, content
}

func assertCommandIgnoringTimeout(t *testing.T, got, want core.Command) {
	t.Helper()
	got.Timeout, want.Timeout = 0, 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox Exec command = %#v, want %#v", got, want)
	}
}

func assertLogString(t *testing.T, record map[string]any, key, want string) {
	t.Helper()
	if got, ok := record[key].(string); !ok || got != want {
		t.Fatalf("log %s = %#v, want %q in %#v", key, record[key], want, record)
	}
}

func assertLogNumber(t *testing.T, record map[string]any, key string, want int) {
	t.Helper()
	if got, ok := record[key].(float64); !ok || int(got) != want {
		t.Fatalf("log %s = %#v, want %d in %#v", key, record[key], want, record)
	}
}
