package core

// BridgeTarget binds one stable sandbox identity to an exact deployment runtime.
// Run/task fields correlate experiment evidence; they are not lifecycle handles.
type BridgeTarget struct {
	SandboxID string `json:"sandbox_id"`
	RunID     string `json:"run_id"`
	TaskID    string `json:"task_id"`
	Backend   string `json:"backend"`
	RuntimeID string `json:"runtime_id"`
	Workdir   string `json:"workdir"`
	ExecUser  string `json:"exec_user,omitempty"`
}
