package harbor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func recordProcessDrift(ctx context.Context, h Hooks, call replay.Call, source Receipt, first core.CommandResult) error {
	event := Receipt{Event: "diagnostic_process_observation", SourceCallID: source.SourceCallID, Step: source.Step, Line: source.Line, Sequence: call.Sequence, SessionID: call.SessionID, Operation: call.Operation, Exit: &first.ExitCode, Expected: &call.ExitCode}
	if call.ExitCode == ProcessRunning && first.ExitCode >= 0 {
		event.StatusBasis = "real_terminal_before_source_running_poll"
		return emit(h, event)
	}
	if (call.Operation == "PollProcess" || ((h.RelaxedMeasuredStatus || h.KeepBoundedPollResult) && call.Operation == "Exec")) &&
		first.ExitCode == ProcessRunning && call.ExitCode >= 0 {
		event.StatusBasis = "real_process_running_at_source_terminal_poll"
		if call.Operation == "Exec" {
			event.StatusBasis = "real_process_running_at_source_terminal_reply"
			event.SessionID = 0
			event.DiagnosticID = diagnosticProcessID(call)
		}
		if err := emit(h, event); err != nil || h.FinishObservedProcess == nil {
			return err
		}
		join := func() error {
			event.StatusBasis = "additional_join_after_source_poll"
			if call.Operation == "Exec" {
				event.StatusBasis = "additional_join_after_source_reply"
			}
			dispatch := event
			dispatch.Event = "diagnostic_process_dispatch"
			dispatch.Exit = nil
			if err := emit(h, dispatch); err != nil {
				return err
			}
			last, err := h.FinishObservedProcess(ctx, call)
			event.Exit = &last.ExitCode
			event.ProcessTiming = last.Timing
			event.Stdout, event.Stderr = last.Stdout, last.Stderr
			event.StdoutSHA, event.StderrSHA = digest([]byte(last.Stdout)), digest([]byte(last.Stderr))
			if h.RelaxedMeasuredStatus && err != nil {
				event.Exit = nil
				event.NativeError = err.Error()
				event.StatusBasis = "native_terminal_observation_error"
			}
			if e := errors.Join(err, emit(h, event)); e != nil {
				return e
			}
			if h.KeepBoundedPollResult || h.RelaxedMeasuredStatus || int32(last.ExitCode) == call.ExitCode {
				return nil
			}
			return fmt.Errorf("step %d observed terminal exit %d, recorded %d", source.Step, last.ExitCode, call.ExitCode)
		}
		if h.deferProcessJoin != nil {
			h.deferProcessJoin(join)
			return nil
		}
		return join()
	}
	if h.RelaxedMeasuredStatus {
		event.StatusBasis = "real_terminal_status_differs_from_source"
		return emit(h, event)
	}
	return fmt.Errorf("step %d live terminal exit %d, recorded %d", source.Step, first.ExitCode, call.ExitCode)
}
