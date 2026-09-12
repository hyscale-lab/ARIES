// Package toolathlon adapts the Toolathlon benchmark (hkust-nlp/Toolathlon)
// to the ARIES Benchmark role.
//
// Toolathlon's own runner has a "decoupled" mode that already separates the
// three concerns ARIES separates: task preparation and the MCP tool gateway
// run inside the task container, the agent loop runs outside it, and the
// grader runs inside it again after the agent is gone. This package drives
// exactly those in-container pieces through the Sandbox capability, and lets
// the ARIES harness be the agent loop. The gateway is an MCP-over-SSE server
// on a fixed port; the harness reaches it through the sandbox's fixed
// `task-sandbox` network alias, the same way Deep Research Bench's SearXNG is
// reached.
//
// Two upstream assumptions do not hold in an ARIES sandbox and are handled
// here rather than by weakening the sandbox:
//
//   - Toolathlon starts its task container with `--network host`, so its MCP
//     servers and preprocess scripts reach the self-hosted applications
//     (Canvas, poste.io, WooCommerce) at `localhost`. An ARIES task container
//     is on a private per-task network, so PrepareSandbox starts a loopback
//     port forwarder inside the container that carries those fixed ports to
//     the Docker host.
//   - Toolathlon hides the grader and ground truth by copying them to the
//     host with `docker cp`. Here the same entries are archived, downloaded
//     into the private run directory, and removed before the bridge exists,
//     then restored only after harness stop and bridge revocation are
//     confirmed — the Terminal-Bench 2 verifier pattern.
package toolathlon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	// DefaultRoot is the conventional local checkout path for Toolathlon.
	DefaultRoot = ".cache/toolathlon"

	// DefaultGatewayPort is Toolathlon's own default for the MCP gateway.
	// The harness profile must name the same port in its MCP server URL.
	DefaultGatewayPort = 10086

	// DefaultMaxSteps mirrors `max_steps_under_single_turn_mode` in
	// Toolathlon's formal run config; it only bounds Toolathlon's own
	// bookkeeping here, since the ARIES harness owns the agent loop.
	DefaultMaxSteps = 200

	// taskPool is the only task pool in the pinned checkout.
	taskPool = "finalpool"

	// workspaceRoot is the task image's project directory and the working
	// directory of every Toolathlon script.
	workspaceRoot = "/workspace"
	// taskRootPath is where Toolathlon's `direct_to_dumps` run config places
	// the task root: the agent workspace, trajectory, and grader output all
	// live beneath it. The harness's terminal starts in the workspace.
	taskRootPath       = workspaceRoot + "/dumps"
	agentWorkspacePath = taskRootPath + "/workspace"
	trajectoryPath     = taskRootPath + "/traj_log.json"
	evalResultPath     = taskRootPath + "/eval_res.json"
	// privateRoot holds the adapter's own files inside the sandbox: the task
	// bundle, the project archive, and service logs. Nothing here is a
	// secret from the agent — the agent runs as the sandbox's exec user — but
	// keeping it out of /workspace keeps it out of the task's own listings.
	privateRoot          = "/run/aries-toolathlon"
	bundleContainerPath  = privateRoot + "/task_bundle.json"
	archiveContainerPath = privateRoot + "/project.tar"
	stashContainerPath   = privateRoot + "/artifact-stash.tar"
	gatewayLogPath       = privateRoot + "/gateway.log"
	forwarderLogPath     = privateRoot + "/portfwd.log"
	forwarderReadyPath   = privateRoot + "/portfwd.ready"
	forwarderScriptPath  = privateRoot + "/portfwd.py"

	// evalConfigPath is Toolathlon's formal run configuration, relative to
	// workspaceRoot. It carries the MCP server catalogue and the
	// `direct_to_dumps` layout the constants above depend on.
	evalConfigPath = "scripts/formal_run_v0.json"

	// modelPlaceholderURL satisfies Toolathlon's preprocess step, which
	// constructs its "unified" model provider and refuses to start without a
	// base URL, although preprocess never calls a model. The .invalid TLD is
	// reserved (RFC 2606) and can never resolve.
	modelPlaceholderURL = "http://model-not-used-by-aries.invalid/v1"
)

// Options selects tasks from one pinned Toolathlon checkout.
type Options struct {
	Root             string
	TaskIDs          []string
	ExecutionTaskIDs []string
	OutputDir        string
	Revision         string
	// Environment is the profile's sandbox description. The task image must
	// be Toolathlon's own task image; the workdir and network policy are
	// fixed by the adapter and any profile values for them are rejected.
	Environment core.Environment
	// GatewayPort is the in-sandbox port of the MCP gateway. Zero selects
	// DefaultGatewayPort.
	GatewayPort int
	// AppHost is where the self-hosted applications listen, as seen from the
	// Docker host. Empty means the sandbox's own default gateway, which is
	// the Docker host for a bridge network.
	AppHost string
	// MaxSteps is passed to Toolathlon's preprocess as
	// max_steps_under_single_turn_mode. Zero selects DefaultMaxSteps.
	MaxSteps int
	// ModelName is recorded in Toolathlon's task bundle as the agent model's
	// short name. It is bookkeeping only; the harness owns the model.
	ModelName string
}

// Benchmark discovers selected Toolathlon tasks and retains their private
// grader archives until evaluation.
type Benchmark struct {
	root             string
	taskIDs          []string
	executionTaskIDs []string
	outputDir        string
	revision         string
	environment      core.Environment
	gatewayPort      int
	appHost          string
	maxSteps         int
	modelName        string

	mu      sync.RWMutex
	details map[string]taskDetails
}

type taskDetails struct {
	// name is the task directory name beneath tasks/finalpool.
	name    string
	servers []string
	// needsApplications is true when any MCP server is backed by one of the
	// self-hosted applications, so the loopback forwarder must run.
	needsApplications bool
	// stashed names the task-directory entries PrepareSandbox moved to the
	// host, for Evaluate to restore.
	stashed []string
}

// taskConfigFile is the subset of tasks/<pool>/<task>/task_config.json the
// adapter reads. needed_mcp_servers is a list in every pinned task, but the
// upstream loader also accepts an object keyed by server name.
type taskConfigFile struct {
	NeededMCPServers json.RawMessage `json:"needed_mcp_servers"`
	NeededLocalTools json.RawMessage `json:"needed_local_tools"`
	MaxTurns         int             `json:"max_turns"`
}

var _ runner.Benchmark = (*Benchmark)(nil)

// serverKind classifies each MCP server named in the pinned checkout's
// configs/mcp_servers by what it needs at run time.
type serverKind int

const (
	// serverLocal runs entirely inside the sandbox.
	serverLocal serverKind = iota
	// serverApplication talks to an application Toolathlon deploys itself
	// (deployment/canvas, deployment/poste, deployment/woocommerce), at a
	// fixed localhost port.
	serverApplication
	// serverPublic reaches the public internet without an account.
	serverPublic
	// serverUnsupported needs a credentialed third-party account, or (k8s)
	// a Docker socket and host networking the sandbox does not grant.
	serverUnsupported
)

var serverKinds = map[string]serverKind{
	"excel": serverLocal, "filesystem": serverLocal, "git": serverLocal, "memory": serverLocal,
	"pdf-tools": serverLocal, "pptx": serverLocal, "terminal": serverLocal, "time": serverLocal,
	"word": serverLocal,

	"canvas": serverApplication, "emails": serverApplication, "woocommerce": serverApplication,

	"12306": serverPublic, "arxiv-latex-mcp": serverPublic, "arxiv_local": serverPublic,
	"howtocook": serverPublic, "npx-fetch": serverPublic, "playwright_with_chunk": serverPublic,
	"scholarly_search": serverPublic, "yahoo-finance": serverPublic, "youtube_transcript": serverPublic,

	"github": serverUnsupported, "google-cloud": serverUnsupported, "google_calendar": serverUnsupported,
	"google_forms": serverUnsupported, "google_map": serverUnsupported, "google_sheet": serverUnsupported,
	"huggingface": serverUnsupported, "k8s": serverUnsupported, "notion": serverUnsupported,
	"notion_official": serverUnsupported, "snowflake": serverUnsupported, "wandb": serverUnsupported,
	"youtube": serverUnsupported,
}

// applicationPorts are the fixed localhost ports Toolathlon's task-side
// code uses for the self-hosted applications (configs/ports_config.yaml):
// Canvas HTTP and HTTPS, poste.io SMTP, IMAP, submission, and web UI, and
// WooCommerce. The forwarder carries exactly these to the Docker host.
var applicationPorts = []int{1143, 1587, 2525, 10001, 10003, 10005, 20001}

func New(options Options) (*Benchmark, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, errors.New("toolathlon root is required")
	}
	if len(options.TaskIDs) == 0 {
		return nil, errors.New("toolathlon task IDs are required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("toolathlon output directory is required")
	}
	if strings.TrimSpace(options.Revision) == "" {
		return nil, errors.New("toolathlon revision is required")
	}
	if strings.TrimSpace(options.Environment.Image) == "" {
		return nil, errors.New("toolathlon task image is required")
	}
	if options.Environment.Workdir != "" && options.Environment.Workdir != agentWorkspacePath {
		return nil, fmt.Errorf("toolathlon workdir is fixed to %s", agentWorkspacePath)
	}
	if options.GatewayPort == 0 {
		options.GatewayPort = DefaultGatewayPort
	}
	if options.GatewayPort < 1024 || options.GatewayPort > 65535 {
		return nil, errors.New("toolathlon gateway port must be between 1024 and 65535")
	}
	if slices.Contains(applicationPorts, options.GatewayPort) {
		return nil, fmt.Errorf("toolathlon gateway port %d collides with an application port", options.GatewayPort)
	}
	if options.AppHost != "" && !validHost(options.AppHost) {
		return nil, fmt.Errorf("toolathlon application host %q is not a hostname or IP address", options.AppHost)
	}
	if options.MaxSteps == 0 {
		options.MaxSteps = DefaultMaxSteps
	}
	if options.MaxSteps < 0 {
		return nil, errors.New("toolathlon max steps must be positive")
	}
	if options.ModelName == "" {
		options.ModelName = "aries"
	}
	if !safeModelName(options.ModelName) {
		return nil, fmt.Errorf("invalid toolathlon model name %q", options.ModelName)
	}

	seen := make(map[string]struct{}, len(options.TaskIDs))
	for _, id := range options.TaskIDs {
		if !safeTaskID(id) {
			return nil, fmt.Errorf("invalid toolathlon task ID %q", id)
		}
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("duplicate toolathlon task ID %q", id)
		}
		seen[id] = struct{}{}
	}
	executionIDs := options.ExecutionTaskIDs
	if executionIDs == nil {
		executionIDs = options.TaskIDs
	} else if len(executionIDs) != len(options.TaskIDs) {
		return nil, errors.New("toolathlon execution task IDs must match task IDs")
	} else {
		seen = make(map[string]struct{}, len(executionIDs))
		for index, id := range executionIDs {
			if !safeExecutionTaskID(options.TaskIDs[index], id) {
				return nil, fmt.Errorf("invalid toolathlon execution task ID %q", id)
			}
			if _, duplicate := seen[id]; duplicate {
				return nil, fmt.Errorf("duplicate toolathlon execution task ID %q", id)
			}
			seen[id] = struct{}{}
		}
	}

	environment := options.Environment
	environment.Env = maps.Clone(options.Environment.Env)
	environment.Workdir = agentWorkspacePath
	// Toolathlon's public-internet servers (arXiv, Yahoo Finance, fetch)
	// and the loopback forwarder's path to the Docker host both need a
	// non-internal network, so the policy is fixed rather than configurable.
	environment.AllowNetwork = true

	return &Benchmark{
		root:             filepath.Clean(options.Root),
		taskIDs:          slices.Clone(options.TaskIDs),
		executionTaskIDs: slices.Clone(executionIDs),
		outputDir:        filepath.Clean(options.OutputDir),
		revision:         options.Revision,
		environment:      environment,
		gatewayPort:      options.GatewayPort,
		appHost:          options.AppHost,
		maxSteps:         options.MaxSteps,
		modelName:        options.ModelName,
		details:          make(map[string]taskDetails, len(options.TaskIDs)),
	}, nil
}

func (b *Benchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	if err := VerifyRevision(ctx, b.root, b.revision); err != nil {
		return nil, err
	}

	tasks := make([]core.Task, 0, len(b.taskIDs))
	details := make(map[string]taskDetails, len(b.taskIDs))
	for index, id := range b.taskIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		task, private, err := loadTask(b.root, id, b.environment)
		if err != nil {
			return nil, fmt.Errorf("load toolathlon task %q: %w", id, err)
		}
		executionID := b.executionTaskIDs[index]
		task.ID = executionID
		tasks = append(tasks, task)
		details[executionID] = private
	}

	b.mu.Lock()
	b.details = details
	b.mu.Unlock()
	return tasks, nil
}

// loadTask reads one task directory and rejects, before any sandbox exists,
// every task whose MCP servers the adapter cannot provide.
func loadTask(root, id string, environment core.Environment) (core.Task, taskDetails, error) {
	taskDir := filepath.Join(root, "tasks", taskPool, id)
	configBytes, err := os.ReadFile(filepath.Join(taskDir, "task_config.json"))
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("read task config: %w", err)
	}
	var parsed taskConfigFile
	if err := json.Unmarshal(configBytes, &parsed); err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("parse task config: %w", err)
	}
	servers, err := serverNames(parsed.NeededMCPServers)
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("parse needed_mcp_servers: %w", err)
	}
	needsApplications := false
	for _, server := range servers {
		kind, known := serverKinds[server]
		switch {
		case !known:
			return core.Task{}, taskDetails{}, fmt.Errorf("MCP server %q is not in the pinned server catalogue", server)
		case kind == serverUnsupported:
			return core.Task{}, taskDetails{}, fmt.Errorf("MCP server %q needs a third-party account or host runtime the sandbox does not provide", server)
		case kind == serverApplication:
			needsApplications = true
		}
	}

	instructionBytes, err := os.ReadFile(filepath.Join(taskDir, "docs", "task.md"))
	if err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("read task description: %w", err)
	}
	instruction := strings.TrimSpace(string(instructionBytes))
	if instruction == "" {
		return core.Task{}, taskDetails{}, errors.New("task description is empty")
	}
	if _, err := os.Stat(filepath.Join(taskDir, "evaluation")); err != nil {
		return core.Task{}, taskDetails{}, fmt.Errorf("task has no evaluation directory: %w", err)
	}

	task := core.Task{
		ID:          id,
		Instruction: renderInstruction(instruction),
		Environment: environment,
	}
	task.Environment.Env = maps.Clone(environment.Env)
	return task, taskDetails{name: id, servers: servers, needsApplications: needsApplications}, nil
}

// renderInstruction is the task description plus the two facts Toolathlon's
// own agent system prompt gives its agent: where the workspace is, and that
// replying without a tool call ends the task. The harness keeps its own
// system prompt; this is the task-level part only.
func renderInstruction(description string) string {
	return description + "\n\n" +
		"Accessible workspace directory: " + agentWorkspacePath + "\n" +
		"When the task refers to a relative path, it is relative to that directory. " +
		"When you believe the task is complete, reply without calling any tool; " +
		"that ends the task and you will have no further opportunity to work on it."
}

// serverNames accepts Toolathlon's two spellings of needed_mcp_servers: a
// list of names, or an object whose keys are names. Missing means none.
func serverNames(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return normalizeServerNames(list)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, errors.New("must be a list of names or an object keyed by name")
	}
	names := make([]string, 0, len(object))
	for name := range object {
		names = append(names, name)
	}
	sort.Strings(names)
	return normalizeServerNames(names)
}

func normalizeServerNames(names []string) ([]string, error) {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !safeServerName(name) {
			return nil, fmt.Errorf("invalid MCP server name %q", name)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

var (
	taskIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	serverNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	modelNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]*$`)
	hostPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

func safeTaskID(id string) bool {
	return id != "." && id != ".." && len(id) <= 128 && taskIDPattern.MatchString(id)
}

// safeExecutionTaskID accepts the logical ID with the runner's zero-padded
// occurrence suffix, matching the other benchmark packages.
func safeExecutionTaskID(logicalID, id string) bool {
	if len(id) > 149 || !safeTaskID(id) || !strings.HasPrefix(id, logicalID+"-") {
		return false
	}
	suffix := strings.TrimPrefix(id, logicalID+"-")
	if len(suffix) < 3 {
		return false
	}
	index, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && index > 0
}

func safeServerName(name string) bool {
	return name != "." && name != ".." && len(name) <= 64 && serverNamePattern.MatchString(name)
}

func safeModelName(name string) bool {
	return len(name) <= 128 && modelNamePattern.MatchString(name)
}

// validHost accepts a hostname or a bare IP literal, IPv6 included: the
// forwarder passes the value to asyncio.open_connection, which takes both.
func validHost(host string) bool {
	return net.ParseIP(host) != nil || (len(host) <= 253 && hostPattern.MatchString(host))
}
