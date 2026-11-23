"""Constrain the existing native process-retention path to one source boundary."""


TRIAL = "93c2fb63-9bd5-4e9d-9024-06fbbc0c1087"
SESSION = 6932
START = "call_nPaabjFZ3lRwBQbPqTwJ7KPW"
POLL = "call_7OBq8rp1lKmAC4atWrs1JqWd"
TERMINAL_STDERR_SHA = "ab15469aa38657fdbd724592977da03cb54417b5341b745f8ee6f364e816cf22"


def apply(tree, edit, recording):
    calls = {}
    for step in recording["steps"]:
        if step["step_id"] in (12, 13):
            call, = step["tool_calls"]
            calls[step["step_id"]] = call
    if calls[12]["tool_call_id"] != START or calls[13]["tool_call_id"] != POLL:
        raise ValueError("unexpected original source boundary")
    if calls[13]["arguments"]["session_id"] != SESSION:
        raise ValueError("unexpected original source handle")
    prepare = tree/"cmd/aries-harbor/main.go"
    needle = 'allowed := grpcPoll ||'
    edit(prepare, needle, f'proteinBoundary := r.Task == "protein-assembly" && r.Trial == "{TRIAL}"\n\t\tallowed := proteinBoundary || grpcPoll ||')
    needle = 'if !allowed || (!financialPoll &&'
    edit(prepare, needle, 'if !allowed || (!financialPoll && !proteinBoundary &&')
    needle = 'return sessions.ObserveDrift(ctx, call)'
    edit(prepare, needle, f'if r.Trial == "{TRIAL}" && call.SessionID != {SESSION} {{\n'
                         '\t\t\t\t\t\treturn sessions.Execute(ctx, call)\n'
                         '\t\t\t\t\t}\n\t\t\t\t\t'+needle)
    entry = 'if h.FinishObservedProcess != nil && call.SessionID != 0 {'
    edit(tree/"pkg/replay/harbor/run.go", entry, entry+f'''
            if call.ToolCallID != "{START}" || call.SessionID != {SESSION} ||
                call.Operation != "StartProcess" || call.ExitCode != ProcessRunning ||
                out.ExitCode != 1 || receipt.StderrSHA != "{TERMINAL_STDERR_SHA}" {{
                return fmt.Errorf("step %d is outside the verified retained-terminal diagnostic", receipt.Step)
            }}
''')


def contract():
    return dict(start_step=12, first_source_call=START, poll_step=13, poll_source_call=POLL,
                session_id=SESSION, early_exit=1, archived_terminal_stderr_sha256=TERMINAL_STDERR_SHA)
