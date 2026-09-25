//go:build integration

package hermes

// Both Hermes routes end to end, short of a model: the harness stages the
// pinned Hermes image, the real bridge serves a real Docker sandbox, and
// Hermes's own tool entry points run one command through it. On the gRPC route
// the harness also stages the ARIES plugin and the seam.

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/hermesgrpc"
	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
)

// toolSandboxImage matches the bridge integration test: the bridge resolves
// `bash`, so the sandbox needs one.
const toolSandboxImage = "docker.io/library/debian:12-slim@sha256:abd67ffcfa541b485a3dff59865ab629aa048a6c613e639d36e7456b0b229241"

// toolProbe runs inside the harness container as the `hermes` user, the
// identity Hermes itself runs as.
const toolProbe = `
from hermes_cli.plugins import discover_plugins
discover_plugins()
from tools.file_tools import _get_file_ops
ops = _get_file_ops("tool-probe")
print("FILE_OPS", type(ops).__name__, type(ops.env).__name__)
from tools.terminal_tool import terminal_tool
print("TOOL", terminal_tool(command="echo aries-tool-ok && pwd", task_id="tool-probe"))
`

// runToolProbe starts one Hermes harness against the named bridge and returns
// what the probe printed.
func runToolProbe(t *testing.T, protocol string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	versions, err := config.LoadVersions(filepath.Join("..", "..", "..", "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	image := versions.Hermes.Image
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := dockersandbox.PullImages(ctx, []string{toolSandboxImage, image}); err != nil {
		t.Fatalf("prepare images: %v", err)
	}

	outputDir := t.TempDir()
	logger := logrus.New()
	logger.SetOutput(io.Discard)

	sandboxes, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	live, err := sandboxes.Start(ctx, core.SandboxRequest{
		RunID: "tool-integration", TaskID: "tool-task",
		Environment: core.Environment{Image: toolSandboxImage, Workdir: "/work", MemoryMB: 128},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = sandboxes.Stop(cleanup, live)
	})

	var bridge interface {
		Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error)
		Stop(context.Context) error
	}
	if protocol == protocolGRPC {
		client := filepath.Join(t.TempDir(), "aries-grpc")
		build := exec.Command("go", "build", "-o", client, "github.com/hyscale-lab/aries/cmd/aries-grpc")
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build aries-grpc: %v\n%s", err, output)
		}
		bridge, err = hermesgrpc.New(hermesgrpc.Options{OutputDir: outputDir, Logger: logger, ClientPath: client})
	} else {
		bridge, err = hermesssh.New(hermesssh.Options{OutputDir: outputDir, Logger: logger})
	}
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := bridge.Start(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Stop(context.Background()) })

	manager, err := New(Options{
		Image: image, OutputDir: outputDir, Logger: logger,
		StartTimeout: 2 * time.Minute, AgentTimeout: time.Minute, CleanupTimeout: time.Minute,
		APIKeyLookup: func(string) ([]byte, bool) { return []byte("sk-integration-not-a-real-key"), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if err := manager.Start(ctx, core.HarnessRequest{
		RunID: "tool-integration", TaskID: "tool-task", Endpoint: endpoint,
		Model: core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-pro", APIKeyEnv: "DEEPSEEK_API_KEY"},
	}); err != nil {
		t.Fatalf("start Hermes: %v", err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background()) })
	container := manager.active.containerName

	if protocol == protocolGRPC {
		// What the agent wrapper does, as root, before Hermes starts.
		if output, err := exec.CommandContext(ctx, "docker", "exec", container, "python3", seamContainerFS).CombinedOutput(); err != nil {
			t.Fatalf("seam: %v\n%s", err, output)
		}
	}
	output, err := exec.CommandContext(ctx, "docker", "exec", "--user", "10000:10000", "--workdir", "/opt/hermes",
		container, "python3", "-c", toolProbe).CombinedOutput()
	text := string(output)
	if err != nil {
		t.Fatalf("tool probe: %v\n%s", err, text)
	}
	// The first command must succeed and run in the sandbox's workdir, which
	// exists only in the sandbox. Hermes runs a session's first command in
	// TERMINAL_CWD, so a path the sandbox lacks fails every command with 126.
	if !strings.Contains(text, "aries-tool-ok") || !strings.Contains(text, "/work") || !strings.Contains(text, `"exit_code": 0`) {
		t.Fatalf("terminal tool did not run in the sandbox workdir:\n%s", text)
	}
	return text
}

func TestHermesSSHRouteRunsTheFirstCommandInTheSandboxWorkdir(t *testing.T) {
	runToolProbe(t, protocolSSH)
}

func TestPluginRunsHermesToolsThroughTheGRPCBridge(t *testing.T) {
	if text := runToolProbe(t, protocolGRPC); !strings.Contains(text, "FILE_OPS AriesFileOperations AriesEnvironment") {
		t.Fatalf("file tools did not reach the plugin through the seam:\n%s", text)
	}
}
