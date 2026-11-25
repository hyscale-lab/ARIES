// Package replay holds a validated sandbox-call plan for one source occurrence.
package replay

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Call is one recorded tool request and its expected logical outcome.
type Call struct {
	Operation       string        `json:"operation"`
	Sequence        uint64        `json:"sequence"`
	StartOffset     time.Duration `json:"start_offset"`
	ToolCallID      string        `json:"tool_call_id,omitempty"`
	Script          string        `json:"script,omitempty"`
	Login           bool          `json:"login,omitempty"`
	Workdir         string        `json:"workdir,omitempty"`
	Path            string        `json:"path,omitempty"`
	Body            []byte        `json:"body,omitempty"`
	MaxBytes        int64         `json:"max_bytes,omitempty"`
	FirstLine       int64         `json:"first_line,omitempty"`
	MaxLines        int64         `json:"max_lines,omitempty"`
	MaxLineBytes    int64         `json:"max_line_bytes,omitempty"`
	ResponseSHA256  string        `json:"response_sha256,omitempty"`
	ResponseBytes   int64         `json:"response_bytes,omitempty"`
	ResponseCode    string        `json:"response_code,omitempty"`
	HarborOutput    *string       `json:"harbor_output,omitempty"`
	HarborToolError *bool         `json:"harbor_tool_error,omitempty"`
	HarborTruncated bool          `json:"harbor_truncated,omitempty"`
	ExitCode        *int32        `json:"exit_code,omitempty"`
	ResponseDebug   bool          `json:"-"`
	ResponseStdout  []byte        `json:"-"`
	ResponseStderr  []byte        `json:"-"`

	SourceRejection *SourceRejection `json:"source_rejection,omitempty"`
}

const RejectionFileNotRead = "file_not_read"

// SourceRejection records a tool handler refusal before sandbox dispatch.
type SourceRejection struct {
	Reason   string        `json:"reason"`
	Duration time.Duration `json:"duration"`
}

// ScheduleEnd returns the scheduled start for an RPC or return for a source rejection.
func (call Call) ScheduleEnd() (time.Duration, bool) {
	const maxDuration = time.Duration(1<<63 - 1)
	end := call.StartOffset
	if end < 0 {
		return 0, false
	}
	if rejection := call.SourceRejection; rejection != nil {
		if rejection.Duration < 0 || rejection.Duration > maxDuration-end {
			return 0, false
		}
		end += rejection.Duration
	}
	return end, true
}

// StrictExitCode reads the expected exit in a preflighted plan.
// Legacy staged plans omitted a recorded zero.
func (call Call) StrictExitCode() int32 {
	if call.ExitCode != nil {
		return *call.ExitCode
	}
	return 0
}

const (
	OriginARIES  = "aries"
	OriginHarbor = "harbor"
)

// Plan holds the complete request schedule and the recorded source evaluation.
type Plan struct {
	Calls             []Call        `json:"calls"`
	FinalWait         time.Duration `json:"final_wait"`
	SourceRunID       string        `json:"source_run_id"`
	SourceTaskID      string        `json:"source_task_id"`
	OriginType        string        `json:"origin_type"`
	TaskPackageDigest string        `json:"task_package_digest,omitempty"`
	SkipResponseCheck bool          `json:"skip_response_check,omitempty"`
	// Evaluation reports unavailable when the source has no grade.
	Evaluation core.Evaluation `json:"evaluation"`
	// StartedAt is the recorded source timeline origin, not a replay scheduling epoch.
	StartedAt time.Time `json:"-"`
	// ReplayAt is the shared replay wall-clock origin. Nil uses caller-local timing.
	ReplayAt *time.Time `json:"replay_at,omitempty"`
}

// DispatchCount is the number of calls that must appear in the replay bridge audit.
func (plan Plan) DispatchCount() int {
	count := 0
	for _, call := range plan.Calls {
		if call.SourceRejection == nil {
			count++
		}
	}
	return count
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
