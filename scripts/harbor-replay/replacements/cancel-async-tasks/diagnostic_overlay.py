"""Add two original-call-specific diagnostics to the sealed replacement adapter."""
from pathlib import Path


B4E_JOIN = "call_IVFjbCkSg5L8zT6Yi0788wdK"
F265_SEARCH = "call_TKcunO6gZG5qPc1QGeSvjWme"

JOIN_REPLY = """
        if call.ToolCallID == cancelAsyncJoinCall && h.FinishObservedProcess != nil &&
            out.ExitCode == ProcessRunning && call.ExitCode == 0 {
            return recordCancelAsyncJoin(ctx, h, call, receipt)
        }
        if call.ToolCallID == cancelAsyncSearchCall && call.ExitCode == 1 && out.ExitCode == 0 {
            receipt.Event = "diagnostic_process_observation"
            receipt.StatusBasis = "recorded_helper_search_fixture_difference"
            return emit(h, receipt)
        }
"""

DIAGNOSTICS = """package harbor

import (
    "context"
    "errors"
    "fmt"

    "github.com/hyscale-lab/aries/pkg/core"
    "github.com/hyscale-lab/aries/pkg/replay"
)

const (
    cancelAsyncJoinCall = "B4E_CALL"
    cancelAsyncSearchCall = "F265_CALL"
)

func IsCancelAsyncJoin(call replay.Call) bool {
    return call.ToolCallID == cancelAsyncJoinCall && call.Operation == "Exec"
}

// ObserveCancelAsync starts the same source command with a private session
// identity. This lets the diagnostic retain its first response before joining.
func (s *ProcessSessions) ObserveCancelAsync(ctx context.Context, call replay.Call) (core.CommandResult, error) {
    call.Operation = "StartProcess"
    call.SessionID = -int(call.Sequence)
    return s.Execute(ctx, call)
}

func (s *ProcessSessions) FinishCancelAsync(ctx context.Context, call replay.Call) (core.CommandResult, error) {
    call.Operation = "PollProcess"
    call.SessionID = -int(call.Sequence)
    return s.FinishObservedProcess(ctx, call)
}

func recordCancelAsyncJoin(ctx context.Context, h Hooks, call replay.Call, first Receipt) error {
    joined, err := h.FinishObservedProcess(ctx, call)
    row := Receipt{
        Event: "diagnostic_process_observation", StatusBasis: "additional_join_after_source_reply",
        Step: first.Step, Line: first.Line, SourceCallID: first.SourceCallID,
        Sequence: first.Sequence, Operation: first.Operation, Expected: &call.ExitCode,
        ProcessTiming: joined.Timing, Stdout: joined.Stdout, Stderr: joined.Stderr,
        StdoutSHA: digest([]byte(joined.Stdout)), StderrSHA: digest([]byte(joined.Stderr)),
    }
    if err == nil {
        row.Exit = &joined.ExitCode
    } else {
        row.StatusBasis = "native_terminal_observation_error"
    }
    if e := errors.Join(err, emit(h, row)); e != nil {
        return e
    }
    if int32(joined.ExitCode) != call.ExitCode {
        return fmt.Errorf("step %d completed with exit %d; recorded %d", first.Step, joined.ExitCode, call.ExitCode)
    }
    return nil
}
"""


def apply(tree: Path, checked_edit):
    harbor = tree/"pkg/replay/harbor"
    (harbor/"cancel_async_diagnostic.go").write_text(
        DIAGNOSTICS.replace("B4E_CALL", B4E_JOIN).replace("F265_CALL", F265_SEARCH)
    )
    edge = "if int32(out.ExitCode) != call.ExitCode {"
    checked_edit(harbor/"run.go", edge, edge + JOIN_REPLY)
    root = tree/"cmd/aries-harbor/main.go"
    edge = "Exec: func(ctx context.Context, call replay.Call) (core.CommandResult, error) {"
    checked_edit(root, edge, edge + """
            if harbor.IsCancelAsyncJoin(call) {
                return sessions.ObserveCancelAsync(ctx, call)
            }
    """)
    edge = "if processDrift {\n\t\th.FinishObservedProcess = sessions.FinishObservedProcess"
    checked_edit(root, edge, """
    if r.Trial == "b4e5be65-a253-4086-bc35-819d746f9549" {
        h.FinishObservedProcess = sessions.FinishCancelAsync
    }
    """ + edge)
