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
CALL = "call-shell"
START = "2026-11-05T12:00:01.123456789Z"
RETURN = "2026-11-05T12:00:02.123456789Z"
END = "2026-11-05T12:00:04Z"
SCRIPT = "  printf '%s\\n' \"$(printf é)\" \n"
OUTPUT = "  é\r\n\n"
WORKDIR = "/app"
DIGEST = "sha256:" + hashlib.sha256(b"package").hexdigest()
HEADER = "Script completed\nWall time 1.0 seconds\nOutput:\n"
EXIT = 7
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


def write_trial(parent, *, name=TRIAL, run=RUN, wrapper=SHELL_WRAPPER + WAIT, with_trajectory=False, with_patch=False):
    trial = parent / name
    agent = trial / "agent"
    sessions = agent / "sessions" / "2026" / "11" / "05"
    sessions.mkdir(parents=True)
    config = {
        "trial_name": name, "job_id": run, "agent": {"name": "codex"},
        "task": {"name": "terminal-bench/task", "source": "terminal-bench/terminal-bench-2-1",
                 "ref": DIGEST},
    }
    write_json(trial / "config.json", config)
    write_json(trial / "result.json", {
        "trial_name": name, "task_name": "terminal-bench/task", "source": config["task"]["source"],
        "task_id": {"org": "terminal-bench", "name": "task", "ref": None},
        "task_checksum": DIGEST.removeprefix("sha256:"), "agent_info": {"name": "codex"},
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


def write_run(root):
    names = ("task__z-early", "task__b-empty", "task__m-later", "task__a-empty", "task__y-early")
    for name in names:
        trial = write_trial(root, name=name)
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
    write_json(root / "config.json", {"job_name": root.name, "n_attempts": len(names), "agents": [{"name": "codex"}]})
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
    run, entry, metadata, calls, first, size, offsets = convert(trial)
    log = Log(calls)
    previous = weakref.ref(log)
    return run, entry, metadata, log, first, size, offsets

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
        self.assert_rejected(trial, "absent")

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
