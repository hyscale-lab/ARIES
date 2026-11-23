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
	deferProcessJoin      func(func() error)
	WasRetained           func(int) bool
	FinishObservedProcess func(context.Context, replay.Call) (core.CommandResult, error)
	DiagnosticVideoImages bool
	Exec                  func(context.Context, replay.Call) (core.CommandResult, error)
	Boundary              func(context.Context, string, int) error
	Record                func(Receipt) error
	IntraTool             *IntraToolOptions
}

type Receipt struct {
	ProcessTiming    *core.CommandTiming `json:"guest_process_timing,omitempty"`
	SourceYieldMS    int64               `json:"source_yield_ms,omitempty"`
	SourceCallID     string              `json:"source_call_id,omitempty"`
	SourceKind       string              `json:"source_kind,omitempty"`
	ParallelSize     int                 `json:"parallel_size,omitempty"`
	Event            string              `json:"event"`
	SessionID        int                 `json:"source_process_session_id,omitempty"`
	Operation        string              `json:"operation,omitempty"`
	StatusBasis      string              `json:"status_basis,omitempty"`
	Time             int64               `json:"monotonic_ns"`
	Step             int                 `json:"step_id,omitempty"`
	Line             int                 `json:"rollout_line,omitempty"`
	Sequence         uint64              `json:"sequence,omitempty"`
	Basis            string              `json:"basis,omitempty"`
	RequestedNS      int64               `json:"requested_ns,omitempty"`
	Exit             *int                `json:"exit_code,omitempty"`
	Expected         *int32              `json:"expected_exit_code,omitempty"`
	StdoutSHA        string              `json:"stdout_sha256,omitempty"`
	StderrSHA        string              `json:"stderr_sha256,omitempty"`
	Stdout           string              `json:"stdout,omitempty"`
	Stderr           string              `json:"stderr,omitempty"`
	OutputComparison string              `json:"output_comparison,omitempty"`
	SourceOutputSHA  string              `json:"source_output_sha256,omitempty"`
	CaptureEndNS     int64               `json:"capture_end_monotonic_ns,omitempty"`
	CaptureIndex     int                 `json:"capture_index,omitempty"`
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
	if h.DiagnosticVideoImages && r.Task != "video-processing" {
		return errors.New("unreviewed video diagnostic")
	}
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
	for i := 0; i < len(r.Events); i++ {
		e := r.Events[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		receipt, err := recordSourceEvent(ctx, h, e)
		if err != nil {
			return err
		}
		if e.ParallelSize > 1 {
			end := i + 2*e.ParallelSize
			if end > len(r.Events) {
				return errors.New("invalid parallel event span")
			}
			if err := runParallel(ctx, h, r, r.Events[i:end], receipt); err != nil {
				return err
			}
			i = end - 1
			continue
		}
		if e.Kind != "exec_command" && e.Kind != "view_image" && e.Kind != "write_stdin" {
			continue
		}
		call := r.Plan.Calls[e.Sequence-1]
		receipt = toolReceipt(h, receipt, call)
		if err := h.Boundary(ctx, "pre-tool", e.Step); err != nil {
			return err
		}
		receipt.Event = "tool_dispatch"
		if err := emit(h, receipt); err != nil {
			return err
		}
		out, replyTime, err := h.executeTool(ctx, call, receipt)
		captureErr := h.Boundary(ctx, "post-tool", e.Step)
		if err := recordReply(ctx, h, call, receipt, out, replyTime, errors.Join(err, captureErr)); err != nil {
			return err
		}
	}
	return nil
}

func recordSourceEvent(ctx context.Context, h Hooks, e Event) (Receipt, error) {
	receipt := Receipt{Event: "source_event", SourceKind: e.Kind, SourceCallID: e.CallID, Line: e.Line, Step: e.Step, Sequence: e.Sequence, Basis: e.Basis, ParallelSize: e.ParallelSize}
	if e.Wait > 0 {
		receipt.Event = "wait_start"
		receipt.RequestedNS = int64(e.Wait)
		if err := emit(h, receipt); err != nil {
			return receipt, err
		}
		timer := time.NewTimer(e.Wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return receipt, ctx.Err()
		case <-timer.C:
		}
		receipt.Event = "wait_end"
		if err := emit(h, receipt); err != nil {
			return receipt, err
		}
		receipt.Event = "source_event"
	}
	if err := emit(h, receipt); err != nil {
		return receipt, err
	}

	return receipt, nil
}
func toolReceipt(h Hooks, receipt Receipt, call replay.Call) Receipt {
	receipt.Operation = call.Operation
	if call.SessionID != 0 {
		receipt.SessionID = call.SessionID
		receipt.StatusBasis = "live_process_status_or_exit"
		if call.Operation == "PollProcess" && h.WasRetained != nil && h.WasRetained(call.SessionID) {
			receipt.StatusBasis = "retained_terminal_status_empty_poll"
		}
	}
	if call.Operation == "ViewImage" {
		receipt.StatusBasis = "recorded_high_detail_image_attachment"
	}

	return receipt
}
func recordReply(ctx context.Context, h Hooks, call replay.Call, receipt Receipt, out core.CommandResult, replyTime int64, callErr error) error {
	receipt.Event = "tool_reply"
	receipt.Time = replyTime
	receipt.OutputComparison = compareOutput(call.ResponseStdout, out)
	receipt.SourceOutputSHA = digest(call.ResponseStdout)
	receipt.ProcessTiming = out.Timing
	receipt.SourceYieldMS = call.YieldTime.Milliseconds()
	receipt.Exit = &out.ExitCode
	receipt.Expected = &call.ExitCode
	receipt.Stdout = out.Stdout
	receipt.Stderr = out.Stderr
	receipt.StdoutSHA = digest([]byte(out.Stdout))
	receipt.StderrSHA = digest([]byte(out.Stderr))
	if err := errors.Join(callErr, h.Record(receipt)); err != nil {
		return err
	}
	if call.Operation == "ViewImage" && out.Stdout != string(call.ResponseStdout) {
		if h.DiagnosticVideoImages && out.ExitCode == 0 && comparableLiveImage(call, out.Stdout) {
			return nil
		}
		return fmt.Errorf("step %d image attachment differs from the source", receipt.Step)
	}
	if int32(out.ExitCode) != call.ExitCode {
		if h.FinishObservedProcess != nil && call.SessionID != 0 {
			return recordProcessDrift(ctx, h, call, receipt, out)
		}
		return fmt.Errorf("step %d live exit %d, recorded %d", receipt.Step, out.ExitCode, call.ExitCode)
	}
	return nil
}
