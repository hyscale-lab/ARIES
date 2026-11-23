"""Add only the retained ordinary Exec boundary proven by the prior prefix."""
from pathlib import Path
import subprocess

SOURCE_STEP = 17
SOURCE_CALL = "call_zkHevzPcbGrer7T32te3CvRp"
ADDITIONAL_STEP = 16
ADDITIONAL_CALL = "call_wcYqo0dPntHFaw4od1NaAv7d"
FINAL_EXEC_BOUNDARIES = (
    (18, "call_vx0lmrkNblwtPdlqWsSKnkyb"),
    (19, "call_nVRzvPg3Diyf4IlgSOML0QvT"),
    (24, "call_4AKIw7pL8TyuPccKQXpsnXB6"),
)
JOIN_TIMEOUT_S = 45
PROCESS_TRACKING_REVISION = "6ecdcc0fc73f6a018ab6541ab9c7bbb4bb75515f"


def boundaries(additional, through_final):
    chosen = [(SOURCE_STEP, SOURCE_CALL)]
    if additional:
        chosen.append((ADDITIONAL_STEP, ADDITIONAL_CALL))
    if through_final:
        chosen.extend(FINAL_EXEC_BOUNDARIES)
    return chosen


def apply(tree, edit, recording, git_root, additional=False, through_final=False):
    approved = boundaries(additional, through_final)
    calls = []
    gates = []
    for source_step, source_call in approved:
        step, = [row for row in recording["steps"] if row["step_id"] == source_step]
        calls_at_boundary = [c for c in step["tool_calls"]
                             if c["tool_call_id"] == source_call and c["function_name"] == "exec_command"]
        if len(calls_at_boundary) != 1:
            raise ValueError("unexpected continuation launch")
        calls.append(f'call.ToolCallID == "{source_call}"')
        gates.append(f'(receipt.Step == {source_step} && call.ToolCallID == "{source_call}")')
    cli_gate = " || ".join(calls)
    replay_gate = " || ".join(gates)
    # Reuse the tested ProcessSessions retention for ordinary exec_command.
    for name in ("process.go", "process_test.go"):
        path = Path("pkg/replay/harbor", name)
        (tree/path).write_bytes(subprocess.check_output(
            ["git", "show", f"{PROCESS_TRACKING_REVISION}:{path}"], cwd=git_root))
    cli = tree/"cmd/aries-harbor/main.go"
    execute='if call.Operation == "StartProcess" || call.Operation == "PollProcess" {'
    edit(cli, execute, f'''if processDrift && isVerifiedTerminalExec(call) {{
                return sessions.ObserveRelaxed(ctx, call)
            }}
            {execute}''')
    finish='h.FinishObservedProcess = sessions.FinishObservedProcess'
    edit(cli, finish, '''h.FinishObservedProcess = func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
            if isVerifiedTerminalExec(call) {
                return sessions.FinishRelaxed(ctx, call)
            }
            return sessions.FinishObservedProcess(ctx, call)
        }''')
    with cli.open("a") as stream:
        stream.write(f'''
func isVerifiedTerminalExec(call replay.Call) bool {{
    return call.Operation == "Exec" && ({cli_gate}) &&
        call.ExitCode == 0 && call.SessionID == 0
}}
''')
    run = tree/"pkg/replay/harbor/run.go"
    if '"time"' not in run.read_text():
        edit(run, '"strings"', '"strings"\n    "time"')
    gate='if h.FinishObservedProcess != nil && call.SessionID != 0 {'
    edit(run, gate, f'''if h.FinishObservedProcess != nil && ({replay_gate}) &&
            call.SessionID == 0 &&
            call.Operation == "Exec" && call.ExitCode == 0 && out.ExitCode == ProcessRunning {{
            // The original first reply was already logged by this point.
            // Continue the same native Exec and record only its still-unread bytes.
            joinCtx, cancel := context.WithTimeout(ctx, {JOIN_TIMEOUT_S}*time.Second)
            defer cancel()
            terminal, err := h.FinishObservedProcess(joinCtx, call)
            joined := receipt
            joined.Event = "diagnostic_process_observation"
            joined.Time = 0
            joined.StatusBasis = "additional_join_after_source_exec"
            joined.ProcessTiming = terminal.Timing
            joined.Exit = &terminal.ExitCode
            joined.Stdout, joined.Stderr = terminal.Stdout, terminal.Stderr
            joined.StdoutSHA, joined.StderrSHA = digest([]byte(terminal.Stdout)), digest([]byte(terminal.Stderr))
            combined := terminal
            combined.Stdout = out.Stdout + terminal.Stdout
            combined.Stderr = out.Stderr + terminal.Stderr
            joined.OutputComparison = compareOutput(call.ResponseStdout, combined)
            if err := errors.Join(err, emit(h, joined)); err != nil {{
                return err
            }}
            if terminal.ExitCode == 0 {{
                return nil
            }}
            return fmt.Errorf("step %d observed terminal exit %d, recorded 0", receipt.Step, terminal.ExitCode)
        }}
        {gate}''')


def contract(additional=False, through_final=False):
    result = {
        "step": SOURCE_STEP,
        "source_call": SOURCE_CALL,
        "expected_terminal": 0,
        "join_timeout_s": JOIN_TIMEOUT_S,
        "process_tracking_revision": PROCESS_TRACKING_REVISION,
    }
    extra = boundaries(additional, through_final)[1:]
    if extra:
        result["additional"] = [
            {"step": step, "source_call": call, "expected_terminal": 0}
            for step, call in extra
        ]
    return result
