//go:build integration

package hermesssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// bridgeFixtureImage backs the deterministic end-to-end test. It is pinned by
// digest and needs nothing from Hermes: a Debian base is used because the
// bridge resolves the bare `bash` token Hermes puts on the wire to
// /bin/bash, so a busybox fixture could not run an agent command at all.
const bridgeFixtureImage = "docker.io/library/debian:12-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241"

// hermesIntegrationImage is the upstream image ARIES is pinned to. It is read
// from configs/versions.json rather than restated here so this test always
// runs against the same Hermes the experiments do: when the pin moves, this
// test reports whether the new upstream still drives the bridge instead of
// re-confirming that a stale image once did. It is used unmodified — the point
// of the test is that no patch layer is required.
func hermesIntegrationImage(t *testing.T) string {
	t.Helper()
	versions, err := config.LoadVersions(filepath.Join(repositoryRoot(t), "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if versions.Hermes.Image == "" {
		t.Fatal("configs/versions.json carries no Hermes image pin")
	}
	return versions.Hermes.Image
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root was not found")
		}
		current = parent
	}
}

// driverScript exercises Hermes's own terminal tool, which is the exact path
// the agent takes when it runs a command. It needs no model.
const driverScript = `import json, sys
from tools.terminal_tool import _get_env_config, terminal_tool

config = _get_env_config()
assert config["env_type"] == "ssh", config["env_type"]
print("CONFIG " + json.dumps({
    "env_type": config["env_type"],
    "host": config["ssh_host"],
    "port": config["ssh_port"],
    "user": config["ssh_user"],
    "cwd": config["cwd"],
}))
result = terminal_tool(command="echo aries-integration-ok && pwd")
# terminal_tool already returns a JSON document; re-encoding would double-escape it.
print("TOOL " + (result if isinstance(result, str) else json.dumps(result)))
`

type integrationSandbox struct {
	mu       sync.Mutex
	commands []core.Command
	workdir  string
}

func (sandbox *integrationSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, errors.New("unused")
}

// ExecStream runs the translated command on the host, standing in for the
// Docker sandbox. The bridge's own behaviour is what is under test.
func (sandbox *integrationSandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	sandbox.mu.Lock()
	recorded := command
	recorded.Args = append([]string(nil), command.Args...)
	sandbox.commands = append(sandbox.commands, recorded)
	sandbox.mu.Unlock()

	process := exec.CommandContext(ctx, command.Path, command.Args...)
	process.Dir = sandbox.workdir
	process.Stdin = stdin
	process.Stdout = stdout
	process.Stderr = stderr
	err := process.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return core.CommandResult{ExitCode: exitErr.ExitCode()}, nil
	}
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	return core.CommandResult{ExitCode: 0}, nil
}

func (*integrationSandbox) Upload(context.Context, string, string) error   { return nil }
func (*integrationSandbox) Download(context.Context, string, string) error { return nil }
func (*integrationSandbox) ContainerID() string                            { return "integration-container" }
func (*integrationSandbox) ContainerName() string                          { return "integration-container" }
func (*integrationSandbox) NetworkName() string                            { return "host" }
func (*integrationSandbox) NetworkGateway(context.Context) (string, error) { return "127.0.0.1", nil }
func (sandbox *integrationSandbox) Workdir() string                        { return sandbox.workdir }
func (*integrationSandbox) RunID() string                                  { return "integration-run" }
func (*integrationSandbox) TaskID() string                                 { return "integration-task" }

func (sandbox *integrationSandbox) snapshot() []core.Command {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	return append([]core.Command(nil), sandbox.commands...)
}

func requireDocker(t *testing.T, image string) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", image).CombinedOutput(); err != nil {
		t.Skipf("pinned Hermes image is not present locally (%s): %s", image, output)
	}
}

// TestUpstreamHermesDrivesTheBridgeWithoutPatches is the load-bearing check for
// this integration: an unmodified upstream Hermes, configured only through
// environment variables, runs a tool call through the ARIES bridge into the
// sandbox, while its ~/.hermes file sync is refused and never reaches the
// sandbox.
func TestUpstreamHermesDrivesTheBridgeWithoutPatches(t *testing.T) {
	image := hermesIntegrationImage(t)
	requireDocker(t, image)
	outputDir := t.TempDir()
	sandboxRoot := t.TempDir()
	manager := newTestManager(t, outputDir)
	sandbox := &integrationSandbox{workdir: sandboxRoot}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}

	driverPath := filepath.Join(t.TempDir(), "driver.py")
	if err := os.WriteFile(driverPath, []byte(driverScript), 0o644); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(t.TempDir(), "id_ed25519")
	identity, err := os.ReadFile(endpoint.IdentitySourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identityPath, identity, 0o600); err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		t.Fatal(err)
	}

	// Plant a skill file so Hermes has something real to try to sync; its
	// contents are the canary for a sandbox leak.
	const canary = "aries-file-sync-canary"
	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "skill.md"), []byte(canary+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	arguments := []string{
		"run", "--rm", "--network", "host",
		"-v", driverPath + ":/driver.py:ro",
		"-v", identityPath + ":/run/aries/ssh/id_ed25519:ro",
		"-v", skillDir + ":/run/aries/hermes/skills/demo:ro",
		"-e", "HERMES_HOME=/run/aries/hermes",
		"-e", "TERMINAL_ENV=ssh",
		"-e", "TERMINAL_SSH_HOST=" + host,
		"-e", "TERMINAL_SSH_PORT=" + port,
		"-e", "TERMINAL_SSH_USER=" + endpoint.Username,
		"-e", "TERMINAL_SSH_KEY=/run/aries/ssh/id_ed25519",
		"-e", "TERMINAL_CWD=" + sandboxRoot,
		"-e", "TERMINAL_TIMEOUT=60",
		"--entrypoint", "/opt/hermes/.venv/bin/python",
		image, "/driver.py",
	}
	command := exec.CommandContext(ctx, "docker", arguments...)
	output, runErr := command.CombinedOutput()
	t.Logf("hermes driver output:\n%s", output)
	if runErr != nil {
		t.Fatalf("hermes driver failed: %v", runErr)
	}
	text := string(output)
	if !strings.Contains(text, `"env_type": "ssh"`) {
		t.Fatalf("Hermes did not select its native SSH backend:\n%s", text)
	}
	if !strings.Contains(text, "aries-integration-ok") || !strings.Contains(text, `"exit_code": 0`) {
		t.Fatalf("Hermes tool call did not succeed through the bridge:\n%s", text)
	}

	commands := sandbox.snapshot()
	if len(commands) == 0 {
		t.Fatal("no command reached the sandbox")
	}
	sawAgentCommand := false
	for _, item := range commands {
		// The agent shell and the bootstrap probe shell are the only executables
		// the bridge may run, so any other path — a future tar, scp, or mkdir
		// file-sync leak included — fails here.
		switch item.Path {
		case remoteShellPath:
			sawAgentCommand = true
		case bootstrapShell:
			if len(item.Args) != 2 || item.Args[0] != "-c" || (item.Args[1] != connectionProbePayload && item.Args[1] != remoteHomePayload) {
				t.Fatalf("an unexpected bootstrap command reached the sandbox: %#v", item)
			}
		default:
			t.Fatalf("an unexpected command reached the sandbox: %#v", item)
		}
	}
	if !sawAgentCommand {
		t.Fatalf("no shell command reached the sandbox: %#v", commands)
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	// The canary must not exist anywhere under the sandbox root.
	walkErr := filepath.Walk(sandboxRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(content), canary) {
			return fmt.Errorf("file sync leaked %s into the sandbox", path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if _, err := os.Stat(filepath.Join(sandboxRoot, ".hermes")); !os.IsNotExist(err) {
		t.Fatalf(".hermes was created in the sandbox: %v", err)
	}

	// Evidence must show both the executed commands and the refusals.
	records := readToolCalls(t, filepath.Join(outputDir, "integration-task", "bridge", "tool-calls.jsonl"))
	var denied, completed int
	for _, record := range records {
		switch record["status"] {
		case "denied":
			denied++
		case "completed":
			completed++
		}
	}
	if completed == 0 {
		t.Fatalf("no completed tool call was recorded: %#v", records)
	}
	if denied == 0 {
		t.Fatalf("no file-sync refusal was recorded: %#v", records)
	}
}

// TestBridgeExecMutatesTheEvaluatorSandbox is the deterministic counterpart to
// the upstream-driven test above: it replays the exact four wire payloads
// Hermes can emit against a real Docker sandbox and a real SSH client, with no
// Hermes image and no model in the loop. It fixes what the bridge must do —
// mutate the sandbox the verifier later inspects, refuse the file sync, revoke
// on Stop, and retain correlated evidence — so a failure here is the bridge's,
// not upstream's.
func TestBridgeExecMutatesTheEvaluatorSandbox(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	outputDir := t.TempDir()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	if err := dockersandbox.PullImages(ctx, []string{bridgeFixtureImage}); err != nil {
		t.Fatalf("prepare pinned bridge fixture image: %v", err)
	}
	sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sandboxes.Start(ctx, core.SandboxRequest{
		RunID: "hermes-bridge-integration", TaskID: "same-state",
		Environment: core.Environment{Image: bridgeFixtureImage, Workdir: "/work", MemoryMB: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := live.(*dockersandbox.Sandbox)
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Errorf("sandbox cleanup: %v", err)
		}
	})

	manager := newTestManager(t, outputDir)
	endpoint, err := manager.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(outputDir, "same-state", "bridge")
	client, err := ssh.Dial("tcp", endpoint.Address, pinnedClientConfig(t, endpoint, filepath.Join(artifactDir, "known_hosts")))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// 1 & 2: the bootstrap probes. _establish_connection raises unless its echo
	// succeeds, so a regression here costs every later command.
	probe, _, err := runExec(t, client, connectionProbePayload, "")
	if err != nil || probe != "SSH connection established\n" {
		t.Fatalf("connection probe = %q, %v", probe, err)
	}
	home, _, err := runExec(t, client, remoteHomePayload, "")
	if err != nil || strings.TrimSpace(home) == "" || strings.Contains(home, "$HOME") {
		t.Fatalf("remote home probe = %q, %v", home, err)
	}

	// 3: an agent command, in the shlex-quoted form tools/environments/ssh.py
	// puts on the wire. It streams stdin, writes state the evaluator reads back,
	// and carries a non-zero exit through the channel.
	const script = "cat > /work/bridge-state; cat /work/bridge-state; printf tool-stderr >&2; exit 7"
	agentPayload := remoteShell + " -c " + shlexQuote(script)
	stdout, stderr, runErr := runExec(t, client, agentPayload, "streamed-input")
	var exitError *ssh.ExitError
	if !errors.As(runErr, &exitError) || exitError.ExitStatus() != 7 || stdout != "streamed-input" || stderr != "tool-stderr" {
		t.Fatalf("agent command = err %v stdout %q stderr %q", runErr, stdout, stderr)
	}
	direct, err := sandbox.Exec(ctx, core.Command{Path: "/bin/cat", Args: []string{"/work/bridge-state"}})
	if err != nil || direct.ExitCode != 0 || direct.Stdout != "streamed-input" {
		t.Fatalf("evaluator read = %#v, %v", direct, err)
	}

	// 4: the ~/.hermes file sync, which must be refused before it reaches the
	// container the verifier later inspects.
	const syncPayload = "mkdir -p /root/.hermes/skills"
	if _, _, err := runExec(t, client, syncPayload, ""); err == nil {
		t.Fatal("the file sync was accepted")
	}
	planted, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "test ! -e /root/.hermes"}})
	if err != nil || planted.ExitCode != 0 {
		t.Fatalf("refused file sync still touched the sandbox: %#v, %v", planted, err)
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("identity remains after Stop: %v", err)
	}
	if connection, err := net.DialTimeout("tcp", endpoint.Address, 200*time.Millisecond); err == nil {
		_ = connection.Close()
		t.Fatal("bridge listener still accepts connections after Stop")
	}
	// The sandbox must outlive revocation: the evaluator runs after the agent.
	after, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", "printf evaluator > /work/after-bridge"}})
	if err != nil || after.ExitCode != 0 {
		t.Fatalf("sandbox was not usable for evaluation after revocation: %#v, %v", after, err)
	}

	wantLogPaths := []string{filepath.Join(artifactDir, "tool-calls.jsonl"), filepath.Join(artifactDir, "ssh_raw.log")}
	if !slices.Equal(endpoint.LogPaths, wantLogPaths) {
		t.Fatalf("log paths = %q, want %q", endpoint.LogPaths, wantLogPaths)
	}
	for _, path := range endpoint.LogPaths {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("retained bridge log %q = %v, %v", path, info, err)
		}
	}
	records := readToolCalls(t, endpoint.LogPaths[0])
	execs := recordsOfType(records, "exec")
	if len(execs) != 4 {
		t.Fatalf("exec records = %#v", records)
	}
	wantExecs := []struct {
		class    string
		status   string
		exitCode float64
		command  string
	}{
		{kindBootstrap, "completed", 0, connectionProbePayload},
		{kindBootstrap, "completed", 0, remoteHomePayload},
		{kindAgent, "completed", 7, agentPayload},
		// A refused sync never runs, so it must not borrow a command's exit code.
		{kindSync, "denied", -1, ""},
	}
	for index, want := range wantExecs {
		record := execs[index]
		if record["operation_class"] != want.class || record["status"] != want.status || record["exit_code"] != want.exitCode {
			t.Fatalf("exec record %d = %#v, want %+v", index, record, want)
		}
		if want.command != "" && record["command"] != want.command {
			t.Fatalf("exec record %d command = %#v, want %q", index, record["command"], want.command)
		}
		if record["run_id"] != "hermes-bridge-integration" || record["task_id"] != "same-state" || record["container_id"] != sandbox.ContainerID() {
			t.Fatalf("exec record %d identity = %#v", index, record)
		}
	}
	if execs[2]["stdin"] != "streamed-input" || execs[2]["stdin_encoding"] != "utf-8" || execs[2]["workdir"] != "/work" || execs[2]["path"] != remoteShellPath {
		t.Fatalf("agent record = %#v", execs[2])
	}
	// Every request Hermes sent must appear, including the env request OpenSSH
	// prepends and ARIES declines.
	envs := recordsOfType(records, "env")
	if len(envs) != len(execs) {
		t.Fatalf("env records = %d, want one per call: %#v", len(envs), records)
	}
	// Command output belongs to the sandbox transcript, never to the audit.
	structured, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"stdout":`, `"stderr":`} {
		if bytes.Contains(structured, []byte(forbidden)) {
			t.Fatalf("tool log retained command output field %s: %s", forbidden, structured)
		}
	}
	rawContent, err := os.ReadFile(endpoint.LogPaths[1])
	if err != nil {
		t.Fatal(err)
	}
	raw := string(rawContent)
	for _, evidence := range []string{
		"wire_command=" + syncPayload, "status=denied",
		"stdin=streamed-input", "run_id=hermes-bridge-integration",
	} {
		if !strings.Contains(raw, evidence) {
			t.Fatalf("raw log lacks %q:\n%s", evidence, raw)
		}
	}
	for _, forbidden := range []string{"stdout=", "stderr="} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("raw log retained command output field %q:\n%s", forbidden, raw)
		}
	}
	if strings.Count(raw, "ARIES SSH CALL BEGIN") != len(records) {
		t.Fatalf("raw and structured audits are uncorrelated: %d raw of %d structured", strings.Count(raw, "ARIES SSH CALL BEGIN"), len(records))
	}
}

// pinnedClientConfig verifies the host key against the known_hosts file the
// bridge retains, which is the evidence of what Hermes pins on first use.
func pinnedClientConfig(t *testing.T, endpoint core.ToolEndpoint, knownHostsPath string) *ssh.ClientConfig {
	t.Helper()
	configuration := clientConfig(t, endpoint)
	callback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		t.Fatal(err)
	}
	configuration.HostKeyCallback = callback
	return configuration
}
