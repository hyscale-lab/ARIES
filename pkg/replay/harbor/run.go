package harbor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"golang.org/x/sys/unix"
)

// Hooks stay on the host. Boundary returns only after the VM is running again.
// Its own freeze receipts identify the exact pause/resume bounds.
type Hooks struct {
	Exec      func(context.Context, replay.Call) (core.CommandResult, error)
	Boundary  func(context.Context, string, int) error
	Record    func(Receipt) error
	IntraTool *IntraToolOptions
}

type Receipt struct {
	Event            string `json:"event"`
	Operation        string `json:"operation,omitempty"`
	StatusBasis      string `json:"status_basis,omitempty"`
	Time             int64  `json:"monotonic_ns"`
	Step             int    `json:"step_id,omitempty"`
	Line             int    `json:"rollout_line,omitempty"`
	Sequence         uint64 `json:"sequence,omitempty"`
	Basis            string `json:"basis,omitempty"`
	RequestedNS      int64  `json:"requested_ns,omitempty"`
	Exit             *int   `json:"exit_code,omitempty"`
	Expected         *int32 `json:"expected_exit_code,omitempty"`
	StdoutSHA        string `json:"stdout_sha256,omitempty"`
	StderrSHA        string `json:"stderr_sha256,omitempty"`
	Stdout           string `json:"stdout,omitempty"`
	Stderr           string `json:"stderr,omitempty"`
	OutputComparison string `json:"output_comparison,omitempty"`
	SourceOutputSHA  string `json:"source_output_sha256,omitempty"`
	CaptureEndNS     int64  `json:"capture_end_monotonic_ns,omitempty"`
	CaptureIndex     int    `json:"capture_index,omitempty"`
}

func Clock() (int64, error) {
	var t unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &t); err != nil {
		return 0, err
	}
	return t.Nano(), nil
}
func emit(h Hooks, r Receipt) error {
	t, err := Clock()
	if err != nil {
		return err
	}
	r.Time = t
	return h.Record(r)
}

func compareOutput(source []byte, result core.CommandResult) string {
	if strings.Contains(string(source), "Warning: truncated output") || strings.Contains(string(source), "tokens truncated") {
		return "source_truncated"
	}
	if result.Stdout != "" && result.Stderr != "" {
		return "interleaving_unavailable"
	}
	if strings.TrimRight(string(source), "\n") == strings.TrimRight(result.Stdout+result.Stderr, "\n") {
		return "matched_ignoring_trailing_newlines"
	}
	return "differed"
}

// Run follows source order, substituting live elapsed time for each recorded
// guest call interval. There is no verifier, benchmark deadline or LLM client.
func Run(ctx context.Context, r *Recording, h Hooks) (runErr error) {
	if h.Exec == nil || h.Boundary == nil || h.Record == nil {
		return errors.New("missing replay hooks")
	}
	if err := h.IntraTool.Validate(r); err != nil {
		return err
	}
	defer func() {
		// A failed initial capture may have acquired observers that still need release.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		runErr = errors.Join(runErr, h.Boundary(cleanup, "session-end", 0))
		if runErr == nil {
			runErr = emit(h, Receipt{Event: "session_end"})
		}
	}()
	if err := emit(h, Receipt{Event: "session_start"}); err != nil {
		return err
	}
	if err := h.Boundary(ctx, "session-start", 0); err != nil {
		return err
	}
	for _, e := range r.Events {
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt := Receipt{Event: "source_event", Line: e.Line, Step: e.Step, Sequence: e.Sequence, Basis: e.Basis}
		if e.Wait > 0 {
			receipt.Event = "wait_start"
			receipt.RequestedNS = int64(e.Wait)
			if err := emit(h, receipt); err != nil {
				return err
			}
			timer := time.NewTimer(e.Wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			receipt.Event = "wait_end"
			if err := emit(h, receipt); err != nil {
				return err
			}
			receipt.Event = "source_event"
		}
		if err := emit(h, receipt); err != nil {
			return err
		}
		if e.Kind != "exec_command" && e.Kind != "view_image" {
			continue
		}
		call := r.Plan.Calls[e.Sequence-1]
		receipt.Operation = call.Operation
		if call.Operation == "ViewImage" {
			receipt.StatusBasis = "recorded_high_detail_image_attachment"
		}
		if err := h.Boundary(ctx, "pre-tool", e.Step); err != nil {
			return err
		}
		receipt.Event = "tool_dispatch"
		if err := emit(h, receipt); err != nil {
			return err
		}
		out, replyTime, err := h.executeTool(ctx, call, receipt)
		captureErr := h.Boundary(ctx, "post-tool", e.Step)
		receipt.Event = "tool_reply"
		receipt.Time = replyTime
		receipt.OutputComparison = compareOutput(call.ResponseStdout, out)
		receipt.SourceOutputSHA = digest(call.ResponseStdout)
		receipt.Exit = &out.ExitCode
		receipt.Expected = &call.ExitCode
		receipt.Stdout = out.Stdout
		receipt.Stderr = out.Stderr
		receipt.StdoutSHA = digest([]byte(out.Stdout))
		receipt.StderrSHA = digest([]byte(out.Stderr))
		if err = errors.Join(err, captureErr, h.Record(receipt)); err != nil {
			return err
		}
		if int32(out.ExitCode) != call.ExitCode {
			return fmt.Errorf("step %d live exit %d, recorded %d", e.Step, out.ExitCode, call.ExitCode)
		}
	}
	return nil
}
