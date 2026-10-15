//go:build integration

package hermes

// The gRPC route end to end, short of a model: the harness stages the pinned
// Hermes image with the ARIES plugin and the seam, the managed tool bridge
// serves a real Docker sandbox, and Hermes's own tool entry points run through
// it.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/internal/testutil/dockerroute"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	tasksandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
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
from tools.file_tools import read_file_tool, write_file_tool
print("WRITE", write_file_tool(path="/work/e2e/note.txt", content="first line\nsecond line\n", task_id="tool-probe"))
print("READ", read_file_tool(path="/work/e2e/note.txt", offset=2, limit=1, task_id="tool-probe"))
print("CHECK", terminal_tool(command="cat /work/e2e/note.txt", task_id="tool-probe"))
`

// syncPrefixes are the commands Hermes's FileSyncManager sends to a remote
// backend (tools/environments/file_sync.py, ssh.py). The gRPC backend builds
// no FileSyncManager, so none of them may reach the bridge.
var syncPrefixes = []string{"mkdir -p ", "rm -f ", "tar ", "scp"}

// On the gRPC route file tools go through the typed procedures end to end:
// plugin, client, bridge and the Docker sandbox's file access.
func TestPluginRunsHermesToolsThroughTheGRPCBridge(t *testing.T) {
	requireDockerImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	versions, err := config.LoadVersions(filepath.Join("..", "..", "..", "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dockerdeployment.PullImages(ctx, "", []string{toolSandboxImage}); err != nil {
		t.Fatal(err)
	}
	const runID, taskID = "hermes-tools-integration", "tool-task"
	output := t.TempDir()
	provider, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	runEnvironment := dockerroute.Environment(t, runID)
	sandboxes, err := tasksandbox.New(tasksandbox.Options{Deployment: provider, NewEnvironment: runEnvironment.NewTaskEnvironment, OutputDir: output})
	if err != nil {
		_ = provider.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sandboxes.Close(); err != nil {
			t.Error(err)
		}
	})
	service := startBridgeService(ctx, t, versions.Bridge.Image, "hermes-grpc", runID, runEnvironment.NetworkName(), output)
	live, err := sandboxes.Start(ctx, core.SandboxRequest{RunID: runID, TaskID: taskID, Environment: core.Environment{Image: toolSandboxImage, Workdir: "/work", MemoryMB: 128}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if err := sandboxes.Stop(cleanup, live); err != nil {
			t.Error(err)
		}
	})
	session, err := service.NewSession(output)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := session.Start(ctx, live)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		_ = session.Stop(cleanup)
	})

	manager := integrationManager(t, output)
	if err := manager.Start(ctx, core.HarnessRequest{
		RunID: runID, TaskID: taskID, Endpoint: endpoint,
		Connectivity: live.(interface {
			Connectivity() core.HarnessConnectivity
		}).Connectivity(),
		Model: core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
	}); err != nil {
		t.Fatalf("start Hermes: %v", err)
	}
	// The launcher has already applied the seam as root; the probe runs as the
	// unprivileged user, with the container's own environment.
	result, err := manager.runtime.Options.Deployment.Exec(ctx, manager.active.ID, core.Command{
		Path: "python3", Args: []string{"-c", toolProbe}, Dir: "/opt/hermes", User: "10000:10000", OutputLimitBytes: maxDockerOutput,
	})
	text := result.Stdout + result.Stderr
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("tool probe: %v (exit %d)\n%s", err, result.ExitCode, text)
	}
	t.Logf("tool probe output:\n%s", text)
	if !strings.Contains(text, "FILE_OPS AriesFileOperations AriesEnvironment") {
		t.Fatalf("file tools did not reach the plugin through the seam:\n%s", text)
	}
	if !strings.Contains(text, "aries-tool-ok") || !strings.Contains(text, "/work") || !strings.Contains(text, `"exit_code": 0`) {
		t.Fatalf("terminal tool did not run in the sandbox workdir:\n%s", text)
	}
	write, read := strings.Index(text, "WRITE"), strings.Index(text, "READ")
	if write < 0 || read < write || strings.Contains(text[write:read], `"error"`) {
		t.Fatalf("write_file failed:\n%s", text)
	}
	if !strings.Contains(text, "2|second line") || strings.Contains(text, "1|first line") {
		t.Fatalf("read_file did not return the requested line:\n%s", text)
	}
	if !strings.Contains(text, `first line\nsecond line`) {
		t.Fatalf("the written bytes are not in the sandbox:\n%s", text)
	}

	// Revocation order as the Runner: harness first, then the bridge, which
	// finalizes and collects the audit.
	if err := manager.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range bytes.Split(bytes.TrimSpace(log), []byte("\n")) {
		var record struct {
			OperationClass string `json:"operation_class"`
			Path           string `json:"path"`
			Command        string `json:"command"`
			Status         string `json:"status"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode record: %v\n%s", err, line)
		}
		if record.Status == "rejected" {
			t.Fatalf("the bridge rejected a call: %s", line)
		}
		for _, prefix := range syncPrefixes {
			if strings.HasPrefix(record.Command, prefix) {
				t.Fatalf("a Hermes file-sync payload reached the bridge: %s", line)
			}
		}
		if strings.Contains(record.Command, ".hermes/") || strings.Contains(record.Path, ".hermes/") {
			t.Fatalf("a Hermes home path reached the bridge: %s", line)
		}
		switch {
		case record.OperationClass == "agent" && strings.Contains(record.Command, "echo aries-tool-ok"):
			seen["terminal"] = true
		case record.OperationClass == "agent" && strings.Contains(record.Command, "cat /work/e2e/note.txt"):
			seen["check"] = true
		case record.Path == "/work/e2e/note.txt":
			seen[record.OperationClass] = true
		}
	}
	for _, want := range []string{"terminal", "check", "WriteFile", "ReadLines"} {
		if !seen[want] {
			t.Fatalf("no %s record in tool-calls.jsonl:\n%s", want, log)
		}
	}
}

// TestPluginFileOperationsInThePinnedImage runs the plugin's file operations
// inside the pinned Hermes image, with the seam applied, against a fake client
// that serves the same contract from the container's own filesystem. Hermes
// is importable only there, so this is where the adapter's unit test lives.
// The binds name package paths: a snap Docker cannot see the host's /tmp.
func TestPluginFileOperationsInThePinnedImage(t *testing.T) {
	requireDockerImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	script := "mkdir -p /fake && cp /t/fake_aries_grpc.py /fake/aries-grpc && chmod +x /fake/aries-grpc" +
		" && python3 /seam.py && cd /opt/hermes && python3 /t/plugin_check.py"
	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: integrationImage, Entrypoint: []string{"sh"}, Cmd: []string{"-c", script},
			Env: []string{"FAKE_CLIENT_LOG=/tmp/client.log", "HERMES_WRITE_SAFE_ROOT="},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none", Binds: []string{
			filepath.Join(here, "plugin") + ":/plugin:ro",
			filepath.Join(here, "seam.py") + ":/seam.py:ro",
			filepath.Join(here, "testdata") + ":/t:ro",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if _, err := api.ContainerRemove(cleanup, created.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			t.Errorf("remove plugin check: %v", err)
		}
		if _, err := api.ContainerInspect(cleanup, created.ID, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Errorf("plugin check absence not confirmed: %v", err)
		}
	}()
	if _, err := api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	wait := api.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{})
	var status int64
	select {
	case result := <-wait.Result:
		status = result.StatusCode
	case err := <-wait.Error:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	logs, err := api.ContainerLogs(ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	_, copyErr := stdcopy.StdCopy(&output, &output, logs)
	_ = logs.Close()
	if copyErr != nil {
		t.Fatal(copyErr)
	}
	if status != 0 || !strings.Contains(output.String(), "PLUGIN_CHECK_OK") {
		t.Fatalf("plugin check exit %d:\n%s", status, output.String())
	}
}
