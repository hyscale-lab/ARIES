"""Add only the retained ordinary Exec boundary proven by the prior prefix."""
from pathlib import Path
import subprocess

SOURCE_STEP = 17
SOURCE_CALL = "call_zkHevzPcbGrer7T32te3CvRp"
JOIN_TIMEOUT_S = 45
PROCESS_TRACKING_REVISION = "6ecdcc0fc73f6a018ab6541ab9c7bbb4bb75515f"


def apply(tree, edit, recording, git_root):
    step, = [row for row in recording["steps"] if row["step_id"] == SOURCE_STEP]
    call, = step["tool_calls"]
    if call["tool_call_id"] != SOURCE_CALL or call["function_name"] != "exec_command":
        raise ValueError("unexpected continuation launch")
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
    return call.Operation == "Exec" && call.ToolCallID == "{SOURCE_CALL}" &&
        call.ExitCode == 0 && call.SessionID == 0
}}
''')
    run = tree/"pkg/replay/harbor/run.go"
    edit(run, '"strings"', '"strings"\n    "time"') if '"time"' not in run.read_text() else None
    gate='if h.FinishObservedProcess != nil && call.SessionID != 0 {'
    edit(run, gate, f'''if h.FinishObservedProcess != nil && receipt.Step == {SOURCE_STEP} &&
            call.ToolCallID == "{SOURCE_CALL}" && call.SessionID == 0 &&
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


def contract():
    return {
        "step": SOURCE_STEP,
        "source_call": SOURCE_CALL,
        "expected_terminal": 0,
        "join_timeout_s": JOIN_TIMEOUT_S,
        "process_tracking_revision": PROCESS_TRACKING_REVISION,
    }
