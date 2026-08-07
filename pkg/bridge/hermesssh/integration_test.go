//go:build integration

package hermesssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// hermesIntegrationImage is the pinned upstream image. It is used unmodified:
// the point of this test is that no patch layer is required.
const hermesIntegrationImage = "docker.io/nousresearch/hermes-agent:v2026.5.29.2"

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

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", hermesIntegrationImage).CombinedOutput(); err != nil {
		t.Skipf("pinned Hermes image is not present locally (%s): %s", hermesIntegrationImage, output)
	}
}

// TestUpstreamHermesDrivesTheBridgeWithoutPatches is the load-bearing check for
// this integration: an unmodified upstream Hermes, configured only through
// environment variables, runs a tool call through the ARIES bridge into the
// sandbox, while its ~/.hermes file sync is refused and never reaches the
// sandbox.
func TestUpstreamHermesDrivesTheBridgeWithoutPatches(t *testing.T) {
	requireDocker(t)
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
		hermesIntegrationImage, "/driver.py",
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
		// The agent shell is the only executable the bridge may run, so any
		// other path — a future tar, scp, or mkdir file-sync leak included —
		// fails here.
		if item.Path != remoteShellPath {
			t.Fatalf("an unexpected command reached the sandbox: %#v", item)
		}
		sawAgentCommand = true
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
