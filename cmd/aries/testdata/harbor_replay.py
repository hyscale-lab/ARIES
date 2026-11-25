"""Write a synthetic Harbor job for the ARIES CLI integration test."""

from datetime import datetime, timedelta, timezone
import json
from pathlib import Path
import shlex
import sys


SOURCE = "terminal-bench/terminal-bench-2-1"
JOB = "synthetic-harbor"
TASK = "terminal-bench/task"
THREAD = "fixture-thread"
EPOCH = datetime(2026, 11, 5, tzinfo=timezone.utc)
CALL_INTERVAL = timedelta(milliseconds=250)
RETURN_DELAY = CALL_INTERVAL / 2
FINAL_WAIT = timedelta(seconds=2)
BARRIER_TIMEOUT_SECONDS = 10
BARRIER_POLL_SECONDS = 0.05
FILE = "/tmp/harbor-replay.txt"
CONTENTS = "fresh\n"


def write_json(path, value):
    path.write_text(json.dumps(value) + "\n", encoding="utf-8")


def trial(root, name, start, grade, expected, *, claude=False):
    directory = root / name
    agent = directory / "agent"
    sessions = agent / "sessions"
    sessions.mkdir(parents=True)
    agent_name = "claude-code" if claude else "codex"
    config = dict(trial_name=name, job_id=JOB, agent={"name": agent_name},
                  task={"name": TASK, "source": SOURCE})
    clock = start
    items = [dict(type="thread.started", thread_id=THREAD), dict(type="turn.started")]
    rollout = []

    def event(kind, at, **payload):
        rollout.append(dict(type=kind, timestamp=at.isoformat(), payload=payload))

    def call(item, wrapper, output, change=None):
        nonlocal clock
        call_id = item["id"]
        items.extend([dict(type="item.started", item=dict(item, status="in_progress")),
                      dict(type="item.completed", item=dict(item, status="completed"))])
        event("response_item", clock, type="custom_tool_call", name="exec", call_id=call_id, input=wrapper)
        if change is not None:
            event("event_msg", clock, type="patch_apply_end", call_id="nested-patch", turn_id="turn",
                  success=True, status="completed", changes=change)
        event("response_item", clock + RETURN_DELAY, type="custom_tool_call_output", call_id=call_id, output=[
            {"type": "input_text", "text": "Script completed\nWall time 0.1 seconds\nOutput:\n"},
            {"type": "input_text", "text": output},
        ])
        clock += CALL_INTERVAL

    def shell(call_id, script, output):
        wrapper = f'const r = await tools.exec_command({{cmd:{json.dumps(script)},workdir:"/"}});\ntext(r.output);\n'
        wrapper += 'if (r.session_id) text((await tools.write_stdin({session_id:r.session_id,chars:""})).output);\n'
        call(dict(id=call_id, type="command_execution", command="/bin/bash -lc " + shlex.quote(script),
                  exit_code=0, aggregated_output=output), wrapper, output)

    event("session_meta", clock, id=THREAD)
    event("event_msg", clock, type="task_started", turn_id="turn")
    if name == "task__first":
        shell("first", "printf first", "first")
        shell("second", "printf second", "change")
    elif name == "task__later":
        body = f"*** Begin Patch\n*** Add File: {FILE}\n+{CONTENTS}*** End Patch"
        call(dict(id="write", type="file_change", changes=[{"path": FILE, "kind": "add"}]),
             f"const patch = {json.dumps(body)};\ntext(await tools.apply_patch(patch));\n", "{}",
             {FILE: {"type": "add", "content": CONTENTS}})
        # Wait for every byte, not only for the file to appear.
        read = ('until cmp -s -- "$1" <(printf "%s" "$2"); do '
                f'sleep {BARRIER_POLL_SECONDS} || exit; done; exec cat -- "$1"')
        shell("read", f"timeout {BARRIER_TIMEOUT_SECONDS}s bash -c {shlex.quote(read)} "
              f"read {shlex.quote(FILE)} {shlex.quote(CONTENTS)}", CONTENTS)
    else:
        shell("size", "printf short", expected)
    returned = clock - CALL_INTERVAL + RETURN_DELAY
    event("event_msg", returned, type="task_complete", turn_id="turn")
    items.append(dict(type="turn.completed"))
    for path, records in ((agent / "codex.txt", items), (sessions / "rollout-fixture.jsonl", rollout)):
        path.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")
    write_json(directory / "config.json", config)
    write_json(directory / "result.json", dict(
        trial_name=name, task_name=TASK, source=SOURCE, task_id={"name": "task"},
        agent_info={"name": agent_name}, config=config,
        agent_execution={"finished_at": (returned + FINAL_WAIT).isoformat()},
        verifier_result={"rewards": grade},
    ))
    if claude:
        (agent / "codex.txt").unlink()
        (sessions / "rollout-fixture.jsonl").unlink()
        raw = sessions / "projects" / "-app" / f"{THREAD}.jsonl"
        raw.parent.mkdir(parents=True)
        ask = dict(role="assistant", content=[
            dict(type="tool_use", id="toolu_fixture", name="Bash", input=dict(command="printf actual; exit 0"))])
        reply = dict(role="user", content=[
            dict(type="tool_result", tool_use_id="toolu_fixture", is_error=True, content="Exit code 1")])
        tool_result = "Error: " + reply["content"][0]["content"]
        for path, records in ((raw, [
            dict(type="assistant", uuid="ask", parentUuid=None, sessionId=THREAD, isSidechain=False,
                 timestamp=start.isoformat(), cwd="/", message=ask),
            dict(type="user", uuid="reply", parentUuid="ask", sessionId=THREAD, isSidechain=False,
                 timestamp=returned.isoformat(), cwd="/", message=reply, toolUseResult=tool_result),
        ]), (agent / "claude-code.txt", [
            dict(type="system", subtype="init", session_id=THREAD),
            dict(type="assistant", uuid="ask", session_id=THREAD, message=ask),
            dict(type="user", uuid="reply", session_id=THREAD, message=reply, tool_use_result=tool_result,
                 timestamp=returned.isoformat()),
            dict(type="result", subtype="success", session_id=THREAD, is_error=False),
        ])):
            path.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")


root = Path(sys.argv[1])
if len(sys.argv) > 2 and sys.argv[2] == "claude":
    trial(root, "task__claude", EPOCH, {"reward": 0}, None, claude=True)
    sys.exit()
trials = (
    ("task__first", EPOCH, {"score": 0, "reward": 1}, None),
    ("task__later", EPOCH + CALL_INTERVAL, {"reward": 0}, None),
    ("task__strict", EPOCH + CALL_INTERVAL + RETURN_DELAY, {}, "longer"),
)
for name, start, grade, expected in trials:
    trial(root, name, start, grade, expected)
write_json(root / "result.json", dict(id=JOB, started_at=EPOCH.isoformat(),
           finished_at=(EPOCH + FINAL_WAIT * 3).isoformat(), n_total_trials=len(trials),
           stats={"n_completed_trials": len(trials)}))
