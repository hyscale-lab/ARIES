package core

import "time"

// Task is the benchmark-independent description of one unit of work.
type Task struct {
	ID          string        `json:"id"`
	Instruction string        `json:"instruction"`
	Timeout     time.Duration `json:"timeout,omitempty"`
	Environment Environment   `json:"environment"`
	// NoSandboxTools withholds the harness's own tools that act in the
	// sandbox (a shell, code execution, file access) for this task, so the
	// agent acts only through its MCP servers and the harness's other tools
	// (web search). A benchmark whose tasks list their own tools sets it for
	// a task that lists none of these, to keep to the benchmark (Toolathlon).
	// False keeps every harness tool.
	NoSandboxTools bool `json:"no_sandbox_tools,omitempty"`
}

// Environment describes the task sandbox requested by a benchmark.
type Environment struct {
	Image        string            `json:"image"`
	Workdir      string            `json:"workdir"`
	CPU          float64           `json:"cpu,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	StorageMB    int               `json:"storage_mb,omitempty"`
	GPUs         int               `json:"gpus,omitempty"`
	AllowNetwork bool              `json:"allow_network"`
	Env          map[string]string `json:"env,omitempty"`
	Services     TaskServices      `json:"services,omitempty"`
	ExecUser     string            `json:"-"`
}

// SandboxPurposeEvaluation marks a fresh sandbox started for evaluation rather
// than for agent access.
const SandboxPurposeEvaluation = "evaluation"

// SandboxRequest carries stable run and task identity separately from the
// benchmark-defined execution environment. An empty Purpose is the task
// sandbox the agent uses.
type SandboxRequest struct {
	RunID       string      `json:"run_id"`
	TaskID      string      `json:"task_id"`
	Purpose     string      `json:"purpose,omitempty"`
	Environment Environment `json:"environment"`
}

// Command is an argument-safe process invocation inside a sandbox.
type Command struct {
	Path             string            `json:"path"`
	Args             []string          `json:"args,omitempty"`
	Dir              string            `json:"dir,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Stdin            []byte            `json:"-"`
	Timeout          time.Duration     `json:"timeout,omitempty"`
	User             string            `json:"-"`
	OutputLimitBytes int               `json:"-"`
}

// CommandResult records a completed sandbox process.
type CommandResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty"`
	Duration time.Duration `json:"duration"`
}

// ResourceReading is one runtime-neutral resource observation. CPU usage is
// cumulative; memory and optional GPU values are gauges. Concrete deployment
// packages collect readings while monitor derives rates and writes artifacts.
type ResourceReading struct {
	Scope               string
	RunID               string
	TaskID              string
	Component           string
	RuntimeID           string
	RuntimeName         string
	ObservedAt          time.Time
	CPUUsageNanoseconds uint64
	MemoryUsageBytes    uint64
	MemoryLimitBytes    uint64
	GPU                 *GPUResourceReading
}

// GPUResourceReading contains one device-level NVIDIA observation.
type GPUResourceReading struct {
	Index                    int      `json:"index"`
	UUID                     string   `json:"uuid"`
	UtilizationPercent       *float64 `json:"utilization_percent,omitempty"`
	MemoryUtilizationPercent *float64 `json:"memory_utilization_percent,omitempty"`
	MemoryUsageBytes         uint64   `json:"memory_usage_bytes"`
	MemoryTotalBytes         uint64   `json:"memory_total_bytes"`
	PowerWatts               *float64 `json:"power_watts,omitempty"`
	TemperatureCelsius       *float64 `json:"temperature_celsius,omitempty"`
}

// ModelConfig identifies a remote OpenAI-compatible model without containing
// an API-key value.
type ModelConfig struct {
	Provider  string `json:"provider"`
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
	// ReasoningEffort controls model requests; empty preserves consumer defaults.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ContextLength, MaxTokens, and Temperature are optional generation
	// settings. Zero or nil keeps the consumer default. Consumers validate
	// which settings they support.
	ContextLength int      `json:"context_length,omitempty"`
	MaxTokens     int      `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
}

// BridgeListen separates a local listener address from the address reached by the harness.
// Zero ports request dynamic allocation; an omitted advertised port follows the bound port.
type BridgeListen struct {
	BindPort      int
	AdvertisePort int
	BindHost      string
	AdvertiseHost string
}

// ToolEndpoint supplies a reachable protocol endpoint and optional client binary.
type ToolEndpoint struct {
	Protocol         string   `json:"protocol"`
	Address          string   `json:"address"`
	Username         string   `json:"username,omitempty"`
	ClientCommand    string   `json:"client_command,omitempty"`
	ClientSourceFile string   `json:"client_source_file,omitempty"`
	LogPaths         []string `json:"log_paths,omitempty"`
	// Workdir is the sandbox directory the endpoint runs agent commands in,
	// for a harness that must name it to its own tool layer.
	Workdir string `json:"workdir,omitempty"`
}

// RuntimePlacement carries the task attachment to the deployment implementation.
// Backend-specific attachment identities remain owned by RunEnvironment.
type RuntimePlacement struct {
	AttachmentID string `json:"attachment_id,omitempty"`
}

// TaskServices declares services the benchmark starts during preparation.
// A zero port means that the task does not provide that service.
type TaskServices struct {
	SearchPort int `json:"search_port,omitempty"`
}

// HarnessConnectivity supplies placement and addresses resolved for that placement.
// Service addresses contain no credentials.
type HarnessConnectivity struct {
	Placement RuntimePlacement `json:"placement"`
	SearchURL string           `json:"search_url,omitempty"`
}

// HarnessRequest contains task-local runtime inputs supplied before Run.
type HarnessRequest struct {
	Connectivity HarnessConnectivity `json:"connectivity"`
	RunID        string              `json:"run_id"`
	TaskID       string              `json:"task_id"`
	Endpoint     ToolEndpoint        `json:"tool_endpoint"`
	Model        ModelConfig         `json:"model"`
	Timeout      time.Duration       `json:"timeout,omitempty"`
	CPU          *float64            `json:"cpu,omitempty"`
	MemoryMB     *int                `json:"memory_mb,omitempty"`
	OutputDir    string              `json:"output_dir"`
	// NoSandboxTools is the task's Task.NoSandboxTools: the harness renders
	// none of its own shell, code-execution or file tools.
	NoSandboxTools bool `json:"no_sandbox_tools,omitempty"`
}

const (
	StatusNotStarted       = "not_started"
	StatusNotRun           = "not_run"
	StatusSucceeded        = "succeeded"
	StatusFailed           = "failed"
	StatusCanceled         = "canceled"
	StatusConfirmed        = "confirmed"
	StatusBlockedIsolation = "blocked_isolation"
	StatusNotEnabled       = "not_enabled"
	StatusNotNeeded        = "not_needed"
	StatusUnsupported      = "unsupported"
)

// HarnessResult is independent from evaluation and cleanup outcomes.
type HarnessResult struct {
	Status        string        `json:"status"`
	FinalResponse string        `json:"final_response,omitempty"`
	Duration      time.Duration `json:"duration"`
	LogPaths      []string      `json:"log_paths,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// IsolationResult records the two positive gates required before evaluation.
type IsolationResult struct {
	Status         string `json:"status"`
	HarnessStopped bool   `json:"harness_stopped"`
	BridgeRevoked  bool   `json:"bridge_revoked"`
	Error          string `json:"error,omitempty"`
}

// Evaluation is produced by the benchmark, independently of the harness.
type Evaluation struct {
	Status         string        `json:"status"`
	Score          float64       `json:"score"`
	Reward         float64       `json:"reward"`
	VerifierStatus string        `json:"verifier_status,omitempty"`
	Duration       time.Duration `json:"duration"`
	LogPaths       []string      `json:"log_paths,omitempty"`
	Error          string        `json:"error,omitempty"`
}

// ObserverResult records observer-only evidence composed outside the Runner.
type ObserverResult struct {
	Reason      string        `json:"reason,omitempty"`
	Status      string        `json:"status"`
	Duration    time.Duration `json:"duration"`
	SampleCount int           `json:"sample_count"`
	LogPaths    []string      `json:"log_paths,omitempty"`
	Error       string        `json:"error,omitempty"`
}

// CleanupResult is separate from functional and evaluation outcomes.
type CleanupResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// TaskResult preserves each outcome even when several operations fail.
type TaskResult struct {
	TaskID       string          `json:"task_id"`
	ToolLogPaths []string        `json:"tool_log_paths,omitempty"`
	Harness      HarnessResult   `json:"harness"`
	Isolation    IsolationResult `json:"isolation"`
	Evaluation   Evaluation      `json:"evaluation"`
	Observer     ObserverResult  `json:"observer"`
	Cleanup      CleanupResult   `json:"cleanup"`
	// Error joins every failure of the task, including sandbox preparation
	// failures that leave the harness not started.
	Error string `json:"error,omitempty"`
	// StartedAt is when the runner began the task: with an arrival schedule
	// it is the realised start, which an analysis sets against the offset the
	// schedule asked for.
	StartedAt time.Time     `json:"started_at,omitempty"`
	Duration  time.Duration `json:"duration"`
}

// RunSummary is a direct count of task outcomes.
type RunSummary struct {
	Tasks                int `json:"tasks"`
	HarnessSucceeded     int `json:"harness_succeeded"`
	HarnessFailed        int `json:"harness_failed"`
	EvaluationsRun       int `json:"evaluations_run"`
	EvaluationsSucceeded int `json:"evaluations_succeeded"`
	EvaluationsFailed    int `json:"evaluations_failed"`
	EvaluationsBlocked   int `json:"evaluations_blocked"`
	CleanupFailed        int `json:"cleanup_failed"`
}

// RunResult contains all task results and their aggregate counts.
type RunResult struct {
	Infrastructure *InfrastructureResult `json:"infrastructure,omitempty"`
	Name           string                `json:"name"`
	RunID          string                `json:"run_id"`
	Tasks          []TaskResult          `json:"tasks"`
	Summary        RunSummary            `json:"summary"`
	Duration       time.Duration         `json:"duration"`
}

// InfrastructureResult records shared costs and failures without adding a task.
type InfrastructureResult struct {
	NetworkID         string         `json:"network_id,omitempty"`
	NetworkName       string         `json:"network_name,omitempty"`
	NetworkPolicy     string         `json:"network_policy"`
	BridgeRuntimeID   string         `json:"bridge_runtime_id,omitempty"`
	BridgeRuntimeName string         `json:"bridge_runtime_name,omitempty"`
	Observer          ObserverResult `json:"observer"`
	Cleanup           CleanupResult  `json:"cleanup"`
	Error             string         `json:"error,omitempty"`
	StartupDuration   time.Duration  `json:"startup_duration"`
	CleanupDuration   time.Duration  `json:"cleanup_duration"`
}
