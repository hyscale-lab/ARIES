// Package replay holds a validated sandbox-call plan for one source occurrence.
package replay

import (
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
	Path           string        `json:"path,omitempty"`
	Body           []byte        `json:"body,omitempty"`
	MaxBytes       int64         `json:"max_bytes,omitempty"`
	FirstLine      int64         `json:"first_line,omitempty"`
	MaxLines       int64         `json:"max_lines,omitempty"`
	MaxLineBytes   int64         `json:"max_line_bytes,omitempty"`
	ResponseSHA256 string        `json:"response_sha256"`
	ResponseBytes  int64         `json:"response_bytes"`
	ResponseCode   string        `json:"response_code"`
}

// Plan holds the complete request schedule and the recorded source evaluation.
type Plan struct {
	Calls        []Call          `json:"calls"`
	FinalWait    time.Duration   `json:"final_wait"`
	SourceRunID  string          `json:"source_run_id"`
	SourceTaskID string          `json:"source_task_id"`
	Evaluation   core.Evaluation `json:"evaluation"`
}
