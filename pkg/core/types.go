package core

import "time"

// Task is the benchmark-independent description of one unit of work.
type Task struct {
	ID          string      `json:"id"`
	Instruction string      `json:"instruction"`
	Environment Environment `json:"environment"`
}

// Environment describes the task sandbox requested by a benchmark.
type Environment struct {
	Image        string            `json:"image"`
	BuildDir     string            `json:"build_dir,omitempty"`
	Workdir      string            `json:"workdir"`
	CPU          float64           `json:"cpu,omitempty"`
	MemoryMB     int               `json:"memory_mb,omitempty"`
	StorageMB    int               `json:"storage_mb,omitempty"`
	GPUs         int               `json:"gpus,omitempty"`
	AllowNetwork bool              `json:"allow_network"`
	Env          map[string]string `json:"env,omitempty"`
}

// SandboxRequest carries stable run and task identity separately from the
// benchmark-defined execution environment.
type SandboxRequest struct {
	RunID       string      `json:"run_id"`
	TaskID      string      `json:"task_id"`
	Environment Environment `json:"environment"`
}

// Command is an argument-safe process invocation inside a sandbox.
type Command struct {
	Path    string            `json:"path"`
	Args    []string          `json:"args,omitempty"`
	Dir     string            `json:"dir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Stdin   []byte            `json:"-"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// CommandResult records a completed sandbox process.
type CommandResult struct {
	ExitCode int           `json:"exit_code"`
	Stdout   string        `json:"stdout,omitempty"`
	Stderr   string        `json:"stderr,omitempty"`
	Duration time.Duration `json:"duration"`
}

// ModelConfig identifies a remote OpenAI-compatible model without containing
// an API-key value.
type ModelConfig struct {
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

// ToolEndpoint is the bridge endpoint and task-local file contract given to a
// harness. Credential bytes are never carried in this value. Source files stay
// private on the host: a harness must copy credentials and config into a
// root-initialized private volume, change ownership to its runtime user, keep
// mode 0600, and mount that volume read-only. Source paths are not direct
// container bind mounts.
type ToolEndpoint struct {
	Protocol             string `json:"protocol"`
	Address              string `json:"address"`
	Username             string `json:"username,omitempty"`
	Network              string `json:"network,omitempty"`
	ClientCommand        string `json:"client_command,omitempty"`
	ClientSourceFile     string `json:"client_source_file,omitempty"`
	IdentityFile         string `json:"identity_file,omitempty"`
	IdentitySourceFile   string `json:"identity_source_file,omitempty"`
	KnownHostsFile       string `json:"known_hosts_file,omitempty"`
	KnownHostsSourceFile string `json:"known_hosts_source_file,omitempty"`
}

// HarnessRequest contains task-local runtime inputs supplied before Run.
type HarnessRequest struct {
	RunID     string       `json:"run_id"`
	TaskID    string       `json:"task_id"`
	Endpoint  ToolEndpoint `json:"tool_endpoint"`
	Model     ModelConfig  `json:"model"`
	OutputDir string       `json:"output_dir"`
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
	TaskID     string          `json:"task_id"`
	Harness    HarnessResult   `json:"harness"`
	Isolation  IsolationResult `json:"isolation"`
	Evaluation Evaluation      `json:"evaluation"`
	Observer   ObserverResult  `json:"observer"`
	Cleanup    CleanupResult   `json:"cleanup"`
	Duration   time.Duration   `json:"duration"`
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
	Name     string        `json:"name"`
	RunID    string        `json:"run_id"`
	Tasks    []TaskResult  `json:"tasks"`
	Summary  RunSummary    `json:"summary"`
	Duration time.Duration `json:"duration"`
}
