//go:build integration

package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
)

// Exercise terminal workdir and spans through the native Gateway and real bridge.
func TestTerminalCallWithoutWorkdirRunsInTheEndpointWorkdir(t *testing.T) {
	const base = "docker.io/nousresearch/hermes-agent:v2026.8.31"
	t.Run("base", func(t *testing.T) { runHermesBridgeScenario(t, "hermes-ssh", false, 1, false) })
	t.Run("derived", func(t *testing.T) { runHermesBridgeScenario(t, "hermes-ssh", false, 1, true, preparedImage(t, base)) })
}

// preparedImage builds, or reuses from the local cache, the image ARIES runs
// for base, exactly as preparation does before a run.
func preparedImage(t *testing.T, base string) string {
	t.Helper()
	// The pins are the ones the version catalog ships.
	versions, err := config.LoadVersions(filepath.Join("..", "..", "..", "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin := versions.Hermes.OTelPlugin
	plugin := OTelPlugin{RepositoryURL: pin.RepositoryURL, Version: pin.Version, Revision: pin.Revision}
	image, err := LocalImage(base, plugin, versions.Hermes.GRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	args, err := ImageBuildArgs(base, plugin, versions.Hermes.GRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := dockerdeployment.BuildImage(ctx, "", image, ImageDockerfile, ImageContext(), args); err != nil {
		t.Fatal(err)
	}
	return image
}

// The ARIES Hermes image carries the pinned grpcio in the Hermes venv, the
// ARIES Hermes plugin in the bundled plugin directory, readable but not
// writable by the hermes user, and the seam in Hermes's own files.
func TestARIESHermesImageCarriesThePluginGRPCIOAndTheSeam(t *testing.T) {
	requireDockerImage(t)
	image := preparedImage(t, integrationImage)
	versions, err := config.LoadVersions(filepath.Join("..", "..", "..", "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	plugin := "/opt/hermes/plugins/aries"
	script := `set -e
/opt/hermes/.venv/bin/python -c 'import grpc; print("GRPCIO", grpc.__version__)'
for path in ` + plugin + ` ` + plugin + `/plugin.yaml ` + plugin + `/__init__.py; do
  test -r "$path" || { echo "NOT_READABLE $path"; exit 1; }
  if test -w "$path"; then echo "WRITABLE $path"; exit 1; fi
done
cmp ` + plugin + `/plugin.yaml /context/plugin.yaml
cmp ` + plugin + `/__init__.py /context/__init__.py
grep -q "def get_file_operations(self):" /opt/hermes/tools/environments/base.py
grep -q "terminal_env.get_file_operations() or ShellFileOperations(terminal_env)" /opt/hermes/tools/file_tools.py
echo IMAGE_CHECK_OK
`
	here, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("%d:%d", runtimeUID, runtimeGID)
	status, output := runScriptInImage(ctx, t, image, user, script, nil, []string{filepath.Join(here, "plugin") + ":/context:ro"})
	if status != 0 || !strings.Contains(output, "IMAGE_CHECK_OK") || !strings.Contains(output, "GRPCIO "+versions.Hermes.GRPCIO+"\n") {
		t.Fatalf("image check exit %d:\n%s", status, output)
	}
}

// The SSH route runs the same image, where Hermes auto-loads the ARIES Hermes
// plugin as a bundled backend. Loading it must not fail or change the
// backend: the terminal stays on Hermes's SSH environment and file tools stay
// on ShellFileOperations. The SSH class is replaced by an offline subclass,
// because the container has no network to connect to.
func TestARIESHermesImageKeepsTheSSHRouteOnTheShell(t *testing.T) {
	requireDockerImage(t)
	image := preparedImage(t, integrationImage)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const probe = `mkdir -p "$HERMES_HOME" && printf '%s' "$ARIES_CONFIG" > "$HERMES_HOME/config.yaml" && cd /opt/hermes && python3 -c '
from hermes_cli.plugins import discover_plugins, get_plugin_manager
discover_plugins(force=True)
for plugin in get_plugin_manager().list_plugins():
    if plugin.get("name") == "aries":
        print("ARIES_PLUGIN", plugin.get("enabled"), plugin.get("error"))
import tools.terminal_tool as terminal_tool
from tools.environments.ssh import SSHEnvironment
class OfflineSSH(SSHEnvironment):
    def __init__(self, **kwargs):
        self.cwd = kwargs["cwd"]
terminal_tool._SSHEnvironment = OfflineSSH
from tools import file_tools
print("ENV_TYPE", terminal_tool._get_env_config()["env_type"])
ops = file_tools._get_file_ops("ssh-route-probe")
print("FILE_OPS", type(ops).__name__, type(ops.env).__name__, ops.env.get_file_operations())
'`
	settings := baseSettings()
	config := mustRender(t, validModel(), settings)
	terminal, err := renderTerminal("ssh", "/work", defaultTerminalTimeout)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := containerEnvironment(validEndpoint(), "/work", defaultTerminalTimeout, false, "", "ssh-route-run", "ssh-route-task")
	if err != nil {
		t.Fatal(err)
	}
	// HERMES_HOME moves to a directory the unprivileged user can create.
	environment = slices.DeleteFunc(environment, func(entry string) bool { return strings.HasPrefix(entry, "HERMES_HOME=") })
	environment = append(environment, "HERMES_HOME=/tmp/hermes", "ARIES_CONFIG="+config+string(terminal))
	user := fmt.Sprintf("%d:%d", runtimeUID, runtimeGID)
	status, output := runScriptInImage(ctx, t, image, user, probe, environment, nil)
	for _, want := range []string{"ARIES_PLUGIN True None\n", "ENV_TYPE ssh\n", "FILE_OPS ShellFileOperations OfflineSSH None\n"} {
		if status != 0 || !strings.Contains(output, want) {
			t.Fatalf("exit %d, want %q:\n%s", status, want, output)
		}
	}
}

// assertToolSpan requires a span named name for the tool call id whose
// wall-clock window is well formed.
func assertToolSpan(t *testing.T, path, name, callID string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		var span struct {
			Name       string         `json:"name"`
			Start      int64          `json:"start_time_unix_nano"`
			End        int64          `json:"end_time_unix_nano"`
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal([]byte(line), &span); err != nil {
			t.Fatalf("decode span: %v\n%s", err, line)
		}
		if span.Name != name || span.Attributes["gen_ai.tool.call.id"] != callID {
			continue
		}
		if span.Start <= 0 || span.End < span.Start {
			t.Fatalf("span window is malformed: %s", line)
		}
		return
	}
	t.Fatalf("no %s span for %s in:\n%s", name, callID, content)
}
