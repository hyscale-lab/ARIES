package core

// BridgeTarget is a credential-free, immutable grant to one task occurrence.
// Infrastructure credentials are always staged separately.
type BridgeTarget struct {
	Version        int               `json:"version"`
	RunID          string            `json:"run_id"`
	TaskID         string            `json:"task_id"`
	OccurrenceID   string            `json:"occurrence_id"`
	Backend        string            `json:"backend"`
	RuntimeID      string            `json:"runtime_id"`
	RuntimeName    string            `json:"runtime_name"`
	ExpectedLabels map[string]string `json:"expected_labels"`
	Workdir        string            `json:"workdir"`
	ExecUser       string            `json:"exec_user,omitempty"`
	MaxInputBytes  int64             `json:"max_input_bytes"`
	MaxOutputBytes int64             `json:"max_output_bytes"`
}
