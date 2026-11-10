// Package replay holds a validated sandbox-call plan for one source occurrence.
package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Call is one recorded sandbox request and its expected logical outcome.
type Call struct {
	Operation      string        `json:"operation"`
	Sequence       uint64        `json:"sequence"`
	StartOffset    time.Duration `json:"start_offset"`
	ToolCallID     string        `json:"tool_call_id,omitempty"`
	Script         string        `json:"script,omitempty"`
	Login          bool          `json:"login,omitempty"`
	Workdir        string        `json:"workdir,omitempty"`
	Path           string        `json:"path,omitempty"`
	Body           []byte        `json:"body,omitempty"`
	MaxBytes       int64         `json:"max_bytes,omitempty"`
	FirstLine      int64         `json:"first_line,omitempty"`
	MaxLines       int64         `json:"max_lines,omitempty"`
	MaxLineBytes   int64         `json:"max_line_bytes,omitempty"`
	ResponseSHA256 string        `json:"response_sha256"`
	ResponseBytes  int64         `json:"response_bytes"`
	ResponseCode   string        `json:"response_code"`
	ExitCode       int32         `json:"exit_code,omitempty"`
	ResponseDebug  bool          `json:"-"`
	ResponseStdout []byte        `json:"-"`
	ResponseStderr []byte        `json:"-"`
}

// Plan holds the complete request schedule and the recorded source evaluation.
type Plan struct {
	Calls        []Call          `json:"calls"`
	FinalWait    time.Duration   `json:"final_wait"`
	SourceRunID  string          `json:"source_run_id"`
	SourceTaskID string          `json:"source_task_id"`
	Evaluation   core.Evaluation `json:"evaluation"`
	// StartedAt is the recorded source timeline origin, not a replay scheduling epoch.
	StartedAt time.Time `json:"-"`
}

// MaxPlanBytes bounds the managed caller's encoded plan.
// A plan expands retained request bytes through JSON base64 encoding.
const MaxPlanBytes = 512 << 20

// EncodePlan uses the managed caller's encoding and rejects data above the bound.
func EncodePlan(plan Plan, maxBytes int) ([]byte, error) {
	data, err := json.Marshal(plan)
	if err != nil || len(data) > maxBytes {
		return nil, errors.Join(err, errors.New("replay caller plan is too large or invalid"))
	}
	return data, nil
}

// ValidateWorkdir checks Exec calls against the selected or resolved workdir.
func (plan Plan) ValidateWorkdir(workdir string) error {
	for _, call := range plan.Calls {
		if call.Operation == "Exec" && call.Workdir != workdir {
			return fmt.Errorf("replay call %d Exec workdir %q differs from replay workdir %q", call.Sequence, call.Workdir, workdir)
		}
	}
	return nil
}
