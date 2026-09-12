package toolathlon

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

//go:embed portfwd.py
var forwarderScript []byte

// Package-level vars, not consts, so tests can shrink them to avoid waiting
// out the real bounds.
var (
	preprocessTimeout = 20 * time.Minute
	// gatewayReadyAttempts covers the gateway's own startup, during which
	// it launches every MCP server the task needs (npx for the Node ones).
	gatewayReadyAttempts   = 120
	gatewayReadyDelay      = time.Second
	forwarderReadyAttempts = 40
	forwarderReadyDelay    = 250 * time.Millisecond
)

// gatewayStartScript backgrounds Toolathlon's MCP gateway from the project
// directory. Its arguments arrive as positional parameters, so no value is
// spliced into shell text: $1 bundle file, $2 port, $3 log file. The gateway
// binds 0.0.0.0 because its client is the harness in another container,
// reaching it over the per-task bridge network by the sandbox's alias; that
// network is internal to the task, so nothing else can. ARIES
// overrides the task image's entrypoint with `/bin/sleep infinity`, so this
// is the only place the gateway is launched, and the backgrounded process
// is reparented to PID 1 when the shell exits (see the SearXNG note in
// deepresearchbench/sandbox.go).
const gatewayStartScript = `cd ` + workspaceRoot + ` && nohup uv run python -m scripts.decoupled.container_tool_gateway ` +
	`--bundle_file "$1" --host 0.0.0.0 --port "$2" --debug >"$3" 2>&1 &`

// forwarderStartScript backgrounds the loopback forwarder: $1 script, $2
// target host, $3 port list, $4 ready file, $5 log file.
const forwarderStartScript = `nohup python3 "$1" --target "$2" --ports "$3" --ready "$4" >"$5" 2>&1 &`

// Toolathlon's scripts run under `uv run` from the image's own PATH, which
// the sandbox does not resolve for a bare command name. The shell resolves
// it; the arguments are still passed positionally, never spliced into text.
const uvScript = `exec uv "$@"`

// uvCommand is `uv <args>` in the project directory.
func uvCommand(args ...string) core.Command {
	return core.Command{
		Path: "/bin/sh",
		Args: append([]string{"-c", uvScript, "aries-toolathlon-uv"}, args...),
		Dir:  workspaceRoot,
	}
}

// PrepareSandbox reproduces the container-side half of Toolathlon's
// decoupled runner, then hides the grader before any bridge exists:
//
//  1. copy the pinned project tree and the task directory into the image;
//  2. start the loopback forwarder if the task uses a self-hosted application;
//  3. run Toolathlon's preprocess, which seeds the workspace and the
//     applications and writes the task bundle;
//  4. keep a trusted copy of the bundle on the host and check its layout;
//  5. stash the grader and ground truth on the host, and prove them absent;
//  6. start the MCP gateway and wait for its health endpoint.
func (b *Benchmark) PrepareSandbox(ctx context.Context, task core.Task, sandbox runner.Sandbox) error {
	if sandbox == nil {
		return errors.New("toolathlon preparation requires a live sandbox")
	}
	b.mu.RLock()
	details, loaded := b.details[task.ID]
	b.mu.RUnlock()
	if !loaded {
		return fmt.Errorf("toolathlon task %q was not loaded by Tasks", task.ID)
	}
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return fmt.Errorf("reverify toolathlon checkout before preparation: %w", err)
	}

	hostDir := filepath.Join(b.outputDir, task.ID, "toolathlon")
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		return fmt.Errorf("create toolathlon artifact directory: %w", err)
	}

	// A reused or dirty image must not pre-seed the two files Evaluate
	// trusts, nor carry a previous task's private state.
	if err := removePaths(ctx, sandbox, []string{trajectoryPath, evalResultPath, privateRoot, taskDirectoryPath(details.name)}); err != nil {
		return fmt.Errorf("clean sandbox before harness: %w", err)
	}
	if err := execOK(ctx, sandbox, core.Command{Path: "/bin/mkdir", Args: []string{"-p", "-m", "700", "--", privateRoot}}, "create private directory"); err != nil {
		return err
	}
	if err := execOK(ctx, sandbox, core.Command{Path: "/bin/mkdir", Args: []string{"-p", "--", taskRootPath}}, "create task root"); err != nil {
		return err
	}

	if err := b.installProject(ctx, sandbox, details.name, hostDir); err != nil {
		return err
	}
	if details.needsApplications {
		if err := b.startForwarder(ctx, sandbox); err != nil {
			return err
		}
	}
	if err := b.runPreprocess(ctx, sandbox, details.name, hostDir); err != nil {
		return err
	}
	bundleHostPath := filepath.Join(hostDir, "task_bundle.json")
	if err := sandbox.Download(ctx, bundleContainerPath, bundleHostPath); err != nil {
		return fmt.Errorf("download task bundle: %w", err)
	}
	if err := validateBundleFile(bundleHostPath, details); err != nil {
		return fmt.Errorf("validate task bundle: %w", err)
	}
	stashed, err := stashArtifacts(ctx, sandbox, details.name, filepath.Join(hostDir, "artifact-stash.tar"))
	if err != nil {
		return fmt.Errorf("stash grader artifacts before harness: %w", err)
	}
	b.mu.Lock()
	details.stashed = stashed
	b.details[task.ID] = details
	b.mu.Unlock()

	if err := b.startGateway(ctx, sandbox); err != nil {
		return err
	}
	// The gateway read the bundle at startup; the container copy is not
	// needed again until evaluation re-injects the trusted host copy.
	return removePaths(ctx, sandbox, []string{bundleContainerPath})
}

// installProject uploads one archive of the pinned project tree and the task
// directory and extracts it over the image's own copy.
func (b *Benchmark) installProject(ctx context.Context, sandbox runner.Sandbox, taskName, hostDir string) error {
	archive := filepath.Join(hostDir, "project.tar")
	if err := writeProjectArchive(b.root, taskName, archive); err != nil {
		return err
	}
	defer os.Remove(archive)
	if err := sandbox.Upload(ctx, archive, archiveContainerPath); err != nil {
		return fmt.Errorf("upload project archive: %w", err)
	}
	if err := execOK(ctx, sandbox, core.Command{Path: tarPath, Args: []string{"-C", workspaceRoot, "-xf", archiveContainerPath}}, "extract project archive"); err != nil {
		return err
	}
	return removePaths(ctx, sandbox, []string{archiveContainerPath})
}

func (b *Benchmark) startForwarder(ctx context.Context, sandbox runner.Sandbox) error {
	scriptHost, err := os.CreateTemp(b.outputDir, ".portfwd-*.py")
	if err != nil {
		return fmt.Errorf("stage forwarder script: %w", err)
	}
	defer os.Remove(scriptHost.Name())
	if _, err := scriptHost.Write(forwarderScript); err != nil {
		scriptHost.Close()
		return fmt.Errorf("stage forwarder script: %w", err)
	}
	if err := scriptHost.Close(); err != nil {
		return fmt.Errorf("stage forwarder script: %w", err)
	}
	if err := sandbox.Upload(ctx, scriptHost.Name(), forwarderScriptPath); err != nil {
		return fmt.Errorf("upload forwarder script: %w", err)
	}
	target := b.appHost
	if target == "" {
		target = "auto"
	}
	ports := make([]string, 0, len(applicationPorts))
	for _, port := range applicationPorts {
		ports = append(ports, strconv.Itoa(port))
	}
	started, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", forwarderStartScript, "aries-toolathlon-portfwd", forwarderScriptPath, target, strings.Join(ports, ","), forwarderReadyPath, forwarderLogPath},
	})
	if err != nil {
		return fmt.Errorf("start loopback forwarder: %w", err)
	}
	if started.ExitCode != 0 {
		return fmt.Errorf("start loopback forwarder: exit code %d", started.ExitCode)
	}
	for attempt := 0; attempt < forwarderReadyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("await loopback forwarder: %w", ctx.Err())
			case <-time.After(forwarderReadyDelay):
			}
		}
		probed, err := sandbox.Exec(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", `[ -s "$1" ]`, "aries-toolathlon-portfwd-ready", forwarderReadyPath}})
		if err == nil && probed.ExitCode == 0 {
			return nil
		}
	}
	return errors.New("loopback forwarder did not become ready before the bound")
}

// runPreprocess runs Toolathlon's container_preprocess with the same
// arguments its decoupled runner uses, minus the ones that only matter to
// its host-side agent loop, and keeps its full output on the host.
func (b *Benchmark) runPreprocess(ctx context.Context, sandbox runner.Sandbox, taskName, hostDir string) error {
	command := uvCommand(
		"run", "python", "-m", "scripts.decoupled.container_preprocess",
		"--eval_config", evalConfigPath,
		"--task_dir", taskPool+"/"+taskName,
		"--max_steps_under_single_turn_mode", strconv.Itoa(b.maxSteps),
		"--model_short_name", b.modelName,
		"--provider", "unified",
		"--bundle_file", bundleContainerPath,
		"--host_output_folder", taskRootPath,
		"--debug",
	)
	command.Env = map[string]string{"TOOLATHLON_OPENAI_BASE_URL": modelPlaceholderURL}
	command.Timeout = preprocessTimeout
	result, execErr := sandbox.Exec(ctx, command)
	logPath := filepath.Join(hostDir, "preprocess.log")
	if err := os.WriteFile(logPath, []byte(result.Stdout+result.Stderr), 0o600); err != nil {
		return fmt.Errorf("write preprocess log: %w", err)
	}
	if execErr != nil {
		return fmt.Errorf("run toolathlon preprocess: %w", execErr)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("run toolathlon preprocess: exit code %d (see %s)", result.ExitCode, logPath)
	}
	return nil
}

func (b *Benchmark) startGateway(ctx context.Context, sandbox runner.Sandbox) error {
	port := strconv.Itoa(b.gatewayPort)
	started, err := sandbox.Exec(ctx, core.Command{
		Path: "/bin/sh",
		Args: []string{"-c", gatewayStartScript, "aries-toolathlon-gateway", bundleContainerPath, port, gatewayLogPath},
	})
	if err != nil {
		return fmt.Errorf("start MCP gateway: %w", err)
	}
	if started.ExitCode != 0 {
		return fmt.Errorf("start MCP gateway: exit code %d", started.ExitCode)
	}
	healthURL := "http://127.0.0.1:" + port + "/health"
	for attempt := 0; attempt < gatewayReadyAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("await MCP gateway readiness: %w", ctx.Err())
			case <-time.After(gatewayReadyDelay):
			}
		}
		probed, err := sandbox.Exec(ctx, core.Command{
			Path: "/bin/sh", Args: []string{"-c", `curl -sf -o /dev/null "$1"`, "aries-toolathlon-gateway-health", healthURL},
		})
		if err == nil && probed.ExitCode == 0 {
			return nil
		}
	}
	return errors.New("MCP gateway did not become ready before the bound")
}

// taskBundle is the part of Toolathlon's phase bundle the adapter checks.
type taskBundle struct {
	SchemaVersion      int             `json:"schema_version"`
	TaskDir            string          `json:"task_dir"`
	NeededMCPServers   json.RawMessage `json:"needed_mcp_servers"`
	ContainerPaths     bundlePaths     `json:"container_paths"`
	ResolvedTaskConfig json.RawMessage `json:"resolved_task_config"`
}

type bundlePaths struct {
	TaskRoot       string `json:"task_root"`
	AgentWorkspace string `json:"agent_workspace"`
	LogFile        string `json:"log_file"`
}

// validateBundleFile is the adapter's equivalent of the checks the decoupled
// runner makes on the bundle it preserves outside the container: the layout
// Evaluate relies on must be the one preprocess actually produced.
func validateBundleFile(hostPath string, details taskDetails) error {
	content, err := os.ReadFile(hostPath)
	if err != nil {
		return err
	}
	return validateBundle(content, details)
}

func validateBundle(content []byte, details taskDetails) error {
	var bundle taskBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if bundle.SchemaVersion != 2 {
		return fmt.Errorf("unsupported schema version %d", bundle.SchemaVersion)
	}
	if want := taskPool + "/" + details.name; bundle.TaskDir != want {
		return fmt.Errorf("task_dir %q; want %q", bundle.TaskDir, want)
	}
	if bundle.ContainerPaths != (bundlePaths{TaskRoot: taskRootPath, AgentWorkspace: agentWorkspacePath, LogFile: trajectoryPath}) {
		return fmt.Errorf("container paths %+v do not match the fixed layout", bundle.ContainerPaths)
	}
	var resolved map[string]json.RawMessage
	if len(bundle.ResolvedTaskConfig) == 0 || json.Unmarshal(bundle.ResolvedTaskConfig, &resolved) != nil || resolved == nil {
		return errors.New("resolved_task_config is missing")
	}
	servers, err := serverNames(bundle.NeededMCPServers)
	if err != nil {
		return fmt.Errorf("needed_mcp_servers: %w", err)
	}
	want := slices.Clone(details.servers)
	slices.Sort(servers)
	slices.Sort(want)
	if !slices.Equal(servers, want) {
		return fmt.Errorf("needed_mcp_servers %v; task config says %v", servers, want)
	}
	return nil
}

func execOK(ctx context.Context, sandbox runner.Sandbox, command core.Command, what string) error {
	result, err := sandbox.Exec(ctx, command)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s: exit code %d: %s", what, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}
