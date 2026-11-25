import hashlib
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

CONVERTER = Path(__file__).with_name("convert_harbor.py")
TRIAL = "task__example"
THREAD = "thread"
RUN = "source-run"
ALTERNATE_AGENT = "synthetic-agent"
CALL = "call-shell"
START = "2026-11-05T12:00:01.123456789Z"
RETURN = "2026-11-05T12:00:02.123456789Z"
END = "2026-11-05T12:00:04Z"
SCRIPT = "  printf '%s\\n' \"$(printf é)\" \n"
INNER_LOGIN_SCRIPT = "  /bin/bash -l -c 'printf ready'\n"
OUTPUT = "  é\r\n\n"
WORKDIR = "/app"
DIGEST = "sha256:" + hashlib.sha256(b"package").hexdigest()
HEADER = "Script completed\nWall time 1.0 seconds\nOutput:\n"
EXIT = 7
CONFIGURED_TIMEOUT_MS = 600_000
PATCH_PATH = "/app/example.txt"
PATCH_BODY = "héllo  \n\n"
PATCH_CALL = "call-patch"
PATCH_START = "2026-11-05T12:00:03Z"
SHELL_WRAPPER = (
    f'const r = await tools.exec_command({{cmd:{json.dumps(SCRIPT)},'
    f'"workdir":{json.dumps(WORKDIR)}}});\ntext(r.output);\n'
)
WAIT = (
    'if (r.session_id) text((await tools.write_stdin({session_id:r.session_id,'
    'chars:"",yield_time_ms:30000,max_output_tokens:12000})).output);\n'
)


def write_json(path, value):
    path.write_text(json.dumps(value) + "\n", encoding="utf-8")


def write_lines(path, records):
    path.write_text("".join(json.dumps(record) + "\n" for record in records), encoding="utf-8")


def read_lines(path):
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]


def write_raw_bash(trial, inputs, *, call_id=CALL, thread=THREAD, start=START, returned=RETURN, stdout=None):
    session = trial / "agent" / "sessions" / "projects" / "-app" / f"{thread}.jsonl"
    session.parent.mkdir(parents=True, exist_ok=True)
    write_lines(session, [
        {"type": "assistant", "sessionId": thread, "timestamp": start, "cwd": WORKDIR,
         "uuid": f"{call_id}-request", "parentUuid": None, "isSidechain": False,
         "message": {"role": "assistant", "content": [
             {"type": "tool_use", "id": call_id, "name": "Bash", "input": inputs},
         ]}},
        {"type": "user", "sessionId": thread, "timestamp": returned, "cwd": WORKDIR,
         "uuid": f"{call_id}-return", "parentUuid": f"{call_id}-request", "isSidechain": False,
         "message": {"role": "user", "content": [
             {"type": "tool_result", "tool_use_id": call_id, "is_error": stdout is None,
              "content": f"Exit code {EXIT}" if stdout is None else stdout},
          ]}, "toolUseResult": f"Error: Exit code {EXIT}" if stdout is None else {
             "stdout": stdout, "stderr": "", "interrupted": False, "isImage": False, "noOutputExpected": False,
         }},
    ])
    return session


def write_claude_transcript(path, raw):
    entries = [{"type": "system", "subtype": "init", "session_id": THREAD}]
    for event in raw:
        entry = {key: event[key] for key in ("type", "uuid", "message")}
        entry["session_id"] = THREAD
        if "toolUseResult" in event:
            entry.update(tool_use_result=event["toolUseResult"], timestamp=event["timestamp"])
        entries.append(entry)
    entries.append({"type": "result", "subtype": "success", "session_id": THREAD, "is_error": False})
    write_lines(path, entries)
    return entries


def rollout_path(trial):
    return next((trial / "agent" / "sessions").rglob("rollout-*.jsonl"))


def add_shell(trial, *, overlap, same=False):
    transcript = trial / "agent" / "codex.txt"
    items = read_lines(transcript)
    path = rollout_path(trial)
    entries = read_lines(path)
    script = SCRIPT if same else "printf second"
    output = OUTPUT if same else "second"
    call_id = "call-second"
    new_item = dict(items[2]["item"], id="item_2", command="/bin/bash -lc " + shlex.quote(script))
    start_item = {"type": "item.started", "item": new_item}
    end_item = {"type": "item.completed", "item": dict(new_item, status="failed", exit_code=EXIT, aggregated_output=output)}
    start = "2026-11-05T12:00:01.5Z" if overlap else PATCH_START
    end = "2026-11-05T12:00:01.75Z" if overlap else PATCH_START
    request = event("response_item", timestamp=start, type="custom_tool_call", name="exec", call_id=call_id,
                    input=SHELL_WRAPPER.replace(json.dumps(SCRIPT), json.dumps(script)))
    response = event("response_item", timestamp=end, type="custom_tool_call_output", call_id=call_id,
                     output=[{"type": "input_text", "text": HEADER}, {"type": "input_text", "text": output}])
    if overlap:
        items[-2:-2] = [start_item, end_item]
        entries[-2:-2] = [request]
        entries.insert(-2, response)
        # The transcript and rollout need not put the completions in the same order.
        items[-3], items[-2] = items[-2], items[-3]
    else:
        items.extend([{"type": "turn.started"}, start_item, end_item, {"type": "turn.completed"}])
        entries.extend([event("event_msg", timestamp=start, type="task_started", turn_id="turn-2"),
                        request, response, event("event_msg", timestamp=end, type="task_complete", turn_id="turn-2")])
    write_lines(transcript, items)
    write_lines(path, entries)
    return call_id, start, end


def event(kind, **payload):
    return {"type": kind, "timestamp": payload.pop("timestamp", START), "payload": payload}


def write_trial(parent, *, name=TRIAL, run=RUN, agent_name="codex", wrapper=SHELL_WRAPPER + WAIT,
                with_trajectory=False, with_patch=False):
    trial = parent / name
    agent = trial / "agent"
    sessions = agent / "sessions" / "2026" / "11" / "05"
    sessions.mkdir(parents=True)
    config = {
        "trial_name": name, "job_id": run, "agent": {"name": agent_name},
        "task": {"name": "terminal-bench/task", "source": "terminal-bench/terminal-bench-2-1",
                 "ref": DIGEST},
    }
    write_json(trial / "config.json", config)
    write_json(trial / "result.json", {
        "trial_name": name, "task_name": "terminal-bench/task", "source": config["task"]["source"],
        "task_id": {"org": "terminal-bench", "name": "task", "ref": None},
        "task_checksum": DIGEST.removeprefix("sha256:"), "agent_info": {"name": agent_name},
        "config": config, "agent_execution": {"finished_at": END},
        "verifier_result": {"rewards": {"reward": 0}},
    })
    shell = {"id": "item_1", "type": "command_execution", "command": "/bin/bash -lc " + shlex.quote(SCRIPT)}
    transcript = [
        {"type": "thread.started", "thread_id": THREAD},
        {"type": "turn.started"},
        {"type": "item.started", "item": dict(shell, status="in_progress", exit_code=None, aggregated_output="")},
        {"type": "item.completed", "item": dict(shell, status="failed", exit_code=EXIT, aggregated_output=OUTPUT)},
        {"type": "turn.completed"},
    ]
    rollout = [
        event("session_meta", id=THREAD),
        event("event_msg", type="task_started", turn_id="turn"),
        event("response_item", type="custom_tool_call", name="exec", call_id=CALL, input=wrapper),
        event("response_item", timestamp=RETURN, type="custom_tool_call_output", call_id=CALL,
              output=[{"type": "input_text", "text": HEADER}, {"type": "input_text", "text": OUTPUT}]),
        event("event_msg", timestamp=RETURN, type="task_complete", turn_id="turn"),
    ]
    requests = [(CALL, wrapper)]
    if with_patch:
        body = "".join("+" + line for line in PATCH_BODY.splitlines(keepends=True))
        patch = f"*** Begin Patch\n*** Add File: {PATCH_PATH}\n{body}*** End Patch"
        patch_wrapper = f"const patch = {json.dumps(patch)};\ntext(await tools.apply_patch(patch));\n"
        patch_item = {"id": "item_2", "type": "file_change", "changes": [{"path": PATCH_PATH, "kind": "add"}]}
        transcript[-1:-1] = [
            {"type": "item.started", "item": dict(patch_item, status="in_progress")},
            {"type": "item.completed", "item": dict(patch_item, status="completed")},
        ]
        rollout[-1:-1] = [
            event("response_item", timestamp=PATCH_START, type="custom_tool_call", name="exec",
                  call_id=PATCH_CALL, input=patch_wrapper),
            event("event_msg", timestamp=PATCH_START, type="patch_apply_end", call_id="nested-patch", turn_id="turn",
                  success=True, status="completed", changes={PATCH_PATH: {"type": "add", "content": PATCH_BODY}}),
            event("response_item", timestamp=PATCH_START, type="custom_tool_call_output", call_id=PATCH_CALL,
                  output=[{"type": "input_text", "text": HEADER}, {"type": "input_text", "text": "{}"}]),
        ]
        rollout[-1]["timestamp"] = PATCH_START
        requests.append((PATCH_CALL, patch_wrapper))
    write_lines(agent / "codex.txt", transcript)
    write_lines(sessions / "rollout-example.jsonl", rollout)
    if with_trajectory:
        write_json(agent / "trajectory.json", {
            "schema_version": "ATIF-v1.7", "session_id": THREAD, "steps": [{
                "tool_calls": [{"tool_call_id": call_id, "function_name": "exec", "arguments": {"input": request}}],
                "observation": {"results": [{"source_call_id": call_id}]},
            } for call_id, request in requests],
        })
    return trial


def write_script_only_trial(parent, *, login=None, with_raw=True):
    wrapper = SHELL_WRAPPER.replace(json.dumps(SCRIPT), json.dumps(INNER_LOGIN_SCRIPT))
    if login is not None:
        wrapper = wrapper.replace("});", f',"login":{json.dumps(login)}}});', 1)
    trial = write_trial(parent, agent_name=ALTERNATE_AGENT, wrapper=wrapper)
    transcript = trial / "agent" / "codex.txt"
    items = read_lines(transcript)
    for entry in items:
        if "item" in entry:
            entry["item"]["command"] = INNER_LOGIN_SCRIPT
        if entry["type"] == "item.completed":
            entry["item"].update(exit_code=0, status="completed")
    write_lines(transcript, items)
    if with_raw:
        write_raw_bash(trial, {"command": INNER_LOGIN_SCRIPT}, stdout=OUTPUT)
    return trial


def set_timing(trial, start, returned, finished):
    path = rollout_path(trial)
    records = read_lines(path)
    times = dict(zip(sorted({record["timestamp"] for record in records}), (start, returned), strict=True))
    for record in records:
        record["timestamp"] = times[record["timestamp"]]
    write_lines(path, records)
    path = trial / "result.json"
    result = json.loads(path.read_bytes())
    result["agent_execution"]["finished_at"] = finished
    write_json(path, result)


def write_run(root, *, agent_name="codex"):
    names = ("task__z-early", "task__b-empty", "task__m-later", "task__a-empty", "task__y-early")
    for name in names:
        trial = write_trial(root, name=name, agent_name=agent_name)
        if name.endswith("-empty"):
            path = trial / "agent" / "codex.txt"
            write_lines(path, [event for event in read_lines(path) if not event["type"].startswith("item.")])
            path = rollout_path(trial)
            write_lines(path, [event for event in read_lines(path) if event["type"] != "response_item"])
        if name.endswith("-later"):
            set_timing(trial, "2026-11-05T12:00:31.123456789Z", "2026-11-05T12:00:32.123456789Z",
                       "2026-11-05T12:00:37.123456789Z")
            path = trial / "result.json"
            result = json.loads(path.read_bytes())
            result["verifier_result"]["rewards"] = {"score": 0.5}
            write_json(path, result)
    write_json(root / "config.json", {"job_name": root.name, "n_attempts": len(names), "agents": [{"name": agent_name}]})
    write_json(root / "result.json", {"id": RUN, "started_at": "2026-11-05T12:00:00", "finished_at": "2026-11-05T12:00:40",
                                     "n_total_trials": len(names), "stats": {"n_completed_trials": len(names)}})
    (root / "job.log").write_text("Synthetic Harbor job\n", encoding="utf-8")


class ConvertHarborTest(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.parent = Path(directory.name)
        self.output = self.parent / "source"

    def convert(self, trial):
        return subprocess.run(
            [sys.executable, str(CONVERTER), "--harbor-trial", str(trial), "--output-dir", str(self.output)],
            capture_output=True, text=True, check=False,
        )

    def convert_run(self, root):
        return subprocess.run(
            [sys.executable, str(CONVERTER), "--harbor-run", str(root), "--output-dir", str(self.output)],
            capture_output=True, text=True, check=False,
        )

    def test_supported_evidence_converts_with_a_different_agent_name(self):
        root = self.parent / "harbor-job"
        write_run(root, agent_name=ALTERNATE_AGENT)
        occurrence = "task__m-later"
        for mode, convert, path in (("trial", self.convert, root / occurrence), ("run", self.convert_run, root)):
            with self.subTest(mode=mode):
                self.output = self.parent / mode
                result = convert(path)
                self.assertEqual(result.returncode, 0, result.stderr)
                call, = read_lines(self.output / occurrence / "bridge" / "tool-calls.jsonl")
                self.assertEqual(call["tool_call_id"], CALL)

    def test_missing_rollout_reports_the_required_artifact_without_publishing(self):
        root = self.parent / "harbor-job"
        write_run(root, agent_name=ALTERNATE_AGENT)
        occurrence = "task__m-later"
        trial = root / occurrence
        rollout_path(trial).unlink()
        for mode, convert, path in (("trial", self.convert, trial), ("run", self.convert_run, root)):
            with self.subTest(mode=mode):
                result = convert(path)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(occurrence, result.stderr)
                self.assertIn("agent/sessions/**/rollout-*.jsonl: missing rollout", result.stderr)
                self.assertFalse(self.output.exists())
                self.assertEqual(list(self.parent.glob(".harbor-*")), [])

    def test_agent_identity_mismatch_is_not_eligible(self):
        trial = write_trial(self.parent, agent_name=ALTERNATE_AGENT)
        path = trial / "result.json"
        result = json.loads(path.read_bytes())
        result["agent_info"]["name"] = "different-agent"
        write_json(path, result)
        converted = self.convert(trial)
        self.assertNotEqual(converted.returncode, 0)
        self.assertIn("trial identity differs between config and result", converted.stderr)
        self.assertFalse(self.output.exists())

    def test_raw_bash_launcher_defaults_to_non_login(self):
        from convert_harbor import raw_bash_argv

        argv = raw_bash_argv({"command": INNER_LOGIN_SCRIPT})
        self.assertEqual(argv, ["/bin/bash", "-c", INNER_LOGIN_SCRIPT])

    def test_script_only_transcript_honors_rollout_login(self):
        trial = write_script_only_trial(self.parent, login=True)
        session = write_raw_bash(trial, {"command": INNER_LOGIN_SCRIPT, "timeout": CONFIGURED_TIMEOUT_MS,
                                         "dangerouslyDisableSandbox": False}, stdout=OUTPUT)
        raw = read_lines(session)
        raw[-1]["toolUseResult"]["dangerouslyDisableSandbox"] = False
        write_lines(session, raw)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, _, (call,) = self.read_source()
        self.assertEqual(call["argv"], ["/bin/bash", "-l", "-c", INNER_LOGIN_SCRIPT])
        self.assertNotIn("dangerouslyDisableSandbox", call)
        raw[-1]["toolUseResult"]["dangerouslyDisableSandbox"] = True
        write_lines(session, raw)
        self.output = self.parent / "conflict"
        self.assert_rejected(trial, f"call {CALL}: Bash result dangerouslyDisableSandbox differs from request")

    def test_script_only_transcript_requires_matching_raw_request(self):
        trial = write_script_only_trial(self.parent, with_raw=False)
        self.assert_rejected(trial, CALL, "missing corroborating raw Bash request")
        write_raw_bash(trial, {"command": SCRIPT}, stdout=OUTPUT)
        self.assert_rejected(trial, CALL, "script, workdir or request time differs from rollout")

    def test_script_only_transcript_does_not_hide_outer_launcher_evidence(self):
        trial = write_script_only_trial(self.parent, login=False)
        path = trial / "agent" / "codex.txt"
        items = read_lines(path)
        for launcher, reason in (("/bin/bash -lc", "login"), ("/bin/bash --noprofile -c", "unsupported transcript")):
            with self.subTest(launcher=launcher):
                for entry in items:
                    if "item" in entry:
                        entry["item"]["command"] = launcher + " " + shlex.quote(INNER_LOGIN_SCRIPT)
                write_lines(path, items)
                self.assert_rejected(trial, reason)

    def test_sequential_script_only_calls_can_share_a_launcher_interpretation(self):
        first_script = "printf ready"
        second_script = "/bin/bash -l -c 'printf ready'"
        output = "ready"
        trial = write_trial(self.parent)
        second_id, second_start, second_end = add_shell(trial, overlap=False)
        transcript = trial / "agent" / "codex.txt"
        items = [item for item in read_lines(transcript) if not item["type"].startswith("turn.")]
        for entry in items:
            if "item" not in entry:
                continue
            item = entry["item"]
            item["command"] = first_script if item["id"] == "item_1" else second_script
            if entry["type"] == "item.completed":
                item.update(exit_code=0, status="completed", aggregated_output=output)
        write_lines(transcript, [items[0], {"type": "turn.started"}, *items[1:], {"type": "turn.completed"}])

        rollout = rollout_path(trial)
        entries = [entry for entry in read_lines(rollout)
                   if entry["payload"].get("type") not in {"task_started", "task_complete"}]
        for entry in entries[1:]:
            payload = entry["payload"]
            if "input" in payload:
                script = first_script if payload["call_id"] == CALL else second_script
                payload["input"] = SHELL_WRAPPER.replace(json.dumps(SCRIPT), json.dumps(script))
            else:
                payload["output"][-1]["text"] = output
        write_lines(rollout, [entries[0], event("event_msg", type="task_started", turn_id="turn"),
                              *entries[1:], event("event_msg", timestamp=second_end, type="task_complete", turn_id="turn")])

        session = write_raw_bash(trial, {"command": first_script}, stdout=output)
        raw = read_lines(session)
        write_raw_bash(trial, {"command": second_script}, call_id=second_id,
                       start=second_start, returned=second_end, stdout=output)
        write_lines(session, raw + read_lines(session))
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, _, calls = self.read_source()
        self.assertEqual([(call["tool_call_id"], call["argv"], call["harbor_output"]) for call in calls], [
            (CALL, ["/bin/bash", "-c", first_script], output),
            (second_id, ["/bin/bash", "-c", second_script], output),
        ])

    def test_overlapping_nested_launcher_matches_canonical_argv(self):
        script = "printf ready"
        launch = "/bin/bash -lc " + shlex.quote(script)
        output = "ready"
        trial = write_trial(self.parent)
        second_id, second_start, second_end = add_shell(trial, overlap=True)
        transcript = trial / "agent" / "codex.txt"
        items = read_lines(transcript)
        for entry in items:
            if "item" in entry:
                item = entry["item"]
                item["command"] = launch if item["id"] == "item_1" else "/bin/bash -c " + shlex.quote(launch)
                if entry["type"] == "item.completed":
                    item.update(aggregated_output=output, exit_code=0, status="completed")
        write_lines(transcript, items)
        rollout = rollout_path(trial)
        entries = read_lines(rollout)
        for entry in entries:
            payload = entry["payload"]
            if "input" in payload:
                command = script if payload["call_id"] == CALL else launch
                payload["input"] = SHELL_WRAPPER.replace(json.dumps(SCRIPT), json.dumps(command))
            elif "output" in payload:
                payload["output"][-1]["text"] = output
        write_lines(rollout, entries)
        for variant in ("codex-only", "mixed", "contradictory-result"):
            with self.subTest(variant=variant):
                self.output = self.parent / variant
                if variant != "codex-only":
                    session = write_raw_bash(trial, {"command": script}, stdout=output)
                    first_request, first_return = read_lines(session)
                    write_raw_bash(trial, {"command": launch}, call_id=second_id, start=second_start,
                                   returned=second_end, stdout=output if variant == "mixed" else "wrong")
                    second_request, second_return = read_lines(session)
                    write_lines(session, [first_request, second_request, second_return, first_return])
                if variant == "contradictory-result":
                    self.assert_rejected(trial, second_id, "output or result status differs from rollout")
                    continue
                result = self.convert(trial)
                self.assertEqual(result.returncode, 0, result.stderr)
                _, _, calls = self.read_source()
                self.assertEqual({call["tool_call_id"]: call["argv"] for call in calls}, {
                    CALL: ["/bin/bash", "-l", "-c", script],
                    second_id: ["/bin/bash", "-c", launch],
                })

    def test_completed_claude_trace_keeps_missing_process_output(self):
        root = self.parent / "harbor-job"
        write_run(root, agent_name=ALTERNATE_AGENT)
        occurrence = "task__m-later"
        trial = root / occurrence
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        call_id = "toolu_example"
        session = write_raw_bash(trial, {"command": SCRIPT, "description": "Run example"}, call_id=call_id)
        request, returned = read_lines(session)
        request.update(uuid="request", parentUuid=None, isSidechain=False)
        returned.update(uuid="return", parentUuid="request", isSidechain=False)
        write_lines(session, [request, returned])
        transcript = agent / "claude-code.txt"
        entries = [
            {"type": "system", "subtype": "init", "session_id": THREAD},
            {"type": "assistant", "session_id": THREAD, "uuid": "request", "message": request["message"]},
            {"type": "user", "session_id": THREAD, "uuid": "return", "message": returned["message"],
             "tool_use_result": returned["toolUseResult"], "timestamp": RETURN},
            {"type": "result", "subtype": "success", "session_id": THREAD, "is_error": False},
        ]
        write_lines(transcript, entries)
        for mode, convert, path in (("trial", self.convert, trial), ("run", self.convert_run, root)):
            with self.subTest(mode=mode):
                self.output = self.parent / f"source-{mode}"
                result = convert(path)
                self.assertEqual(result.returncode, 0, result.stderr)
                call, = read_lines(self.output / occurrence / "bridge" / "tool-calls.jsonl")
                self.assertEqual(call["tool_call_id"], call_id)
                self.assertEqual(call["argv"], ["/bin/bash", "-c", SCRIPT])
                self.assertEqual(call["workdir"], WORKDIR)
                self.assertEqual(call["started_at"], START)
                self.assertEqual(call["timestamp"], RETURN)
                self.assertEqual(call["exit_code"], EXIT)
                self.assertNotIn("harbor_output", call)
                self.assertNotIn("response_sha256", call)
                self.assertNotIn("status", call)
                self.assertIs(call["harbor_tool_error"], True)
                self.assertEqual(list(self.parent.glob(".harbor-*")), [])

        for name, trace, raw, reason in (
            ("unclosed", entries[:-1], [request, returned], "missing successful Claude completion"),
            ("unreturned", entries, [request], "missing tool result"),
            ("unbound", [entries[0], entries[1], dict(entries[2], uuid="other"), entries[3]],
             [request, returned], "differs from Claude transcript"),
        ):
            with self.subTest(name=name):
                self.output = self.parent / f"source-{name}"
                write_lines(transcript, trace)
                write_lines(session, raw)
                result = self.convert(trial)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(reason, result.stderr)
                self.assertFalse(self.output.exists())

        returned["message"]["content"][0].update(is_error=False, content="(Bash completed with no output)")
        returned["toolUseResult"] = dict(stdout="", stderr="", interrupted=False, isImage=False, noOutputExpected=False)
        entries[2]["tool_use_result"] = returned["toolUseResult"]
        write_lines(transcript, entries)
        write_lines(session, [request, returned])
        self.output = self.parent / "source-empty-output"
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        call, = read_lines(self.output / occurrence / "bridge" / "tool-calls.jsonl")
        self.assertEqual(call["harbor_output"], "")
        self.assertIs(call["harbor_tool_error"], False)
        self.assertNotIn("exit_code", call)

    def test_claude_bash_exit_diagnostic_is_not_process_output(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT})
        raw = read_lines(session)
        result = raw[-1]
        block = result["message"]["content"][0]
        marker = f"Exit code {EXIT}"
        display = marker + "\nCould not open example.\nCheck the input."
        for name, content, wrapper, is_error, valid in (
            ("diagnostic", display, "Error: " + display, True, True),
            ("bare", marker, "Error: " + marker, True, True),
            ("wrapper-conflict", display, "Error: " + marker, True, False),
            ("missing-flag", display, "Error: " + display, None, False),
            ("success-flag", display, "Error: " + display, False, False),
            ("not-leading", "\n" + display, "Error: \n" + display, True, False),
            ("not-a-line", marker + " extra", "Error: " + marker + " extra", True, False),
            ("zero", "Exit code 0\nDiagnostic", "Error: Exit code 0\nDiagnostic", True, False),
            ("outside-range", "Exit code 256\nDiagnostic", "Error: Exit code 256\nDiagnostic", True, False),
        ):
            with self.subTest(name=name):
                self.output = self.parent / name
                block.update(content=content, is_error=is_error)
                result["toolUseResult"] = wrapper
                write_lines(session, raw)
                write_claude_transcript(agent / "claude-code.txt", raw)
                if not valid:
                    self.assert_rejected(trial, CALL, "Bash")
                    continue
                converted = self.convert(trial)
                self.assertEqual(converted.returncode, 0, converted.stderr)
                _, _, (call,) = self.read_source()
                self.assertEqual(call["exit_code"], EXIT)
                self.assertIs(call["harbor_tool_error"], True)
                for key in ("harbor_output", "stdout", "stderr", "response_sha256"):
                    self.assertNotIn(key, call)

    def test_claude_rejection_names_the_unsupported_tool(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT})
        raw = read_lines(session)
        raw[0]["message"]["content"][0].update(
            name="Edit", input={"file_path": PATCH_PATH, "old_string": "old", "new_string": "new"})
        write_lines(session, raw)
        write_claude_transcript(agent / "claude-code.txt", raw)
        self.assert_rejected(trial, f"call {CALL} Edit:")

    def test_claude_preserves_only_confirmed_unread_file_rejection(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT})
        request, result = read_lines(session)
        args = {"file_path": PATCH_PATH, "old_string": "fixture before", "new_string": "fixture after",
                "replace_all": False}
        request["message"]["content"][0].update(name="Edit", input=args)
        block = result["message"]["content"][0]
        reason = "File has not been read yet. Read it first before writing to it."
        display = f"<tool_use_error>{reason}</tool_use_error>"
        wrapper = "Error: " + reason
        for name, content, raw_wrapper, is_error in (
            ("confirmed", display, wrapper, True),
            ("successful", "File was updated", {"filePath": PATCH_PATH}, False),
            ("generic", "<tool_use_error>Cannot edit</tool_use_error>", "Error: Cannot edit", True),
            ("contradictory-wrapper", display, "Error: Cannot edit", True),
            ("missing-wrapper", display, wrapper, True),
            ("missing-return-time", display, wrapper, True),
            ("other-parent", display, wrapper, True),
            ("reversed-clock", display, wrapper, True),
            ("trajectory-conflict", display, wrapper, True),
            ("overlap", display, wrapper, True),
        ):
            with self.subTest(name=name):
                self.output = self.parent / name
                block.update(content=content, is_error=is_error)
                result.update(toolUseResult=raw_wrapper,
                              parentUuid=request["uuid"] if name != "other-parent" else "other")
                result["timestamp"] = "2026-11-05T12:00:00Z" if name == "reversed-clock" else RETURN
                events = [request, result]
                calls = [dict(tool_call_id=CALL, function_name="Edit", arguments=args)]
                results = [dict(source_call_id=CALL, content=content,
                                extra=dict(tool_result_is_error=is_error and name != "trajectory-conflict"))]
                if name == "overlap":
                    bash_id = "call-overlap"
                    bash_args = {"command": "printf fixture"}
                    bash_session = write_raw_bash(trial, bash_args, call_id=bash_id, start="2026-11-05T12:00:01.5Z",
                                                  returned=PATCH_START, stdout="fixture")
                    bash_request, bash_result = read_lines(bash_session)
                    events = [request, bash_request, result, bash_result]
                    calls.append(dict(tool_call_id=bash_id, function_name="Bash", arguments=bash_args))
                    results.append(dict(source_call_id=bash_id))
                write_lines(session, events)
                transcript = agent / "claude-code.txt"
                trace = write_claude_transcript(transcript, events)
                if name == "missing-wrapper":
                    del trace[-2]["tool_use_result"]
                    write_lines(transcript, trace)
                if name == "missing-return-time":
                    del trace[-2]["timestamp"]
                    write_lines(transcript, trace)
                write_json(agent / "trajectory.json", dict(schema_version="ATIF-v1.7", session_id=THREAD,
                    steps=[dict(tool_calls=calls, observation=dict(results=results))]))
                if name not in {"confirmed", "overlap"}:
                    self.assert_rejected(trial, CALL)
                    continue
                converted = self.convert(trial)
                self.assertEqual(converted.returncode, 0, converted.stderr)
                _, _, records = self.read_source()
                call = records[0]
                if name == "overlap":
                    self.assertEqual([record["tool_call_id"] for record in records], [CALL, bash_id])
                    self.assertEqual(records[-1]["started_at"], bash_request["timestamp"])
                else:
                    self.assertEqual(len(records), 1)
                self.assertEqual(call.pop("operation_class"), "source_rejection")
                self.assertEqual(call.pop("operation"), "Edit")
                self.assertEqual(call.pop("rejection_reason"), "file_not_read")
                self.assertEqual(call.pop("tool_call_id"), CALL)
                self.assertEqual(call.pop("started_at"), START)
                self.assertEqual(call.pop("timestamp"), RETURN)
                self.assertEqual(set(call), {"sequence", "run_id", "task_id", "sandbox_id"})

    def test_claude_bash_options_do_not_change_the_replay_call(self):
        root = self.parent / "harbor-job"
        write_run(root, agent_name=ALTERNATE_AGENT)
        occurrence = "task__m-later"
        trial = root / occurrence
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        beyond_uint64_ms = 100_000_000_000_000_000_000
        expected = {}
        for label, options in (
            ("omitted", {}),
            ("enabled", {"dangerouslyDisableSandbox": True, "timeout": CONFIGURED_TIMEOUT_MS}),
            ("disabled", {"dangerouslyDisableSandbox": False, "timeout": beyond_uint64_ms}),
        ):
            session = write_raw_bash(trial, {"command": SCRIPT, **options}, stdout=OUTPUT)
            raw = read_lines(session)
            if "dangerouslyDisableSandbox" in options:
                raw[-1]["toolUseResult"]["dangerouslyDisableSandbox"] = options["dangerouslyDisableSandbox"]
            write_lines(session, raw)
            write_claude_transcript(agent / "claude-code.txt", raw)
            for mode, convert, source in (("trial", self.convert, trial), ("run", self.convert_run, root)):
                with self.subTest(options=label, mode=mode):
                    self.output = self.parent / f"source-{mode}-{label}"
                    result = convert(source)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    log = (self.output / occurrence / "bridge" / "tool-calls.jsonl").read_bytes()
                    if label == "omitted":
                        expected[mode] = log
                    else:
                        self.assertEqual(log, expected[mode])

    def test_claude_bash_result_echo_must_match_request_and_transcript(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        option = "dangerouslyDisableSandbox"
        session = write_raw_bash(trial, {"command": SCRIPT, option: True}, stdout=OUTPUT)
        raw = read_lines(session)
        transcript = agent / "claude-code.txt"
        for label, raw_echo, transcript_echo, reason in (
            ("raw-type", int(True), True, "invalid Bash result dangerouslyDisableSandbox"),
            ("raw-conflict", False, False, "Bash result dangerouslyDisableSandbox differs from request"),
            ("transcript-type", True, int(True), "raw tool event differs from Claude transcript"),
            ("transcript-conflict", True, False, "raw tool event differs from Claude transcript"),
        ):
            with self.subTest(label=label):
                raw[-1]["toolUseResult"][option] = raw_echo
                write_lines(session, raw)
                entries = write_claude_transcript(transcript, raw)
                entries[-2]["tool_use_result"][option] = transcript_echo
                write_lines(transcript, entries)
                self.assert_rejected(trial, reason)

    def test_claude_bash_options_must_match_corroborating_requests(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        inputs = {"command": SCRIPT, "dangerouslyDisableSandbox": True, "timeout": CONFIGURED_TIMEOUT_MS}
        session = write_raw_bash(trial, inputs)
        raw = read_lines(session)
        transcript = agent / "claude-code.txt"
        entries = write_claude_transcript(transcript, raw)
        entries[1]["message"]["content"][0]["input"]["dangerouslyDisableSandbox"] = False
        write_lines(transcript, entries)
        self.assert_rejected(trial, "raw tool event differs from Claude transcript")
        write_claude_transcript(transcript, read_lines(session))

        trajectory = agent / "trajectory.json"
        for label, arguments in (
            ("integer-boolean", dict(inputs, dangerouslyDisableSandbox=int(True))),
            ("float-timeout", dict(inputs, timeout=float(CONFIGURED_TIMEOUT_MS))),
            ("different-boolean", dict(inputs, dangerouslyDisableSandbox=False)),
            ("omitted", {"command": SCRIPT}),
            ("matched", inputs),
        ):
            with self.subTest(label=label):
                self.output = self.parent / label
                write_json(trajectory, {
                    "schema_version": "ATIF-v1.7", "session_id": THREAD, "steps": [{
                        "tool_calls": [{"tool_call_id": CALL, "function_name": "Bash", "arguments": arguments}],
                        "observation": {"results": [{"source_call_id": CALL}]},
                    }],
                })
                if label == "matched":
                    result = self.convert(trial)
                    self.assertEqual(result.returncode, 0, result.stderr)
                else:
                    self.assert_rejected(trial, f"call {CALL}: trajectory request differs")

    def test_claude_conversion_preserves_causal_order(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT}, stdout=OUTPUT)
        ask, reply = read_lines(session)
        write_raw_bash(trial, {"command": "printf next"}, call_id="call-next", start=PATCH_START, returned=END, stdout="")
        next_ask, next_reply = read_lines(session)
        transcript = agent / "claude-code.txt"
        overlap_start = "2026-11-05T12:00:01.5Z"
        for name, raw, order, reason in (
            ("reversed-clock", [next_ask, next_reply, ask, reply], None, "raw Claude tool timestamps"),
            ("sequential-with-overlapping-clock", [ask, reply, dict(next_ask, timestamp=overlap_start), next_reply],
             None, "raw Claude tool timestamps"),
            ("sequential-with-reversed-overlapping-clock",
             [dict(ask, timestamp=overlap_start), reply, dict(next_ask, timestamp=START), next_reply],
             None, "raw Claude tool timestamps"),
            ("return-before-request", [ask, reply], [1, 0], "Claude transcript returns before"),
            ("reversed-precedence", [ask, reply, next_ask, next_reply], [2, 3, 0, 1], "Claude transcript contradicts"),
            ("overlap", [ask, dict(next_ask, timestamp=START), reply, next_reply], [1, 0, 3, 2], ""),
            ("distinct-overlap", [ask, dict(next_ask, timestamp=overlap_start), reply, next_reply], [1, 0, 3, 2], ""),
            ("transcript-sequential-with-overlapping-clock",
             [ask, dict(next_ask, timestamp=overlap_start), reply, next_reply], [0, 2, 1, 3], "Claude transcript contradicts"),
            ("touching", [ask, reply, dict(next_ask, timestamp=RETURN), next_reply], None, ""),
        ):
            with self.subTest(name=name):
                write_lines(session, raw)
                entries = write_claude_transcript(transcript, raw)
                if order is not None:
                    write_lines(transcript, [entries[0], *(entries[1 + index] for index in order), entries[-1]])
                self.output = self.parent / name
                if reason:
                    self.assert_rejected(trial, reason)
                else:
                    result = self.convert(trial)
                    self.assertEqual(result.returncode, 0, result.stderr)

    def test_codex_rejects_contradictory_or_unfinished_exit_evidence(self):
        trial = write_trial(self.parent)
        path = trial / "agent" / "codex.txt"
        entries = read_lines(path)
        completed, = (entry["item"] for entry in entries if entry["type"] == "item.completed")
        for status, exit_code in (("completed", EXIT), ("failed", False), ("in_progress", None)):
            with self.subTest(status=status, exit=exit_code):
                completed.update(status=status, exit_code=exit_code)
                write_lines(path, entries)
                self.assert_rejected(trial, "invalid transcript exit result")

    def test_claude_checks_artifact_precedence_at_equal_times(self):
        root = self.parent / "harbor-job"
        write_run(root)
        trial = root / "task__m-later"
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT}, stdout=OUTPUT, returned=START)
        ask, reply = read_lines(session)
        write_raw_bash(trial, {"command": "printf next"}, call_id="call-next", stdout="", returned=START)
        next_ask, next_reply = read_lines(session)
        raw = [ask, reply, next_ask, next_reply]
        write_lines(session, raw)
        transcript = agent / "claude-code.txt"
        entries = write_claude_transcript(transcript, raw)
        for name, order, accepted in (
            ("opposite", [2, 3, 0, 1], False),
            ("same", [0, 1, 2, 3], True),
            ("overlap", [2, 0, 3, 1], True),
        ):
            write_lines(transcript, [entries[0], *(entries[1 + index] for index in order), entries[-1]])
            for mode, convert, source in (("trial", self.convert, trial), ("run", self.convert_run, root)):
                with self.subTest(name=name, mode=mode):
                    self.output = self.parent / f"{mode}-{name}"
                    result = convert(source)
                    if accepted:
                        self.assertEqual(result.returncode, 0, result.stderr)
                    else:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("contradictory Claude transcript and raw session precedence", result.stderr)
                        self.assertFalse(self.output.exists())
                        self.assertEqual(list(self.parent.glob(".harbor-*")), [])

    def test_claude_compares_structured_result_without_key_order(self):
        trial = write_trial(self.parent)
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        raw = read_lines(write_raw_bash(trial, {"command": SCRIPT}, stdout=OUTPUT))
        path = agent / "claude-code.txt"
        entries = write_claude_transcript(path, raw)
        result = raw[-1]["toolUseResult"]
        for name, fields, accepted in (
            ("reordered", dict(reversed(list(result.items()))), True),
            ("different-output", dict(result, stdout="else"), False),
            ("number-not-boolean", dict(result, interrupted=0), False),
        ):
            with self.subTest(name=name):
                entries[-2]["tool_use_result"] = fields
                write_lines(path, entries)
                self.output = self.parent / name
                if accepted:
                    self.assertEqual(self.convert(trial).returncode, 0)
                else:
                    self.assert_rejected(trial, "differs from Claude transcript")

        raw[0]["message"]["content"][0]["input"]["description"] = False
        write_lines(next((agent / "sessions").rglob(f"{THREAD}.jsonl")), raw)
        entries = write_claude_transcript(path, raw)
        entries[1]["message"]["content"][0]["input"]["description"] = 0
        write_lines(path, entries)
        self.output = self.parent / "nested-request-type"
        self.assert_rejected(trial, "differs from Claude transcript")

    def test_raw_bash_result_without_completion_is_rejected(self):
        trial = write_trial(self.parent, agent_name=ALTERNATE_AGENT)
        (trial / "agent" / "codex.txt").unlink()
        rollout_path(trial).unlink()
        for stdout in (OUTPUT, ""):
            with self.subTest(stdout=stdout):
                write_raw_bash(trial, {"command": SCRIPT}, stdout=stdout)
                self.assert_rejected(trial, "claude-code.txt: absent or unreadable trace")

        session = write_raw_bash(trial, {"command": SCRIPT, "timeout": CONFIGURED_TIMEOUT_MS}, stdout="")
        request, returned = read_lines(session)
        returned["toolUseResult"]["interrupted"] = True
        transcript = trial / "agent" / "claude-code.txt"
        write_lines(session, [request, returned])
        write_claude_transcript(transcript, [request, returned])
        self.assert_rejected(trial, f"call {CALL} Bash: cannot correlate raw Bash result fields")
        returned["toolUseResult"]["interrupted"] = False
        returned["toolUseResult"]["backgroundTaskId"] = "synthetic-background-task"
        write_lines(session, [request, returned])
        write_claude_transcript(transcript, [request, returned])
        self.assert_rejected(trial, f"call {CALL} Bash: unrecognized raw Bash result fields")
        del returned["toolUseResult"]["backgroundTaskId"]
        del returned["toolUseResult"]["stdout"]
        write_lines(session, [request, returned])
        self.assert_rejected(trial, f"call {CALL} Bash: cannot correlate raw Bash output streams")

    def test_raw_bash_result_must_match_its_request(self):
        trial = write_trial(self.parent, agent_name=ALTERNATE_AGENT)
        (trial / "agent" / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT}, stdout=OUTPUT)
        request, returned = read_lines(session)
        block, = returned["message"]["content"]
        block["tool_use_id"] = "different-call"
        write_lines(session, [request, returned])
        self.assert_rejected(trial, "different-call", "tool result has no pending request")

    def test_raw_bash_diagnostics_distinguish_request_shapes(self):
        trial = write_trial(self.parent, agent_name=ALTERNATE_AGENT)
        (trial / "agent" / "codex.txt").unlink()
        rollout_path(trial).unlink()
        fractional_timeout_ms = 0.5
        for inputs, reason in (
            ({}, "missing input.command shell script"),
            ({"command": []}, "invalid input.command shell script"),
            ({"command": SCRIPT, "argv": ["/bin/bash", "-c", SCRIPT]},
             "unrecognized Bash invocation fields"),
            *(({"command": SCRIPT, "timeout": value}, "invalid Bash timeout")
              for value in (None, False, True, 0, -CONFIGURED_TIMEOUT_MS, str(CONFIGURED_TIMEOUT_MS),
                            fractional_timeout_ms)),
            *(({"command": SCRIPT, "dangerouslyDisableSandbox": value}, "invalid Bash dangerouslyDisableSandbox")
              for value in (None, int(True), "true")),
        ):
            with self.subTest(inputs=inputs, reason=reason):
                write_raw_bash(trial, inputs)
                result = self.convert(trial)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(f"call {CALL} Bash: {reason}", result.stderr)
                self.assertFalse(self.output.exists())

    def test_mixed_evidence_rejects_an_unmatched_call_without_publishing(self):
        root = self.parent / "harbor-job"
        write_run(root, agent_name=ALTERNATE_AGENT)
        for occurrence in ("task__b-empty", "task__m-later"):
            trial = root / occurrence
            call_id = "toolu_additional"
            write_raw_bash(trial, {"command": SCRIPT}, call_id=call_id)
            for mode, convert, path in (("trial", self.convert, trial), ("run", self.convert_run, root)):
                with self.subTest(occurrence=occurrence, mode=mode):
                    self.output = self.parent / f"{occurrence}-{mode}"
                    result = convert(path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(occurrence, result.stderr)
                    self.assertIn(f"call {call_id}: no matching rollout request", result.stderr)
                    self.assertFalse(self.output.exists())
                    self.assertEqual(list(self.parent.glob(".harbor-*")), [])
            (trial / "agent" / "sessions" / "projects" / "-app" / f"{THREAD}.jsonl").unlink()

    def test_mixed_evidence_checks_session_request_and_result_before_publication(self):
        for case, reason, thread, script, stdout in (
            ("session", "sessionId differs", "other-session", SCRIPT, OUTPUT),
            ("script", "script, workdir or request time differs", THREAD, "printf other", OUTPUT),
            ("result", "output or result status differs", THREAD, SCRIPT, "other output"),
            ("match", "", THREAD, SCRIPT, OUTPUT),
        ):
            with self.subTest(case=case):
                trial = write_trial(self.parent / case, agent_name=ALTERNATE_AGENT)
                transcript = trial / "agent" / "codex.txt"
                items = read_lines(transcript)
                completed, = (item["item"] for item in items if item["type"] == "item.completed")
                completed.update(exit_code=0, status="completed")
                write_lines(transcript, items)
                write_raw_bash(trial, {"command": script}, thread=thread, stdout=stdout)
                self.output = self.parent / f"source-{case}"
                result = self.convert(trial)
                if reason:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(f"call {CALL}: {reason}", result.stderr)
                    self.assertFalse(self.output.exists())
                    self.assertEqual(list(self.parent.glob(".harbor-*")), [])
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    call, = read_lines(self.output / TRIAL / "bridge" / "tool-calls.jsonl")
                    self.assertEqual(call["tool_call_id"], CALL)
                    self.assertEqual(call["argv"], ["/bin/bash", "-l", "-c", SCRIPT])

    def test_event_level_result_requires_one_result_block(self):
        root = self.parent / "harbor-job"
        write_run(root, agent_name=ALTERNATE_AGENT)
        occurrence = "task__b-empty"
        trial = root / occurrence
        session = write_raw_bash(trial, {"command": SCRIPT})
        _, result_event = read_lines(session)
        block, = result_event["message"]["content"]
        for case, content in (
            ("absent", []),
            ("ambiguous", [block, dict(block, tool_use_id="other-call")]),
        ):
            result_event["message"]["content"] = content
            write_lines(session, [result_event])
            for mode, convert, path in (("trial", self.convert, trial), ("run", self.convert_run, root)):
                with self.subTest(case=case, mode=mode):
                    self.output = self.parent / f"{case}-{mode}"
                    result = convert(path)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(occurrence, result.stderr)
                    self.assertIn("toolUseResult requires exactly one tool_result block", result.stderr)
                    self.assertFalse(self.output.exists())
                    self.assertEqual(list(self.parent.glob(".harbor-*")), [])

    def test_run_rejects_missing_trial_directory(self):
        root = self.parent / "harbor-job"
        write_run(root)
        (root / "task__m-later").rename(self.parent / "removed-trial")
        result = self.convert_run(root)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("trial directory count", result.stderr)
        self.assertFalse(self.output.exists())
        self.assertEqual(list(self.parent.glob(".harbor-*")), [])

    def test_run_requires_valid_ordered_completion_timestamps(self):
        root = self.parent / "harbor-job"
        write_run(root)
        path = root / "result.json"
        result = json.loads(path.read_bytes())
        for index, (changes, message) in enumerate((
            ({"finished_at": "pending"}, "finished_at"),
            ({"started_at": "unknown"}, "started_at"),
            ({"finished_at": "2026-11-05T11:59:59.999999"}, "before"),
        )):
            with self.subTest(changes=changes):
                self.output = self.parent / f"invalid-{index}"
                write_json(path, dict(result, **changes))
                converted = self.convert_run(root)
                self.assertNotEqual(converted.returncode, 0)
                self.assertIn(message, converted.stderr)
                self.assertFalse(self.output.exists())
        write_json(path, result)
        self.output = self.parent / "valid"
        converted = self.convert_run(root)
        self.assertEqual(converted.returncode, 0, converted.stderr)

    def test_run_reconciles_completed_counts_without_counting_errors_twice(self):
        root = self.parent / "harbor-job"
        write_run(root)
        path = root / "result.json"
        result = json.loads(path.read_bytes())
        total = result["n_total_trials"]
        for index, stats in enumerate((
            {"n_completed_trials": total + 1},
            {"n_completed_trials": total - 1},
            {"n_completed_trials": total - 1, "n_errored_trials": 1},
            {"n_completed_trials": total, "n_trials": total - 1},
            {"n_completed_trials": total, "n_pending_trials": 1},
            {"n_completed_trials": total, "n_running_trials": 1},
            {"n_completed_trials": total, "n_errored_trials": total + 1},
        )):
            with self.subTest(stats=stats):
                self.output = self.parent / f"invalid-{index}"
                write_json(path, dict(result, stats=stats))
                converted = self.convert_run(root)
                self.assertNotEqual(converted.returncode, 0)
                self.assertIn("trial count", converted.stderr)
                self.assertFalse(self.output.exists())

        trial_path = root / "task__m-later" / "result.json"
        trial = json.loads(trial_path.read_bytes())
        del trial["verifier_result"]
        trial["exception_info"] = {
            "exception_type": "RuntimeError", "exception_message": "synthetic verifier exception",
            "exception_traceback": "RuntimeError: synthetic verifier exception", "occurred_at": END,
        }
        write_json(trial_path, trial)
        for name, stats in (
            ("current", {"n_completed_trials": total, "n_errored_trials": 1}),
            ("legacy", {"n_trials": total, "n_errors": 1}),
        ):
            with self.subTest(name=name):
                self.output = self.parent / name
                write_json(path, dict(result, stats=stats))
                converted = self.convert_run(root)
                self.assertEqual(converted.returncode, 0, converted.stderr)
                entries = json.loads((self.output / "run-result.json").read_bytes())["tasks"]
                self.assertIn({"task_id": "task__m-later"}, entries)

    def test_output_must_not_become_an_input_trial(self):
        root = self.parent / "harbor-job"
        write_run(root)
        alias = self.parent / "alias"
        alias.symlink_to(root, target_is_directory=True)
        trial = root / "task__m-later"
        before = sorted(root.iterdir())
        for selection, convert in (("run", self.convert_run), ("trial", self.convert)):
            for parent in (root, alias):
                with self.subTest(parent=parent, selection=selection):
                    self.output = parent / f"converted-{selection}-{parent.name}"
                    converted = convert(root if selection == "run" else trial)
                    self.assertNotEqual(converted.returncode, 0)
                    self.assertIn("output", converted.stderr)
                    self.assertEqual(sorted(root.iterdir()), before)
        self.output = self.parent / "source"
        converted = self.convert_run(root)
        self.assertEqual(converted.returncode, 0, converted.stderr)

    def test_run_releases_staged_log_before_next_trial(self):
        root = self.parent / "harbor-job"
        write_run(root)
        probe = """
import sys
import weakref
import convert_harbor as converter

class Log(bytearray):
    pass

convert = converter.convert
previous = lambda: None

def track_log(trial):
    global previous
    assert previous() is None, "staged log still held before conversion"
    run, entry, metadata, calls, first, size, offsets, partial = convert(trial)
    log = Log(calls)
    previous = weakref.ref(log)
    return run, entry, metadata, log, first, size, offsets, partial

converter.convert = track_log
sys.exit(converter.main())
"""
        converted = subprocess.run(
            [sys.executable, "-B", "-c", probe, "--harbor-run", str(root), "--output-dir", str(self.output)],
            cwd=CONVERTER.parent, capture_output=True, text=True, check=False,
        )
        self.assertEqual(converted.returncode, 0, converted.stderr)

    def test_run_does_not_publish_after_a_later_trial_fails(self):
        for cause in ("unsupported", "absent", "trial job ID"):
            with self.subTest(cause=cause):
                root = self.parent / cause
                write_run(root)
                trial = root / "task__m-later"
                path = rollout_path(trial)
                if cause == "unsupported":
                    events = read_lines(path)
                    for record in events:
                        if record["payload"].get("type") == "custom_tool_call":
                            record["payload"]["input"] = "unsupported"
                    write_lines(path, events)
                elif cause == "absent":
                    path.unlink()
                else:
                    trial.rename(self.parent / "replacement")
                    write_trial(root, name=trial.name, run="different-job")
                result = self.convert_run(root)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(trial.name, result.stderr)
                self.assertIn(cause, result.stderr)
                if cause == "unsupported":
                    self.assertIn(CALL, result.stderr)
                self.assertFalse(self.output.exists())
                self.assertEqual(list(self.parent.glob(".harbor-*")), [])

    def test_source_flags_and_output_are_required(self):
        root = self.parent / "harbor-job"
        write_run(root)
        trial = root / "task__m-later"
        trial_flag = ("--harbor-trial", str(trial))
        run_flag = ("--harbor-run", str(root))
        output_flag = ("--output-dir", str(self.output))
        for flags in (output_flag, trial_flag + run_flag + output_flag, trial_flag, run_flag):
            with self.subTest(flags=flags):
                result = subprocess.run([sys.executable, str(CONVERTER), *flags], cwd=self.parent,
                                        capture_output=True, text=True, check=False)
                self.assertEqual(result.returncode, 2, result.stderr)  # argparse usage status.
                self.assertIn("usage:", result.stderr)
                self.assertEqual(list(self.parent.iterdir()), [root])
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertCountEqual([path.name for path in self.output.iterdir()], ("run-result.json", trial.name))
        result = self.convert_run(root)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already exists", result.stderr)
        self.assertCountEqual([path.name for path in self.output.iterdir()], ("run-result.json", trial.name))

    def test_run_rejects_unrepresentable_shared_offset(self):
        root = self.parent / "harbor-job"
        write_run(root)
        trial = root / "task__m-later"
        set_timing(trial, "2426-11-05T12:00:31.123456789Z", "2426-11-05T12:00:32.123456789Z",
                   "2426-11-05T12:00:37.123456789Z")
        self.assertEqual(self.convert(trial).returncode, 0)
        self.output = self.parent / "run-source"
        result = self.convert_run(root)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(trial.name, result.stderr)
        self.assertIn(CALL, result.stderr)
        self.assertIn("global call offset", result.stderr)
        self.assertEqual(list(self.parent.glob(".harbor-*")), [])
        self.assertFalse(self.output.exists())

    def test_run_plan_size_includes_clock_and_expanded_offsets(self):
        root = self.parent / "harbor-job"
        write_run(root)
        # Later trial: local plan bound plus the clock and the extra offset digits.
        local_plan_bound = 499
        clock_bytes = 45
        offset_growth = len("30000000000") - len("0")
        shared_plan_bound = local_plan_bound + clock_bytes + offset_growth
        for limit in (shared_plan_bound - 1, shared_plan_bound):
            with self.subTest(limit=limit):
                result = subprocess.run(
                    [sys.executable, "-B", "-c",
                     "import sys, convert_harbor as c; c.MAX_PLAN_BYTES = int(sys.argv.pop(1)); sys.exit(c.main())",
                     str(limit), "--harbor-run", str(root), "--output-dir", str(self.output)],
                    cwd=CONVERTER.parent, capture_output=True, text=True, check=False,
                )
                if limit == shared_plan_bound:
                    self.assertEqual(result.returncode, 0, result.stderr)
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("task__m-later", result.stderr)
                    self.assertIn(CALL, result.stderr)
                    self.assertIn("global plan", result.stderr)
                    self.assertFalse(self.output.exists())

    def test_partial_plan_size_includes_unchecked_policy(self):
        root = self.parent / "harbor-job"
        write_run(root)
        trial = root / "task__m-later"
        agent = trial / "agent"
        (agent / "codex.txt").unlink()
        rollout_path(trial).unlink()
        session = write_raw_bash(trial, {"command": SCRIPT}, stdout=OUTPUT,
                                 start="2026-11-05T12:00:31.123456789Z",
                                 returned="2026-11-05T12:00:32.123456789Z")
        write_claude_transcript(agent / "claude-code.txt", read_lines(session))
        # Compare with the complete fixture above. Claude also uses a non-login shell.
        codex_plan_bound = 499
        omitted = len(',"exit_code":7,"login":true')
        added = len(',"harbor_tool_error":false,"skip_response_check":true')
        local = codex_plan_bound - omitted + added
        clock = len(',"replay_at":"2006-01-02T15:04:05.999999999Z"')
        growth = len("30000000000") - len("0")
        for mode, source, bound in (
            ("trial", trial, local),
            ("run", root, local + clock + growth),
        ):
            for limit in (bound - 1, bound):
                with self.subTest(mode=mode, limit=limit):
                    self.output = self.parent / f"source-{mode}-{limit}"
                    result = subprocess.run(
                        [sys.executable, "-B", "-c",
                         "import sys, convert_harbor as c; c.MAX_PLAN_BYTES = int(sys.argv.pop(1)); sys.exit(c.main())",
                         str(limit), f"--harbor-{mode}", str(source), "--output-dir", str(self.output)],
                        cwd=CONVERTER.parent, capture_output=True, text=True, check=False,
                    )
                    if limit == bound:
                        self.assertEqual(result.returncode, 0, result.stderr)
                    else:
                        self.assertNotEqual(result.returncode, 0, result.stderr)
                        self.assertIn("plan exceeds", result.stderr)
                        self.assertFalse(self.output.exists())
                        self.assertEqual(list(self.parent.glob(".harbor-*")), [])

    def read_source(self):
        bridge = self.output / TRIAL / "bridge"
        log = (bridge / "tool-calls.jsonl").read_bytes()
        runtime = json.loads((bridge / "runtime.json").read_bytes())
        index = json.loads((self.output / "run-result.json").read_bytes())
        self.assertEqual(runtime["tool_calls_bytes"], len(log))
        self.assertEqual(runtime["tool_calls_sha256"], hashlib.sha256(log).hexdigest())
        for directory in (self.output, self.output / TRIAL, bridge):
            self.assertEqual(directory.stat().st_mode & 0o777, 0o700)
        for file in (self.output / "run-result.json", bridge / "runtime.json", bridge / "tool-calls.jsonl"):
            self.assertEqual(file.stat().st_mode & 0o777, 0o600)
        return index, runtime, [json.loads(line) for line in log.splitlines()]

    def test_shell_with_optional_wait(self):
        result = self.convert(write_trial(self.parent))
        self.assertEqual(result.returncode, 0, result.stderr)
        index, runtime, calls = self.read_source()
        self.assertEqual(index, {"run_id": RUN, "tasks": [{"task_id": TRIAL,
                         "evaluation": {"status": "succeeded", "reward": 0}}]})
        self.assertEqual(runtime["origin_type"], "harbor")
        self.assertEqual(runtime["run_id"], RUN)
        self.assertEqual(runtime["occurrence_id"], TRIAL)
        self.assertEqual(runtime["task_id"], "task")
        self.assertEqual(runtime["benchmark_type"], "terminalbench2")
        self.assertEqual(runtime["task_package_digest"], DIGEST)
        self.assertEqual(runtime["harness_completed_at"], END)
        self.assertNotIn("image", runtime)
        self.assertNotIn("benchmark_revisions", runtime)
        call, = calls
        self.assertEqual(call, {
            "run_id": RUN, "task_id": TRIAL, "sandbox_id": runtime["SandboxID"], "sequence": 1,
            "operation_class": "exec", "path": "/bin/bash", "workdir": WORKDIR,
            "command": SCRIPT, "command_hash": hashlib.sha256(SCRIPT.encode()).hexdigest(),
            "argv": ["/bin/bash", "-l", "-c", SCRIPT], "stdin": "", "stdin_encoding": "utf-8",
            "stdin_bytes": 0, "status": "completed", "exit_code": EXIT,
            "tool_call_id": CALL, "started_at": START, "timestamp": RETURN, "harbor_output": OUTPUT,
        })

    def test_shell_and_add_file_with_and_without_trajectory(self):
        for trajectory in (False, True):
            with self.subTest(trajectory=trajectory), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                parent = Path(directory)
                self.output = parent / "source"
                result = self.convert(write_trial(parent, with_trajectory=trajectory, with_patch=True))
                self.assertEqual(result.returncode, 0, result.stderr)
                _, runtime, (shell, patch) = self.read_source()
                self.assertEqual(shell["command"], SCRIPT)
                self.assertEqual(patch, {
                    "run_id": RUN, "task_id": TRIAL, "sandbox_id": runtime["SandboxID"], "sequence": shell["sequence"] + 1,
                    "operation_class": "write", "path": PATCH_PATH, "harbor_patch": "add", "stdin": PATCH_BODY,
                    "stdin_encoding": "utf-8", "stdin_bytes": len(PATCH_BODY.encode()),
                    "sha256": hashlib.sha256(PATCH_BODY.encode()).hexdigest(), "status": "completed",
                    "tool_call_id": PATCH_CALL, "started_at": PATCH_START, "timestamp": PATCH_START,
                })

    def test_rejects_wrong_trajectory_observation(self):
        trial = write_trial(self.parent, with_trajectory=True)
        path = trial / "agent" / "trajectory.json"
        trajectory = json.loads(path.read_bytes())
        trajectory["steps"][0]["observation"]["results"][0]["source_call_id"] = "wrong-call"
        write_json(path, trajectory)
        self.assert_rejected(trial, CALL, "trajectory")

    def test_rejects_unsupported_wrappers(self):
        bad_wrappers = {
            "input-bearing wait": SHELL_WRAPPER + WAIT.replace('chars:""', 'chars:"input"'),
            "termination": SHELL_WRAPPER + WAIT.replace('chars:""', 'chars:"\\u0003"'),
            "session reuse": SHELL_WRAPPER + WAIT.replace("session_id:r.session_id", "session_id:33"),
            "script generated outside shell": SHELL_WRAPPER.replace(json.dumps(SCRIPT), "previous.output"),
        }
        for label, wrapper in bad_wrappers.items():
            with self.subTest(label=label), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                self.assert_rejected(write_trial(Path(directory), wrapper=wrapper), CALL, "unsupported")

    def test_rejects_later_session_call_without_transcript_item(self):
        trial = write_trial(self.parent)
        path = rollout_path(trial)
        requests = read_lines(path)
        call_id = "later-session"
        requests[-1:-1] = [
            event("response_item", timestamp=RETURN, type="custom_tool_call", name="exec",
                  call_id=call_id, input='text((await tools.write_stdin({session_id:33,chars:""})).output);'),
            dict(requests[-2], payload=dict(requests[-2]["payload"], call_id=call_id)),
        ]
        write_lines(path, requests)
        self.assert_rejected(trial, call_id, "unsupported wrapper")

    def test_package_digest_in_lock(self):
        trial = write_trial(self.parent)
        path = trial / "config.json"
        config = json.loads(path.read_bytes())
        del config["task"]["ref"]
        write_json(path, config)
        write_json(trial / "lock.json", {"task": dict(config["task"], type="package", digest=DIGEST)})
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, runtime, _ = self.read_source()
        self.assertEqual(runtime["task_package_digest"], DIGEST)

    def test_rejects_ambiguous_or_incomplete_rollout(self):
        for label, alter, reason in (
            ("duplicate request", lambda requests: requests.insert(3, requests[2]), "ambiguous"),
            ("missing return", lambda requests: requests.pop(3), "missing"),
            ("different output", lambda requests: requests[3]["payload"]["output"].pop(), "incomplete"),
        ):
            with self.subTest(label=label), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                trial = write_trial(Path(directory))
                path = rollout_path(trial)
                requests = read_lines(path)
                alter(requests)
                write_lines(path, requests)
                self.assert_rejected(trial, CALL, reason)

    def test_rejects_missing_or_failed_add_file_evidence(self):
        for field, value in (("success", False), ("changes", {PATCH_PATH: {"type": "add"}})):
            with self.subTest(field=field), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                trial = write_trial(Path(directory), with_patch=True)
                path = rollout_path(trial)
                requests = read_lines(path)
                patch = next(event["payload"] for event in requests if event["payload"].get("type") == "patch_apply_end")
                patch[field] = value
                write_lines(path, requests)
                self.assert_rejected(trial, PATCH_CALL, "add-file")

    def test_zero_calls_without_grade_requires_complete_trace(self):
        trial = write_trial(self.parent)
        result = trial / "result.json"
        data = json.loads(result.read_bytes())
        del data["verifier_result"]
        write_json(result, data)
        transcript = trial / "agent" / "codex.txt"
        events = read_lines(transcript)
        write_lines(transcript, [event for event in events if "item" not in event])
        rollout = rollout_path(trial)
        events = read_lines(rollout)
        zero = [event for event in events if event["type"] != "response_item"]
        write_lines(rollout, zero)

        self.output = self.parent / "complete"
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        index, _, calls = self.read_source()
        self.assertEqual(index, {"run_id": RUN, "tasks": [{"task_id": TRIAL}]})
        self.assertEqual(calls, [])

        self.output = self.parent / "incomplete"
        write_lines(rollout, zero[:-1])
        self.assert_rejected(trial, "incomplete")
        rollout.write_bytes(b"\xff")
        self.assert_rejected(trial, "unreadable")
        rollout.unlink()
        self.assert_rejected(trial, "missing rollout")

    def test_rejects_orphan_tool_event_in_otherwise_empty_trace(self):
        trial = write_trial(self.parent)
        transcript = trial / "agent" / "codex.txt"
        write_lines(transcript, [entry for entry in read_lines(transcript) if "item" not in entry])
        rollout = rollout_path(trial)
        entries = [entry for entry in read_lines(rollout) if entry["type"] != "response_item"]
        orphan = "orphan-exec"
        entries.insert(-1, event("event_msg", type="exec_command_end", call_id=orphan))
        write_lines(rollout, entries)
        self.assert_rejected(trial, orphan, "tool")

    def test_two_completed_turns(self):
        trial = write_trial(self.parent)
        call_id, start, end = add_shell(trial, overlap=False)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, _, (first, second) = self.read_source()
        self.assertEqual(first["started_at"], START)
        self.assertEqual((second["tool_call_id"], second["started_at"], second["timestamp"]), (call_id, start, end))

    def test_overlapping_calls_join_by_evidence(self):
        trial = write_trial(self.parent)
        call_id, start, end = add_shell(trial, overlap=True)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, _, (second, first) = self.read_source()
        self.assertEqual((second["tool_call_id"], second["started_at"], second["timestamp"]), (call_id, start, end))
        self.assertEqual(second["harbor_output"], "second")
        self.assertEqual((first["tool_call_id"], first["started_at"], first["timestamp"]), (CALL, START, RETURN))
        self.assertEqual(first["harbor_output"], OUTPUT)

    def test_rejects_indistinguishable_overlapping_calls(self):
        trial = write_trial(self.parent)
        add_shell(trial, overlap=True, same=True)
        self.assert_rejected(trial, "ambiguous")

    def test_rollout_clock_allows_ties_but_not_reversal(self):
        trial = write_trial(self.parent)
        add_shell(trial, overlap=False)
        path = rollout_path(trial)
        entries = read_lines(path)
        tools = [entry for entry in entries if entry["type"] == "response_item"]
        for entry in tools:
            entry["timestamp"] = PATCH_START
        write_lines(path, entries)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.output = self.parent / "reversed"
        for entry in tools[-2:]:
            entry["timestamp"] = START
        write_lines(path, entries)
        self.assert_rejected(trial, "call-second", "timestamp")

    def test_rejects_reversed_strict_precedence_but_allows_overlap(self):
        trial = write_trial(self.parent)
        add_shell(trial, overlap=True)
        transcript = trial / "agent" / "codex.txt"
        items = read_lines(transcript)
        # The transcript establishes first-before-second; the rollout overlaps.
        items[3], items[4] = items[4], items[3]
        write_lines(transcript, items)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.output = self.parent / "reversed"
        path = rollout_path(trial)
        entries = read_lines(path)
        first_request = entries.pop(2)
        first_request["timestamp"] = RETURN
        entries.insert(-2, first_request)
        write_lines(path, entries)
        self.assert_rejected(trial, CALL, "precedence")

    def test_overlapping_adds_need_unique_path_and_content_evidence(self):
        for same in (False, True):
            with self.subTest(same=same), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                parent = Path(directory)
                self.output = parent / "source"
                trial = write_trial(parent, with_patch=True)
                transcript = trial / "agent" / "codex.txt"
                items = read_lines(transcript)
                path = rollout_path(trial)
                entries = read_lines(path)
                patch_path = PATCH_PATH if same else "/app/second.txt"
                other_call = "call-second-patch"
                request, change, response = entries[-4:-1]
                second_request = dict(request["payload"], call_id=other_call,
                                      input=request["payload"]["input"].replace(PATCH_PATH, patch_path))
                second_change = dict(change["payload"], call_id="nested-second",
                                     changes={patch_path: {"type": "add", "content": PATCH_BODY}})
                second_response = dict(response["payload"], call_id=other_call)
                entries[-3:-3] = [dict(request, payload=second_request), dict(change, payload=second_change),
                                  dict(response, payload=second_response)]
                write_lines(path, entries)
                start, end = items[-3:-1]
                second_items = [dict(entry, item=dict(entry["item"], id="item_3",
                                changes=[{"path": patch_path, "kind": "add"}])) for entry in (start, end)]
                items[-2:-2] = second_items
                write_lines(transcript, items)
                if same:
                    self.assert_rejected(trial, "nested-second", "ambiguous")
                    continue
                result = self.convert(trial)
                self.assertEqual(result.returncode, 0, result.stderr)
                _, _, (_, second, first) = self.read_source()
                self.assertEqual((second["tool_call_id"], second["path"], second["stdin"]), (other_call, patch_path, PATCH_BODY))
                self.assertEqual((first["tool_call_id"], first["path"], first["stdin"]), (PATCH_CALL, PATCH_PATH, PATCH_BODY))

    def test_overlapping_identical_shell_outputs_use_explicit_login(self):
        trial = write_trial(self.parent)
        call_id, _, _ = add_shell(trial, overlap=True, same=True)
        transcript = trial / "agent" / "codex.txt"
        items = read_lines(transcript)
        for entry in items:
            item = entry.get("item", {})
            if item.get("id") == "item_2":
                item["command"] = item["command"].replace("/bin/bash -lc", "/bin/bash -c", 1)
        write_lines(transcript, items)
        path = rollout_path(trial)
        entries = read_lines(path)
        request = next(entry["payload"] for entry in entries
                       if entry["payload"].get("call_id") == call_id and "input" in entry["payload"])
        wrapper = request["input"]
        request["input"] = wrapper.replace("});", ',"login":false});', 1)
        write_lines(path, entries)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, _, (second, first) = self.read_source()
        self.assertEqual((second["tool_call_id"], second["argv"]), (call_id, ["/bin/bash", "-c", SCRIPT]))
        self.assertEqual((first["tool_call_id"], first["argv"]), (CALL, ["/bin/bash", "-l", "-c", SCRIPT]))
        self.output = self.parent / "serial-repeat"
        repeat_id = "call-serial-repeat"
        repeat_command = "/bin/bash -c " + shlex.quote(SCRIPT)
        items[-1:-1] = [
            dict(entry, item=dict(entry["item"], id="item_3", command=repeat_command))
            for entry in items if entry.get("item", {}).get("id") == "item_2"
        ]
        write_lines(transcript, items)
        repeat_return = dict(entries[-3]["payload"], call_id=repeat_id)
        entries[-1:-1] = [
            event("response_item", timestamp=PATCH_START, type="custom_tool_call", name="exec",
                  call_id=repeat_id, input=wrapper),
            dict(entries[-3], timestamp=PATCH_START, payload=repeat_return),
        ]
        entries[-1]["timestamp"] = PATCH_START
        write_lines(path, entries)
        result = self.convert(trial)
        self.assertEqual(result.returncode, 0, result.stderr)
        _, _, (second, first, repeat) = self.read_source()
        self.assertEqual([second["tool_call_id"], first["tool_call_id"], repeat["tool_call_id"]],
                         [call_id, CALL, repeat_id])
        self.assertEqual([second["argv"], first["argv"], repeat["argv"]],
                         [["/bin/bash", "-c", SCRIPT], ["/bin/bash", "-l", "-c", SCRIPT],
                          ["/bin/bash", "-c", SCRIPT]])
        self.output = self.parent / "contradictory"
        first_request = next(entry["payload"] for entry in entries
                             if entry["payload"].get("call_id") == CALL and "input" in entry["payload"])
        first_request["input"] = first_request["input"].replace("});", ',"login":false});', 1)
        write_lines(path, entries)
        self.assert_rejected(trial, call_id, "login")

    def test_empty_wait_joins_output_chunks_without_separator(self):
        for wait in (True, False):
            with self.subTest(wait=wait), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                parent = Path(directory)
                self.output = parent / "source"
                trial = write_trial(parent, wrapper=SHELL_WRAPPER + (WAIT if wait else ""))
                path = rollout_path(trial)
                entries = read_lines(path)
                response = entries[-2]["payload"]["output"]
                prefix, separator, suffix = OUTPUT.partition("\r")
                response[-1:] = [{"type": "input_text", "text": chunk} for chunk in (prefix, separator + suffix)]
                write_lines(path, entries)
                if not wait:
                    self.assert_rejected(trial, CALL, "wait")
                    continue
                result = self.convert(trial)
                self.assertEqual(result.returncode, 0, result.stderr)
                _, _, (call,) = self.read_source()
                self.assertEqual(call["harbor_output"], OUTPUT)

    def test_invalid_grade_is_not_absent_or_zero(self):
        trial = write_trial(self.parent)
        path = trial / "result.json"
        recorded = path.read_text(encoding="utf-8")
        for index, grade in enumerate(("null", "1e-1000", "1e1000", "false", '"0"')):
            with self.subTest(grade=grade):
                self.output = self.parent / f"source-{index}"
                path.write_text(recorded.replace('"reward": 0', f'"reward": {grade}'), encoding="utf-8")
                self.assert_rejected(trial, "reward")

    def test_go_duration_bounds_at_nanosecond_precision(self):
        epoch = "2000-01-01T00:00:00Z"
        cases = (
            ("offset", "2292-04-10T23:47:16.854775807Z", True),
            ("offset", "2292-04-10T23:47:16.854775808Z", False),
            ("wait", "2292-04-10T23:47:16.854775807Z", True),
            ("wait", "2292-04-10T23:47:16.854775808Z", False),
        )
        for bound, limit, accepted in cases:
            with self.subTest(bound=bound, accepted=accepted), tempfile.TemporaryDirectory(dir=self.parent) as directory:
                parent = Path(directory)
                self.output = parent / "source"
                trial = write_trial(parent)
                if bound == "offset":
                    add_shell(trial, overlap=False)
                path = rollout_path(trial)
                entries = read_lines(path)
                for entry in entries:
                    entry["timestamp"] = limit if entry["timestamp"] == PATCH_START else epoch
                write_lines(path, entries)
                path = trial / "result.json"
                result = json.loads(path.read_bytes())
                result["agent_execution"]["finished_at"] = limit
                write_json(path, result)
                if accepted:
                    result = self.convert(trial)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    _, runtime, _ = self.read_source()
                    self.assertEqual(runtime["harness_completed_at"], limit)
                else:
                    self.assert_rejected(trial, bound, "duration")

    def test_staged_plan_byte_bound_accounts_for_go_json_escapes(self):
        trial = write_trial(self.parent)
        path = trial / "agent" / "codex.txt"
        entries = read_lines(path)
        payload = "<>&\u2028\u2029"
        entries[-2]["item"]["aggregated_output"] = payload
        write_lines(path, entries)
        path = rollout_path(trial)
        entries = read_lines(path)
        entries[-2]["payload"]["output"][-1]["text"] = payload
        write_lines(path, entries)
        path = trial / "result.json"
        result = json.loads(path.read_bytes())
        del result["verifier_result"]
        write_json(path, result)
        # Go LoadReplaySource followed by json.Marshal produces 487 bytes.
        go_plan_bytes = 487
        for limit in (go_plan_bytes, go_plan_bytes - 1):
            with self.subTest(limit=limit):
                self.output = self.parent / f"source-{limit}"
                result = subprocess.run(
                    [sys.executable, "-B", "-c",
                     "import sys, convert_harbor as c; c.MAX_PLAN_BYTES = int(sys.argv.pop(1)); sys.exit(c.main())",
                     str(limit), "--harbor-trial", str(trial), "--output-dir", str(self.output)],
                    cwd=CONVERTER.parent, capture_output=True, text=True, check=False,
                )
                if limit == go_plan_bytes:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.read_source()
                else:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("plan", result.stderr)
                    self.assertFalse(self.output.exists())

    def test_bridge_occurrence_would_hide_the_run_root(self):
        trial = write_trial(self.parent)
        reserved = "bridge"
        renamed = trial.with_name(reserved)
        trial.rename(renamed)
        for filename in ("config.json", "result.json"):
            path = renamed / filename
            path.write_text(path.read_text(encoding="utf-8").replace(TRIAL, reserved), encoding="utf-8")
        result = self.convert(renamed)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(reserved, result.stderr)
        self.assertIn("reserved", result.stderr)
        self.assertFalse(self.output.exists())

    def test_existing_destination_untouched(self):
        trial = write_trial(self.parent)
        self.output.mkdir()
        marker = self.output / "keep"
        marker.write_bytes(b"original")
        result = self.convert(trial)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("already exists", result.stderr)
        self.assertEqual(list(self.output.iterdir()), [marker])
        self.assertEqual(marker.read_bytes(), b"original")

    def assert_rejected(self, trial, *reasons):
        result = self.convert(trial)
        self.assertNotEqual(result.returncode, 0)
        for reason in (TRIAL, *reasons):
            self.assertIn(reason, result.stderr)
        self.assertFalse(self.output.exists())
        self.assertEqual(list(self.output.parent.glob(".harbor-*")), [])


if __name__ == "__main__":
    unittest.main()
