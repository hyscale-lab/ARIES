package harbor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func recordProcessDrift(ctx context.Context, h Hooks, call replay.Call, source Receipt, first core.CommandResult) error {
	event := Receipt{Event: "diagnostic_process_observation", Step: source.Step, Line: source.Line, Sequence: call.Sequence, SessionID: call.SessionID, Operation: call.Operation, Exit: &first.ExitCode, Expected: &call.ExitCode}
	if call.ExitCode == ProcessRunning && first.ExitCode >= 0 {
		event.StatusBasis = "real_terminal_before_source_running_poll"
		return emit(h, event)
	}
	if call.Operation == "PollProcess" && first.ExitCode == ProcessRunning && call.ExitCode >= 0 {
		event.StatusBasis = "real_process_running_at_source_terminal_poll"
		if err := emit(h, event); err != nil {
			return err
		}
		join := func() error {
			last, err := h.FinishObservedProcess(ctx, call)
			event.StatusBasis = "additional_join_after_source_poll"
			event.Exit = &last.ExitCode
			event.Stdout, event.Stderr = last.Stdout, last.Stderr
			event.StdoutSHA, event.StderrSHA = digest([]byte(last.Stdout)), digest([]byte(last.Stderr))
			if e := errors.Join(err, emit(h, event)); e != nil {
				return e
			}
			if int32(last.ExitCode) == call.ExitCode {
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
	return fmt.Errorf("step %d live terminal exit %d, recorded %d", source.Step, first.ExitCode, call.ExitCode)
}
