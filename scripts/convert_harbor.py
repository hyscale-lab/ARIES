"""Convert supported Harbor trial or run evidence into a private ARIES replay source."""

import argparse
from collections import Counter, defaultdict
from contextlib import suppress
import ctypes
from datetime import datetime
from decimal import Decimal, InvalidOperation
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shlex
import sys
import tempfile


BASH = "/bin/bash"
TERMINAL_BENCH = "terminal-bench/terminal-bench-2-1"
SOURCE_SCHEMA = 1
PRIVATE_DIR = 0o700
PRIVATE_FILE = 0o600
MAX_EXIT_CODE = 255
# Match the ARIES source loader's file and call record bounds.
MAX_METADATA_BYTES = 64 << 20
MAX_RECORD_BYTES = MAX_METADATA_BYTES * 2
MAX_LOG_BYTES = 256 << 20
MAX_PLAN_BYTES = 512 << 20
REPLAY_ORIGIN_BYTES = len(',"replay_at":"2006-01-02T15:04:05.999999999Z"')
SKIP_RESPONSE_CHECK_BYTES = len(',"skip_response_check":true')
MAX_DURATION_NS = (1 << 63) - 1
NANOSECONDS_PER_SECOND = 10**9
SECONDS_PER_DAY = 24 * 60 * 60
GO_JSON_ESCAPES = {character: len(f"\\u{ord(character):04x}") - len(character.encode())
                   for character in "<>&\u2028\u2029"}
CALL_ID = re.compile(r"[A-Za-z0-9._:-]{1,128}")  # ARIES tool call ID contract.
UTC = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.([0-9]{1,9}))?(?:Z|\+00:00)")
JOB_TIME = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}"
                      r"(?:\.[0-9]{1,6})?(?:Z|[+-][0-9]{2}:[0-5][0-9])?")
NANOSECOND_DIGITS = 9
JSON_STRING = r'"(?:[^"\\\x00-\x1f]|\\.)*"'
EXEC_PREFIX = "const r = await tools.exec_command({"
EXEC_KEY = re.compile(rf'\s*({JSON_STRING}|[a-z_]+)\s*:')
EXEC_RETURN = re.compile(r"\}\);\s*text\(r\.output\);\s*")
WAIT = re.compile(
    r"if \(r\.session_id\)\s*text\(\(await tools\.write_stdin\("
    r'\{session_id:r\.session_id,\s*chars:""'
    r'(?:,\s*(?:yield_time_ms|"yield_time_ms"):[0-9]+)?'
    r'(?:,\s*(?:max_output_tokens|"max_output_tokens"):[0-9]+)?\}'
    r"\)\)\.output\);\s*"
)
PATCH = re.compile(rf"\s*const patch = ({JSON_STRING});\s*text\(await tools\.apply_patch\(patch\)\);\s*")
OUTPUT_HEADER = re.compile(r"Script completed\nWall time [0-9]+(?:\.[0-9]+)? seconds\nOutput:\n")
PASSIVE_EVENTS = {"user_message", "agent_message", "agent_reasoning", "token_count"}
EDIT_UNREAD_REASON = "File has not been read yet. Read it first before writing to it."
EDIT_UNREAD_DISPLAY = f"<tool_use_error>{EDIT_UNREAD_REASON}</tool_use_error>"


class Unsupported(ValueError):
    pass


def require(condition, reason):
    if not condition:
        raise Unsupported(reason)


def unsupported_tool(value):
    call_id = value.get("call_id")
    label = f"call {call_id}: " if isinstance(call_id, str) and CALL_ID.fullmatch(call_id) else ""
    raise Unsupported(f"{label}rollout: unmatched or unsupported tool activity")


def unique_fields(pairs):
    fields = dict(pairs)
    require(len(fields) == len(pairs), "duplicate JSON field")
    return fields


DECODER = json.JSONDecoder(object_pairs_hook=unique_fields)


def json_object(line, decoder=DECODER):
    value = decoder.decode(line)
    require(isinstance(value, dict), "expected a JSON object")
    return value


def load_json(path, decoder=DECODER):
    try:
        return json_object(path.read_text(encoding="utf-8"), decoder)
    except (ValueError, OSError, InvalidOperation):
        raise Unsupported(f"{path.name}: absent, unreadable or invalid JSON") from None


def events(path, *, transcript=False):
    try:
        with path.open(encoding="utf-8") as stream:
            for number, line in enumerate(stream, 1):
                if transcript and (line.startswith("WARNING:") or line == "Reading additional input from stdin...\n"):
                    continue
                try:
                    yield json_object(line)
                except ValueError:
                    raise Unsupported(f"{path.name}:{number}: invalid JSON event") from None
    except (OSError, UnicodeError):
        raise Unsupported(f"{path.name}: absent or unreadable trace") from None


def raw_bash_argv(inputs, argv=None, *, login=False):
    require(isinstance(inputs, dict), "invalid tool_use.input")
    require("command" in inputs, "missing input.command shell script")
    command = inputs["command"]
    require(isinstance(command, str) and command and "\0" not in command, "invalid input.command shell script")
    require(inputs.keys() <= {"command", "description", "timeout", "dangerouslyDisableSandbox"},
            "unrecognized Bash invocation fields")
    if "timeout" in inputs:
        timeout = inputs["timeout"]
        require(type(timeout) is int and timeout > 0, "invalid Bash timeout")
    if "dangerouslyDisableSandbox" in inputs:
        require(type(inputs["dangerouslyDisableSandbox"]) is bool, "invalid Bash dangerouslyDisableSandbox")
    non_login = [BASH, "-c", command]
    login_argv = [BASH, "-l", "-c", command]
    if argv is None:
        return login_argv if login else non_login
    require(argv in (non_login, login_argv), "script, workdir or request time differs from rollout")
    return argv


def raw_session_path(sessions):
    paths = list(sessions.glob("projects/*/*.jsonl"))
    require(len(paths) <= 1, "agent/sessions/projects/*/*.jsonl: multiple raw sessions")
    return next(iter(paths), None)


def raw_bash_requests(path, thread):
    requests = {}
    if path is not None:
        for event in events(path):
            message = event.get("message")
            if not isinstance(message, dict) or event.get("type") != "assistant" or message.get("role") != "assistant":
                continue
            blocks = message.get("content")
            if not isinstance(blocks, list):
                continue
            for block in blocks:
                if not isinstance(block, dict) or block.get("type") != "tool_use":
                    continue
                call_id = block.get("id")
                if isinstance(call_id, str) and CALL_ID.fullmatch(call_id):
                    request = (event, block) if event.get("sessionId") == thread and block.get("name") == "Bash" else None
                    requests[call_id] = None if call_id in requests else request
    return requests


def correlate_raw_bash_request(event, block, inputs, started, argv=None):
    argv = raw_bash_argv(block.get("input"), argv, login=inputs.get("login", False))
    require(argv[-1] == inputs["cmd"] and event.get("cwd") == inputs["workdir"]
            and timestamp(event.get("timestamp"))[1] == started[1],
            "script, workdir or request time differs from rollout")
    return argv


def check_raw_session(path, thread, pairs):
    if path is None:
        return {}, {}
    expected = {evidence[0]["call_id"]: (item, evidence, inputs, output, argv)
                for _, item, evidence, inputs, output, argv in pairs}
    normalized = {}
    pending = {}
    returned = {}
    session_matched = False
    for event in events(path):
        message = event.get("message")
        blocks = message.get("content") if isinstance(message, dict) else None
        if "toolUseResult" in event:
            results = [block for block in blocks if isinstance(block, dict)
                       and block.get("type") == "tool_result"] if isinstance(blocks, list) else []
            require(len(results) == 1, f"{path.name}: toolUseResult requires exactly one tool_result block")
        session_matched |= event.get("sessionId") == thread
        if not isinstance(blocks, list):
            continue
        for block in blocks:
            if not isinstance(block, dict) or block.get("type") not in {"tool_use", "tool_result"}:
                continue
            request = block["type"] == "tool_use"
            role = "assistant" if request else "user"
            require(event.get("type") == message.get("role") == role, f"{path.name}: invalid tool event role")
            call_id = block.get("id" if request else "tool_use_id")
            require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id), f"{path.name}: invalid tool call ID")
            label = f"{path.name}: call {call_id}"
            try:
                require(event.get("sessionId") == thread, "sessionId differs from rollout")
                require(call_id in expected, "no matching rollout request")
                item, evidence, inputs, output, argv = expected[call_id]
                require(item["type"] == "command_execution", "no known raw request match for rollout operation")
                if request:
                    require(call_id not in pending and call_id not in returned, "duplicate raw tool request")
                    require(block.get("name") == "Bash", "raw tool name does not match rollout operation")
                    argv = correlate_raw_bash_request(event, block, inputs, evidence[1], argv)
                    pending[call_id] = block["input"].get("dangerouslyDisableSandbox")
                    normalized[call_id] = argv
                else:
                    require(call_id in pending, "raw tool result has no pending request")
                    require(timestamp(event.get("timestamp"))[1] == evidence[3][1], "return time differs from rollout")
                    check_raw_bash_result(event, block, item, output, pending.pop(call_id))
                    returned[call_id] = block["is_error"]
            except Unsupported as error:
                raise Unsupported(f"{label}: {error}") from None
    require(session_matched, f"{path.name}: no matching rollout sessionId")
    missing = next((call_id for call_id in expected if call_id not in returned), None)
    require(missing is None, f"{path.name}: call {missing}: raw session lacks a matching request or result")
    return normalized, returned


def raw_bash_output(result, sandbox_override):
    require(isinstance(result, dict) and all(result.get(key) is False
            for key in ("interrupted", "isImage", "noOutputExpected")), "cannot correlate raw Bash result fields")
    stdout, stderr = result.get("stdout"), result.get("stderr")
    require(isinstance(stdout, str) and isinstance(stderr, str) and not (stdout and stderr),
            "cannot correlate raw Bash output streams")
    require(result.keys() <= {"stdout", "stderr", "interrupted", "isImage", "noOutputExpected",
                              "dangerouslyDisableSandbox"},
            "unrecognized raw Bash result fields")
    if "dangerouslyDisableSandbox" in result:
        recorded_override = result["dangerouslyDisableSandbox"]
        require(type(recorded_override) is bool, "invalid Bash result dangerouslyDisableSandbox")
        require(sandbox_override is None or recorded_override is sandbox_override,
                "Bash result dangerouslyDisableSandbox differs from request")
    return stdout or stderr


def claude_bash_response(event, block, sandbox_override):
    result = event.get("toolUseResult")
    content = block.get("content")
    require(type(block.get("is_error")) is bool, "invalid Bash result error flag")
    if isinstance(result, str):
        line = content.partition("\n")[0] if isinstance(content, str) else ""
        exit_code = re.fullmatch(r"Exit code ([0-9]{1,3})", line)
        require(block["is_error"] and exit_code and 0 < int(exit_code[1]) <= MAX_EXIT_CODE,
                "invalid or unsupported Bash exit result")
        require(result == "Error: " + content, "Bash error wrapper differs from display text")
        return {"exit_code": int(exit_code[1]), "harbor_tool_error": True}
    output = raw_bash_output(result, sandbox_override)
    require(not block["is_error"] and (content == output or output == "" and content == "(Bash completed with no output)"),
            "raw Bash result does not match captured output")
    return {"harbor_output": output, "harbor_tool_error": False}


def claude_blocks(event, label):
    message = event.get("message")
    content = message.get("content") if isinstance(message, dict) else None
    if not isinstance(content, list):
        return
    seen = set()
    for block in content:
        if not isinstance(block, dict) or block.get("type") not in {"tool_use", "tool_result"}:
            continue
        request = block["type"] == "tool_use"
        call_id = block.get("id" if request else "tool_use_id")
        require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id) and call_id not in seen,
                f"{label}: invalid or duplicate tool call ID")
        seen.add(call_id)
        require(event.get("type") == message.get("role") == ("assistant" if request else "user"),
                f"{label}: invalid tool event role")
        yield call_id, request, block


def claude_tool_proof(request, block):
    # The CLI transcript can omit helper fields attached to raw content blocks.
    keys = ("name", "input") if request else ("is_error", "content")
    return [block["type"], {key: block.get(key) for key in keys}]


def same_json(left, right):
    if type(left) is type(right) is dict:
        return left.keys() == right.keys() and all(same_json(value, right[key]) for key, value in left.items())
    if type(left) is type(right) is list:
        return len(left) == len(right) and all(same_json(a, b) for a, b in zip(left, right))
    return type(left) is type(right) and left == right


def check_claude_precedence(calls, starts, returns, reason):
    # Compare only precedence that the raw clock proves. Overlap and ties have no serial order.
    earlier = iter(calls)
    previous = next(earlier, None)
    latest_return = -1
    for _, call_id, started, _ in sorted(calls, key=lambda call: call[2][1]):
        while previous is not None and previous[3][1] < started[1]:
            latest_return = max(latest_return, returns[previous[1]])
            previous = next(earlier, None)
        require(latest_return < starts[call_id], f"call {call_id}: {reason}")
    # A return before another request in the artifact cannot overlap in time.
    earlier = iter(sorted(calls, key=lambda call: returns[call[1]]))
    previous = next(earlier, None)
    latest_time = -1
    for _, call_id, started, _ in sorted(calls, key=lambda call: starts[call[1]]):
        while previous is not None and returns[previous[1]] < starts[call_id]:
            latest_time = max(latest_time, previous[3][1])
            previous = next(earlier, None)
        require(latest_time <= started[1], f"call {call_id}: {reason}")


def claude_calls(path, transcript, completed):
    """Match raw timed calls with the finished Claude transcript by event and call IDs."""
    require(path is not None, "agent/sessions/projects/*/*.jsonl: missing raw Claude session")
    requests, calls, transcript_calls_by_event, trajectory_requests = {}, [], {}, {}
    edit_rejections = set()
    thread = None
    raw_starts, raw_returns = {}, {}
    for position, event in enumerate(events(path)):
        blocks = list(claude_blocks(event, path.name))
        require("toolUseResult" not in event or len(blocks) == 1 and not blocks[0][1],
                f"{path.name}: toolUseResult requires exactly one tool_result block")
        if not blocks:
            continue
        event_id, session = event.get("uuid"), event.get("sessionId")
        require(isinstance(event_id, str) and event_id and event_id not in transcript_calls_by_event
                and isinstance(session, str) and session and (thread is None or session == thread)
                and event.get("isSidechain") is False, f"{path.name}: invalid raw Claude tool event identity")
        thread = session
        time = timestamp(event.get("timestamp"))
        proof = {}
        transcript_calls_by_event[event_id] = (proof, event.get("timestamp"), event.get("toolUseResult"))
        for call_id, request, block in blocks:
            tool = block.get("name") if request else trajectory_requests.get(call_id, (None, None))[0]
            try:
                require(time[1] <= completed[1], "tool event is after agent phase end")
                proof[call_id] = claude_tool_proof(request, block)
                if request:
                    require(call_id not in requests, "duplicate raw tool request")
                    inputs = block.get("input")
                    if tool == "Edit":
                        require(isinstance(inputs, dict)
                                and inputs.keys() <= {"file_path", "old_string", "new_string", "replace_all"}
                                and all(isinstance(inputs.get(key), str) for key in ("file_path", "old_string", "new_string"))
                                and inputs["file_path"].startswith("/") and "\0" not in inputs["file_path"]
                                and type(inputs.get("replace_all", False)) is bool, "invalid Edit request")
                        requests[call_id] = (time, event_id)
                    else:
                        require(tool == "Bash", "unsupported raw session tool request")
                        argv = raw_bash_argv(inputs)
                        workdir = event.get("cwd")
                        check_workdir(workdir)
                        requests[call_id] = (time, argv, workdir, inputs.get("dangerouslyDisableSandbox"))
                    raw_starts[call_id] = position
                    trajectory_requests[call_id] = (tool, inputs)
                else:
                    pending = requests.get(call_id)
                    require(pending is not None, "tool result has no pending request")
                    started = pending[0]
                    require(time[1] >= started[1], "return is earlier than request")
                    if tool == "Edit":
                        require(event.get("parentUuid") == pending[1], "Edit result is not a child of the request")
                        require(block.get("is_error") is True and block.get("content") == EDIT_UNREAD_DISPLAY
                                and event.get("toolUseResult") == "Error: " + EDIT_UNREAD_REASON,
                                "unsupported Edit result: no confirmed unread-file rejection")
                        edit_rejections.add(call_id)
                        record = dict(operation_class="source_rejection", operation="Edit", rejection_reason="file_not_read")
                    else:
                        _, argv, workdir, sandbox_override = pending
                        command = argv[-1]
                        record = dict(operation_class="exec", path=BASH, argv=argv, command=command, workdir=workdir,
                                      command_hash=digest(command.encode()), stdin="", stdin_bytes=0)
                        record.update(claude_bash_response(event, block, sandbox_override))
                    calls.append((record, call_id, started, time))
                    raw_returns[call_id] = position
                    requests[call_id] = None
            except Unsupported as error:
                label = f" {tool}" if isinstance(tool, str) and CALL_ID.fullmatch(tool) else ""
                raise Unsupported(f"{path.name}: call {call_id}{label}: {error}") from None
    missing = next((call_id for call_id, pending in requests.items() if pending is not None), None)
    if missing is not None:
        raise Unsupported(f"{path.name}: call {missing} {trajectory_requests[missing][0]}: missing tool result")
    require(thread is not None, f"{path.name}: raw session does not establish supported tool activity")
    calls.sort(key=lambda call: call[3][1])
    check_claude_precedence(calls, raw_starts, raw_returns, f"{path.name}: raw Claude tool timestamps contradict call order")
    initialized = finished = False
    starts, returns = {}, {}
    for position, event in enumerate(events(transcript)):
        kind, subtype = event.get("type"), event.get("subtype")
        require(not finished and event.get("session_id") == thread, "invalid Claude transcript session or event after completion")
        if not initialized:
            require(kind == "system" and subtype == "init", "missing Claude transcript initialization")
            initialized = True
        elif kind == "result":
            require(subtype == "success" and event.get("is_error") is False, "missing successful Claude completion")
            finished = True
        elif kind in {"assistant", "user"}:
            proof = {call_id: claude_tool_proof(request, block)
                     for call_id, request, block in claude_blocks(event, transcript.name)}
            require("tool_use_result" not in event or len(proof) == 1 and next(iter(proof.values()))[0] == "tool_result",
                    "Claude transcript result has no unique tool event")
            if proof:
                raw = transcript_calls_by_event.pop(event["uuid"], None) if isinstance(event.get("uuid"), str) else None
                require(event.get("parent_tool_use_id") is None and raw is not None and same_json(raw[0], proof)
                        and ("timestamp" not in event or timestamp(event["timestamp"])[1] == timestamp(raw[1])[1])
                        and ("tool_use_result" not in event or same_json(event["tool_use_result"], raw[2])),
                        "raw tool event differs from Claude transcript")
                for call_id, (kind, _) in proof.items():
                    if kind == "tool_use":
                        starts[call_id] = position
                    else:
                        require(call_id in starts, f"call {call_id}: Claude transcript returns before request")
                        if call_id in edit_rejections:
                            require("tool_use_result" in event and "timestamp" in event,
                                    f"call {call_id}: missing Claude Edit result wrapper or timestamp")
                        returns[call_id] = position
        else:
            require(kind == "system" and subtype in {"task_started", "task_notification"}, "unsupported Claude transcript event")
    require(finished, "missing successful Claude completion")
    require(not transcript_calls_by_event, "raw tool event differs from Claude transcript")
    check_trajectory(transcript.with_name("trajectory.json"), thread, trajectory_requests, edit_rejections)
    check_claude_precedence(calls, starts, returns, "Claude transcript contradicts raw call precedence")
    # Equal timestamps can mask opposite event order. Compare the matched intervals directly.
    check_precedence([(starts[call_id], returns[call_id], raw_starts[call_id], raw_returns[call_id], call_id)
                      for _, call_id, _, _ in calls], "Claude transcript and raw session", allow_equal=True)
    return thread, calls


def check_raw_bash_result(event, block, item, output, sandbox_override):
    require(raw_bash_output(event.get("toolUseResult"), sandbox_override) == output and block.get("content") == output
            and block.get("is_error") is (item.get("status") == "failed"), "output or result status differs from rollout")


def timestamp(value):
    parts = UTC.fullmatch(value) if isinstance(value, str) else None
    require(parts, "missing or invalid UTC timestamp")
    second, fraction = parts.groups()
    try:
        time = datetime.fromisoformat(second)
    except ValueError:
        raise Unsupported("invalid UTC timestamp") from None
    delta = time - datetime.min
    nanos = (delta.days * SECONDS_PER_DAY + delta.seconds) * NANOSECONDS_PER_SECOND
    nanos += int((fraction or "").ljust(NANOSECOND_DIGITS, "0"))
    return second + ("." + fraction if fraction else "") + "Z", nanos


def job_time(result, field):
    value = result.get(field)
    message = f"run result has missing or invalid {field}"
    require(isinstance(value, str) and JOB_TIME.fullmatch(value), message)
    if value.endswith("Z"):
        value = value.removesuffix("Z") + "+00:00"
    try:
        return datetime.fromisoformat(value)
    except ValueError:
        raise Unsupported(message) from None


def check_job_counts(result, directory_count):
    total = result.get("n_total_trials")
    require(type(total) is int and total == directory_count, "trial directory count differs from run result")
    stats = result.get("stats")
    require(isinstance(stats, dict), "run result has no trial counts")
    # Harbor counts every terminal result as completed. Errors are a subset.
    completed = [stats[key] for key in ("n_completed_trials", "n_trials") if key in stats]
    require(completed and all(type(count) is int and count == total for count in completed),
            "completed trial count differs from run total")
    for key in ("n_pending_trials", "n_running_trials"):
        count = stats.get(key, 0)
        require(type(count) is int and count == 0, f"unfinished trial count in {key}")
    for key in ("n_errored_trials", "n_errors", "n_cancelled_trials"):
        count = stats.get(key, 0)
        require(type(count) is int and 0 <= count <= total, f"invalid trial count in {key}")
    require(not {"n_errored_trials", "n_errors"} <= stats.keys() or stats["n_errored_trials"] == stats["n_errors"],
            "errored trial counts differ")


def digest(data):
    return hashlib.sha256(data).hexdigest()


ENCODER = json.JSONEncoder(ensure_ascii=False, allow_nan=False, separators=(",", ":"))


def encoded(data):
    return (ENCODER.encode(data) + "\n").encode("utf-8")


def transcript_calls(path):
    thread = None
    calls = None
    pending = {}
    seen = set()
    turns = []
    for position, event in enumerate(events(path, transcript=True)):
        kind = event.get("type")
        if kind == "thread.started":
            require(thread is None and isinstance(event.get("thread_id"), str) and event["thread_id"],
                    "codex.txt: missing or duplicate thread ID")
            thread = event["thread_id"]
        elif kind == "turn.started":
            require(thread and calls is None, "codex.txt: unsupported turn start")
            calls = []
        elif kind == "turn.completed":
            require(calls is not None, "codex.txt: turn completion has no start")
            if pending:
                raise Unsupported(f"codex.txt item {next(iter(pending))}: missing completion")
            turns.append(calls)
            calls = None
        elif kind in {"item.started", "item.updated", "item.completed"}:
            item = event["item"]
            if item["type"] in {"agent_message", "reasoning", "todo_list"}:
                continue
            require(item["type"] in {"command_execution", "file_change"}, "codex.txt: unsupported tool item")
            call_id = item["id"]
            require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id), "codex.txt: invalid item ID")
            require(calls is not None, f"codex.txt item {call_id}: outside active turn")
            if kind == "item.started":
                require(call_id not in seen, f"codex.txt item {call_id}: ambiguous or repeated start")
                seen.add(call_id)
                pending[call_id] = item, position
            else:
                start, started_at = pending.get(call_id, (None, None))
                key = "command" if item["type"] == "command_execution" else "changes"
                require(start and start["type"] == item["type"] and start[key] == item[key],
                        f"codex.txt item {call_id}: request changed or is missing")
                if kind == "item.completed":
                    calls.append((item, started_at, position))
                    del pending[call_id]
        else:
            raise Unsupported("codex.txt: unsupported transcript event")
    require(turns and calls is None, "codex.txt: incomplete trace")
    return thread, turns


def rollout_calls(path, thread):
    source = events(path)
    meta = next(source, {})
    require(meta.get("type") == "session_meta" and meta["payload"].get("id") == thread,
            "rollout: thread does not match codex.txt")
    turn = None
    pending = {}
    seen = set()
    patch_ids = set()
    pending_patches = defaultdict(set)
    turn_ids = set()
    turns = []
    last_tool_time = 0

    def tool_time(event):
        nonlocal last_tool_time
        time = timestamp(event.get("timestamp"))
        require(time[1] >= last_tool_time, "tool timestamp reverses rollout event order")
        last_tool_time = time[1]
        return time

    for position, event in enumerate(source):
        kind, value = event["type"], event["payload"]
        name = value.get("type")
        if kind == "event_msg" and name == "task_started":
            turn_id = value.get("turn_id")
            require(turn is None and isinstance(turn_id, str) and turn_id and turn_id not in turn_ids,
                    "rollout: unsupported turn start")
            turn = turn_id
            turn_ids.add(turn)
            calls = []
        elif kind == "event_msg" and name == "task_complete":
            if pending:
                raise Unsupported(f"call {next(iter(pending))}: missing tool return")
            require(turn and value.get("turn_id") == turn, "rollout: invalid turn end")
            turn = None
            turns.append(calls)
        elif kind == "response_item" and name in {"custom_tool_call", "custom_tool_call_output"}:
            call_id = value.get("call_id")
            require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id), "rollout: invalid call ID")
            try:
                require(turn and value.get("turn_id", turn) == turn, "outside active turn")
                time = tool_time(event)
                if name == "custom_tool_call":
                    require(call_id not in seen, "ambiguous or repeated request")
                    require(value.get("name") == "exec", "unsupported Codex tool")
                    require(isinstance(value.get("input"), str), "missing wrapper input")
                    seen.add(call_id)
                    pending[call_id] = (value, time, position, None)
                    if PATCH.fullmatch(value["input"]):
                        pending_patches[patch_input(value["input"])].add(call_id)
                else:
                    require(call_id in pending, "no matching request")
                    request, start, started_at, patch = pending.pop(call_id)
                    require(patch is not None or not PATCH.fullmatch(request["input"]), "missing add-file result")
                    require(start[1] <= time[1], "return is earlier than request")
                    require(patch is None or patch[1] <= time[1], "return is earlier than patch result")
                    calls.append((request, start, value, time, patch[0] if patch else None, started_at, position))
            except Unsupported as error:
                raise Unsupported(f"call {call_id}: {error}") from None
        elif kind == "response_item" and (name not in {"message", "reasoning"} or "call_id" in value):
            unsupported_tool(value)
        elif kind == "event_msg" and name == "patch_apply_end":
            call_id = value.get("call_id")
            require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id) and call_id not in patch_ids,
                    "rollout: invalid or repeated nested patch ID")
            changes = value.get("changes")
            inputs = None
            if isinstance(changes, dict) and len(changes) == 1:
                (path, change), = changes.items()
                if isinstance(change, dict) and change.get("type") == "add" and isinstance(change.get("content"), str):
                    inputs = path, change["content"]
            candidates = pending_patches.get(inputs)
            if not candidates:
                outer_ids = [outer for group in pending_patches.values() for outer in group]
                related = f" (pending call {outer_ids[0]})" if len(outer_ids) == 1 else ""
                raise Unsupported(f"call {call_id}{related}: unmatched add-file result")
            require(len(candidates) == 1, f"call {call_id}: ambiguous add-file result")
            outer_id, = candidates
            request, start, started_at, patch = pending[outer_id]
            try:
                patch_time = tool_time(event)
                # Codex gives the nested patch a separate ID from the wrapper call.
                require(value.get("turn_id") == turn and patch is None and start[1] <= patch_time[1],
                        "ambiguous or unmatched patch result")
            except Unsupported as error:
                raise Unsupported(f"call {outer_id}: {error}") from None
            patch_ids.add(call_id)
            del pending_patches[inputs]
            pending[outer_id] = (request, start, started_at, (value, patch_time[1]))
        elif kind == "event_msg" and (name not in PASSIVE_EVENTS or "call_id" in value):
            unsupported_tool(value)
        elif "call_id" in value:
            unsupported_tool(value)
        elif kind not in {"event_msg", "world_state", "turn_context", "response_item"}:
            raise Unsupported("rollout: unsupported event")
    if pending:
        raise Unsupported(f"call {next(iter(pending))}: missing return")
    require(turns and turn is None, "rollout: incomplete trace")
    return turns


def exec_arguments(text):
    text = text.lstrip()
    require(text.startswith(EXEC_PREFIX), "unsupported wrapper: expected one literal shell command")
    rest = text[len(EXEC_PREFIX):].lstrip()
    args = {}
    while not rest.startswith("}"):
        key = EXEC_KEY.match(rest)
        require(key, "unsupported wrapper argument")
        name = key[1]
        name = DECODER.decode(name) if name.startswith('"') else name
        require(name not in args, "duplicate wrapper argument")
        try:
            args[name], index = DECODER.raw_decode(rest[key.end():])
        except ValueError:
            raise Unsupported("unsupported wrapper argument: expected literal value") from None
        rest = rest[key.end() + index:].lstrip()
        require(rest.startswith((",", "}")), "unsupported wrapper argument: expected separator")
        rest = rest.removeprefix(",").lstrip()
    require(args.keys() <= {"cmd", "workdir", "login", "yield_time_ms", "max_output_tokens"},
            "unsupported shell argument")
    returned = EXEC_RETURN.match(rest)
    require(returned, "unsupported wrapper return")
    wait = rest[returned.end():]
    require(not wait or WAIT.fullmatch(wait), "unsupported wait or session reuse")
    return args, bool(wait)


def output_text(response, *, wait=False):
    output = response.get("output")
    require(isinstance(output, list) and len(output) >= 2, "incomplete Codex return")
    require(len(output) == 2 or wait and len(output) == 3, "unexpected Codex output chunks for wrapper wait")
    require(all(isinstance(part, dict) and part.get("type") == "input_text"
                and isinstance(part.get("text"), str) for part in output), "unsupported Codex return")
    require(OUTPUT_HEADER.fullmatch(output[0]["text"]), "return does not confirm script completion")
    return "".join(part["text"] for part in output[1:])


def shell_argv(item):
    try:
        argv = shlex.split(item["command"])
    except ValueError:
        raise Unsupported("invalid transcript shell quoting") from None
    if argv[:2] == [BASH, "-lc"]:
        argv[1:2] = ["-l", "-c"]
    return argv


def check_workdir(workdir):
    require(isinstance(workdir, str) and (workdir == "/" or (
        re.fullmatch(r"(?:/[a-zA-Z0-9._-]+)+", workdir)
        and not {".", ".."}.intersection(workdir.split("/"))
    )), "missing or invalid workdir")


def exec_record(item, args, output, argv):
    script, workdir = args.get("cmd"), args.get("workdir")
    require(isinstance(script, str) and "\0" not in script, "missing or invalid shell script")
    check_workdir(workdir)
    require(all(type(args[key]) is int for key in ("yield_time_ms", "max_output_tokens") if key in args),
            "invalid shell capture or wait limit")
    require(argv is not None, "missing corroborating raw Bash request")
    require(argv[-1] == script, "shell command differs from codex.txt")
    login = argv[1] == "-l"
    require(args.get("login", login) is login, "shell login flag differs from codex.txt")
    code = item.get("exit_code")
    require(item.get("status") in {"completed", "failed"} and (
        code is None or type(code) is int and 0 <= code <= MAX_EXIT_CODE
        and item["status"] == ("completed" if code == 0 else "failed")),
            "missing or invalid transcript exit result")
    require(item.get("aggregated_output") == output, "shell output differs from codex.txt")
    record = dict(operation_class="exec", path=BASH, command=script, argv=argv, workdir=workdir,
                  command_hash=digest(script.encode()), stdin="", stdin_bytes=0, harbor_output=output)
    if code is not None:
        record["exit_code"] = code
    return record


def patch_input(wrapper):
    match = PATCH.fullmatch(wrapper)
    require(match, "unsupported patch wrapper")
    lines = DECODER.decode(match[1]).removesuffix("\n").split("\n")
    add = "*** Add File: "
    require(len(lines) >= 3 and lines[0] == "*** Begin Patch" and lines[-1] == "*** End Patch"
            and lines[1].startswith(add) and all(line.startswith("+") for line in lines[2:-1]),
            "expected one add-file patch")
    path = lines[1][len(add):]
    content = "".join(line[1:] + "\n" for line in lines[2:-1])
    require(path.startswith("/") and "\0" not in path, "missing or invalid add-file path")
    return path, content


def patch_record(item, inputs, output, patch):
    path, content = inputs
    require(item.get("status") == "completed" and item.get("changes") == [{"path": path, "kind": "add"}],
            "add-file request or result differs from codex.txt")
    require(patch and patch.get("success") is True and patch.get("status") == "completed" and output == "{}",
            "missing successful add-file result")
    require(patch.get("changes") == {path: {"type": "add", "content": content}},
            "add-file content or path differs from patch result")
    body = content.encode()
    return dict(operation_class="write", path=path, harbor_patch="add", stdin=content,
                stdin_bytes=len(body), sha256=digest(body))


def check_trajectory(path, thread, expected, edit_rejections=()):
    if not os.path.lexists(path):
        return
    document = load_json(path)
    require(document.get("schema_version") == "ATIF-v1.7" and document.get("session_id") == thread,
            "trajectory.json: unsupported format or session mismatch")
    observed = {}
    for step in document["steps"]:
        results = (step.get("observation") or {}).get("results", [])
        returned = [value.get("source_call_id") for value in results]
        for call in step.get("tool_calls", []):
            call_id = call.get("tool_call_id")
            require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id), "trajectory.json: invalid call ID")
            require(call_id not in observed and (call.get("function_name") in {"exec", "Bash"} or call_id in edit_rejections),
                    f"call {call_id}: ambiguous or unsupported trajectory request")
            require(returned.count(call_id) == 1, f"call {call_id}: missing or ambiguous trajectory observation")
            returned.remove(call_id)
            arguments = call["arguments"]
            name = call["function_name"]
            observed[call_id] = (name, arguments["input"] if name == "exec" else arguments)
            if call_id in edit_rejections:
                result = next(value for value in results if value.get("source_call_id") == call_id)
                extra = result.get("extra")
                require(isinstance(extra, dict) and extra.get("tool_result_is_error") is True
                        and result.get("content") in (EDIT_UNREAD_DISPLAY,
                            EDIT_UNREAD_DISPLAY + "\n\n[error] tool reported failure"),
                        f"call {call_id}: trajectory Edit rejection differs")
        require(not returned, "trajectory.json: observation has no request")
    for call_id, (name, arguments) in expected.items():
        actual = observed.pop(call_id, None)
        require(actual is not None and actual[0] == name and same_json(actual[1], arguments),
                f"call {call_id}: trajectory request differs")
    require(not observed, "trajectory.json: request missing from source trace")


def item_keys(item, call_groups, literal_keys, raw_errors):
    if item["type"] == "command_execution":
        output = item.get("aggregated_output")
        require(isinstance(output, str), "missing transcript shell output")
        literal = ("exec", item["command"], output)
        try:
            argv = shell_argv(item)
            script = argv[-1] if argv else None
            require(argv in ([BASH, "-c", script], [BASH, "-l", "-c", script]), "unsupported transcript shell argv")
        except Unsupported:
            if literal in call_groups:
                require_literal(literal, call_groups, literal_keys, raw_errors)
                return [(literal, None, None)]
            raise
        canonical = ("exec", script, output)
        keys = [(canonical, argv, argv[1] == "-l")]
        if literal in call_groups and canonical not in call_groups:
            require_literal(literal, call_groups, literal_keys, raw_errors)
            return [(literal, None, None)]
        if literal in literal_keys:
            keys.append((literal, None, None))
        return keys
    change, = item["changes"]
    require(isinstance(change.get("path"), str), "invalid transcript patch path")
    return [(("patch", change["path"]), None, None)]


def require_literal(key, call_groups, literal_keys, raw_errors):
    call_id = call_groups[key][0][2][0]["call_id"]
    require(key in literal_keys, f"call {call_id}: {raw_errors[call_id]}")


def call_input(request, response):
    wrapper = request["input"]
    if PATCH.fullmatch(wrapper):
        inputs = patch_input(wrapper)
        return ("patch", inputs[0]), inputs, output_text(response)
    inputs, wait = exec_arguments(wrapper)
    require("login" not in inputs or type(inputs["login"]) is bool, "invalid shell login flag")
    script = inputs.get("cmd")
    require(isinstance(script, str), "missing or invalid shell script")
    output = output_text(response, wait=wait)
    return ("exec", script, output), inputs, output


def join_calls(items, calls, raw_requests):
    """Use exact request and output evidence, with order only for sequential duplicates."""
    call_groups = defaultdict(list)
    raw_errors = {}
    literal_keys = set()
    for evidence in calls:
        request, _, response, _, _, start, end = evidence
        call_id = request["call_id"]
        try:
            key, inputs, output = call_input(request, response)
            call_groups[key].append((start, end, evidence, inputs, output))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "incomplete or invalid call fields"
            raise Unsupported(f"call {call_id}: {reason}") from None
        raw = raw_requests.get(call_id)
        if key[0] == "exec":
            reason = "missing corroborating raw Bash request"
            if raw is not None:
                try:
                    correlate_raw_bash_request(*raw, inputs, evidence[1])
                    reason = None
                    literal_keys.add(key)
                except Unsupported as error:
                    reason = str(error)
            raw_errors[call_id] = reason
    transcript = []
    for item, start, end in items:
        try:
            transcript.append((start, end, item, item_keys(item, call_groups, literal_keys, raw_errors)))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "invalid transcript evidence"
            raise Unsupported(f"codex.txt item {item['id']}: {reason}") from None
    pairs = []
    intervals = []
    if any(len(keys) > 1 for _, _, _, keys in transcript):
        canonical = Counter(keys[0][0] for _, _, _, keys in transcript
                            if keys[0][0][0] != "exec" or keys[0][1] is not None)
        # A complete canonical join must pass validation without a literal retry.
        if sum(canonical.values()) == len(transcript) and canonical == {
            key: len(group) for key, group in call_groups.items()
        }:
            transcript = [(start, end, item, keys[:1]) for start, end, item, keys in transcript]
    if any(len(keys) > 1 for _, _, _, keys in transcript):
        ordered = sorted((start, end, key, evidence, inputs, output)
                         for key, group in call_groups.items() for start, end, evidence, inputs, output in group)
        require(len(ordered) == len(transcript), "codex.txt and rollout have different tool call counts")
        # Strict precedence fixes which calls belong to each common serial block.
        for calls, items in serial_blocks(ordered, sorted(transcript)):
            block_groups = defaultdict(list)
            for start, end, key, evidence, inputs, output in calls:
                block_groups[key].append((start, end, evidence, inputs, output))
            join_groups(block_groups, items, pairs, intervals, raw_errors)
    else:
        join_groups(call_groups, transcript, pairs, intervals, raw_errors)
    check_precedence(intervals)
    return sorted(pairs)


def join_groups(call_groups, transcript, pairs, intervals, raw_errors):
    item_groups = defaultdict(list)
    literal_keys = {key for key, group in call_groups.items() if key[0] == "exec"
                    and any(raw_errors[call[2][0]["call_id"]] is None for call in group)}
    for start, end, item, keys in transcript:
        matching = [candidate for candidate in keys if candidate[0] in call_groups and (
            candidate[0][0] != "exec" or candidate[1] is not None or candidate[0] in literal_keys
        )]
        require(len(matching) < 2, f"codex.txt item {item['id']}: ambiguous launcher or literal script")
        key, argv, login = (matching or keys)[0]
        item_groups[key].append((start, end, item, argv, login))
    for key, group in call_groups.items():
        candidates = item_groups.pop(key, [])
        call_id = group[0][2][0]["call_id"]
        require(len(group) == len(candidates), f"call {call_id}: missing matching codex.txt command or output")
        group.sort()
        candidates.sort()
        for calls, items in serial_blocks(group, candidates):
            for (start, end, evidence, inputs, output), (item_start, item_end, item, argv, _) in pair_group(calls, items):
                if key[0] == "exec" and argv is None:
                    call_id = evidence[0]["call_id"]
                    require(raw_errors[call_id] is None, f"call {call_id}: {raw_errors[call_id]}")
                pairs.append((evidence[-1], item, evidence, inputs, output, argv))
                intervals.append((item_start, item_end, start, end, evidence[0]["call_id"]))
    if item_groups:
        item = next(iter(item_groups.values()))[0][2]
        raise Unsupported(f"codex.txt item {item['id']}: missing matching rollout request")


def serial_blocks(calls, items):
    # Crossing a common serial boundary in one direction forces a match in
    # the other direction, which would reverse strict precedence.
    start = 0
    call_end = item_end = -1
    for index, (call, item) in enumerate(zip(calls, items), 1):
        call_end = max(call_end, call[1])
        item_end = max(item_end, item[1])
        if index == len(calls) or (call_end < calls[index][0] and item_end < items[index][0]):
            yield calls[start:index], items[start:index]
            start = index


def sequential(group):
    return all(left[1] < right[0] for left, right in zip(group, group[1:]))


def pair_group(group, candidates):
    if sequential(group) and sequential(candidates) and all(
        item[-1] is None or not isinstance(call[3], dict) or call[3].get("login", item[-1]) is item[-1]
        for call, item in zip(group, candidates)
    ):
        return list(zip(group, candidates))
    if isinstance(group[0][3], dict) and any(item[-1] is None for item in candidates):
        call_id = group[0][2][0]["call_id"]
        raise Unsupported(f"call {call_id}: ambiguous join with script-only transcript evidence")
    # An omitted login flag can match either argv. Assign explicit flags first
    # without guessing which candidate belongs to an unspecified flag.
    explicit = defaultdict(list)
    unspecified = []
    for call in group:
        inputs = call[3]
        if isinstance(inputs, dict) and "login" in inputs:
            explicit[inputs["login"]].append(call)
        else:
            unspecified.append(call)
    pairs = []
    for login, calls in explicit.items():
        eligible = [item for item in candidates if item[-1] is login]
        call_id = calls[0][2][0]["call_id"]
        if len(calls) > len(eligible):
            ids = ", ".join(call[2][0]["call_id"] for call in calls)
            raise Unsupported(f"calls {ids}: login flag has insufficient matching codex.txt argv evidence")
        require(len(calls) == len(eligible) and sequential(calls) and sequential(eligible),
                f"call {call_id}: ambiguous join for overlapping calls with identical login evidence")
        pairs.extend(zip(calls, eligible))
    remaining = [item for item in candidates if item[-1] not in explicit]
    if unspecified:
        call_id = unspecified[0][2][0]["call_id"]
        require(sequential(unspecified) and sequential(remaining),
                f"call {call_id}: ambiguous join for overlapping calls with identical evidence")
        pairs.extend(zip(unspecified, remaining))
    return pairs


def check_precedence(intervals, sources="transcript and rollout", *, allow_equal=False):
    # Of the calls already finished in the first source, only the latest
    # start in the second source can contradict the current call's return.
    completed = iter(sorted(intervals, key=lambda interval: interval[1]))
    earlier = next(completed, None)
    latest = (-1, "")
    for item_start, _, _, end, call_id in sorted(intervals):
        while earlier and earlier[1] < item_start:
            latest = max(latest, (earlier[2], earlier[4]))
            earlier = next(completed, None)
        require(latest[0] < end or allow_equal and latest[0] == end,
                f"calls {latest[1]} and {call_id}: contradictory {sources} precedence")


def recorded_grades(result):
    verifier = result.get("verifier_result")
    if verifier is None:
        return {}
    require(isinstance(verifier, dict), "invalid verifier result")
    rewards = verifier.get("rewards")
    if rewards is None:
        return {}
    require(isinstance(rewards, dict), "invalid verifier rewards")
    grades = {}
    for field in ("score", "reward"):
        if field in rewards:
            value = rewards[field]
            require(type(value) in {int, Decimal}, f"invalid verifier {field}")
            try:
                rounded = float(value)
            except OverflowError:
                raise Unsupported(f"invalid verifier {field}") from None
            require(math.isfinite(rounded), f"invalid verifier {field}")
            require(value == 0 or rounded != 0, f"verifier {field} is too small")
            grades[field] = rounded
    return grades


def go_json_size(value):
    # Go's json.Marshal escapes these five code points even inside strings.
    return sum(len(chunk.encode()) + sum(chunk.count(character) * extra for character, extra in GO_JSON_ESCAPES.items())
               for chunk in ENCODER.iterencode(value))


def staged_call_size(record, sequence, call_id, offset, duration):
    call = dict(sequence=sequence, tool_call_id=call_id, start_offset=offset)
    if record["operation_class"] == "exec":
        call.update(operation="Exec", workdir=record["workdir"])
        if "harbor_output" in record:
            call["harbor_output"] = record["harbor_output"]
        if "harbor_tool_error" in record:
            call["harbor_tool_error"] = record["harbor_tool_error"]
        if record["command"]:
            call["script"] = record["command"]
        if "-l" in record["argv"][:-1]:
            call["login"] = True
        if "exit_code" in record:
            call["exit_code"] = record["exit_code"]
    elif record["operation_class"] == "source_rejection":
        call.update(operation=record["operation"], source_rejection=dict(reason=record["rejection_reason"], duration=duration))
    else:
        call.update(operation="WriteFile", path=record["path"])
        if record["stdin_bytes"]:
            # JSON quotes plus the padded base64 length for Go's []byte.
            padded = 4 * ((record["stdin_bytes"] + 2) // 3)
            return go_json_size(dict(call, body="")) + padded
    return go_json_size(call)


def staged_plan_size(run, occurrence, package, grades, final_wait, count, call_bytes):
    evaluation = dict(status="succeeded" if grades else "unavailable", duration=0)
    # Python and Go use different shortest float spellings. Reserve the maximum
    # finite float64 spelling per recorded grade instead of encoding it twice.
    max_grade_bytes = len("-0.0000010000000000000002")
    evaluation.update(dict.fromkeys(grades, 0))
    plan = dict(calls=[] if count else None, final_wait=final_wait, source_run_id=run,
                source_task_id=occurrence, origin_type="harbor", evaluation=evaluation)
    if package is not None:
        plan["task_package_digest"] = package
    return go_json_size(plan) + len(grades) * (max_grade_bytes - len("0")) + call_bytes + max(0, count - 1)


def codex_calls(agent, transcript, rollouts, raw_session):
    thread, transcript_turns = transcript_calls(transcript)
    require(rollouts, "agent/sessions/**/rollout-*.jsonl: missing rollout")
    require(len(rollouts) == 1, "agent/sessions/**/rollout-*.jsonl: multiple rollouts")
    rollout_turns = rollout_calls(rollouts[0], thread)
    require(len(transcript_turns) == len(rollout_turns), "codex.txt and rollout have different turn counts")
    calls = [evidence for turn in rollout_turns for evidence in turn]
    raw_requests = raw_bash_requests(raw_session, thread)
    pairs = [pair for items, turn in zip(transcript_turns, rollout_turns) for pair in join_calls(items, turn, raw_requests)]
    normalized, raw_errors = check_raw_session(raw_session, thread, pairs)
    check_trajectory(agent / "trajectory.json", thread,
                     {request["call_id"]: ("exec", request["input"]) for request, *_ in calls})
    records = []
    for _, item, evidence, inputs, output, argv in pairs:
        request, started, _, returned, patch, *_ = evidence
        call_id = request["call_id"]
        try:
            if item["type"] == "command_execution":
                require(patch is None, "unexpected patch result for shell command")
                record = exec_record(item, inputs, output, normalized.get(call_id, argv))
                if call_id in raw_errors:
                    record["harbor_tool_error"] = raw_errors[call_id]
            else:
                record = patch_record(item, inputs, output, patch)
            records.append((dict(record, status="completed"), call_id, started, returned))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "incomplete or invalid call fields"
            raise Unsupported(f"call {call_id}: {reason}") from None
    return thread, records


def convert(trial):
    occurrence = trial.name
    # Go uses a bridge child to distinguish an occurrence from a run root.
    require(occurrence != "bridge", "reserved trial directory name")
    require(occurrence not in {"", ".", "..", "run-result.json"} and "\\" not in occurrence,
            "invalid trial directory name")
    config = load_json(trial / "config.json")
    agent_name = config["agent"]["name"]
    require(isinstance(agent_name, str) and agent_name.strip(), "config.json: invalid agent.name")
    result = load_json(trial / "result.json", json.JSONDecoder(
        object_pairs_hook=unique_fields, parse_float=Decimal))
    task = result["task_id"]["name"]
    run = config["job_id"]
    package = config["task"].get("ref")
    require(config["trial_name"] == result["trial_name"] == occurrence
            and isinstance(run, str) and run and result["config"]["job_id"] == run
            and result["agent_info"]["name"] == agent_name
            and isinstance(task, str) and task and "/" not in task
            and config["task"]["name"] == result["task_name"] == f"terminal-bench/{task}",
            "trial identity differs between config and result")
    require(config["task"]["source"] == result["source"] == TERMINAL_BENCH, "unsupported benchmark source")
    lock = trial / "lock.json"
    if os.path.lexists(lock):
        task_lock = load_json(lock)["task"]
        require(task_lock.get("name") == config["task"]["name"] and task_lock.get("source") == TERMINAL_BENCH
                and task_lock.get("type") == "package", "lock.json: unsupported task package or identity mismatch")
        locked = task_lock.get("digest")
        require(package is None or locked == package, "lock.json: task package digest differs from config")
        package = locked
    for reference in (package, result["task_id"].get("ref")):
        if reference is not None:
            require(isinstance(reference, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", reference),
                    "unsupported task package reference")
            require(package is None or reference == package, "task package digest differs between sources")
            package = reference
    completed = timestamp(result["agent_execution"]["finished_at"])
    agent = trial / "agent"
    transcript = agent / "codex.txt"
    sessions = agent / "sessions"
    raw_session = raw_session_path(sessions)
    rollouts = list(sessions.rglob("rollout-*.jsonl"))
    claude_transcript = agent / "claude-code.txt"
    if os.path.lexists(transcript) or rollouts:
        require(not os.path.lexists(claude_transcript), "ambiguous Codex and Claude tool evidence")
        thread, calls = codex_calls(agent, transcript, rollouts, raw_session)
    else:
        thread, calls = claude_calls(raw_session, claude_transcript, completed)
    first = min((started[1] for _, _, started, _ in calls), default=completed[1])
    latest = max((returned[1] for _, _, _, returned in calls), default=completed[1])
    records = []
    offsets = []
    call_bytes = 0
    for sequence, (record, call_id, started, returned) in enumerate(calls, 1):
        try:
            require(returned[1] <= completed[1], "call return is after agent phase end")
            offset = started[1] - first
            require(offset <= MAX_DURATION_NS, "start offset exceeds Go duration limit")
            duration = returned[1] - started[1]
            require(record["operation_class"] != "source_rejection" or offset + duration <= MAX_DURATION_NS,
                    "rejection return exceeds Go duration limit")
            bound = dict(record, sequence=sequence, run_id=run, task_id=occurrence,
                         sandbox_id=thread, tool_call_id=call_id, started_at=started[0], timestamp=returned[0])
            if record["operation_class"] != "source_rejection":
                bound["stdin_encoding"] = "utf-8"
            line = encoded(bound)
            require(len(line) <= MAX_RECORD_BYTES + len(b"\n"), "call record exceeds ARIES size limit")
            call_bytes += staged_call_size(record, sequence, call_id, offset, duration)
            records.append(line)
            offsets.append((offset, call_id, duration if record["operation_class"] == "source_rejection" else 0))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "incomplete or invalid call fields"
            raise Unsupported(f"call {call_id}: {reason}") from None
    final_wait = completed[1] - latest
    require(final_wait <= MAX_DURATION_NS, "final wait exceeds Go duration limit")
    grades = recorded_grades(result)
    plan_bytes = staged_plan_size(run, occurrence, package, grades, final_wait, len(records), call_bytes)
    partial = any(record["operation_class"] == "exec"
                  and ("exit_code" not in record or "harbor_output" not in record) for record, *_ in calls)
    require(plan_bytes + (SKIP_RESPONSE_CHECK_BYTES if partial else 0) <= MAX_PLAN_BYTES,
            "staged plan exceeds ARIES size limit")
    require(sum(map(len, records)) <= MAX_LOG_BYTES, "call log exceeds ARIES size limit")
    log = b"".join(records)
    entry = {"task_id": occurrence}
    if grades:
        entry["evaluation"] = dict(grades, status="succeeded")
    runtime = dict(schema_version=SOURCE_SCHEMA, replay_ready=True, origin_type="harbor", run_id=run,
                   occurrence_id=occurrence, task_id=task, benchmark_type="terminalbench2", SandboxID=thread,
                   harness_completed_at=completed[0], tool_calls_bytes=len(log), tool_calls_sha256=digest(log))
    if package is not None:
        runtime["task_package_digest"] = package
    metadata = encoded(runtime)
    require(len(metadata) <= MAX_METADATA_BYTES, "metadata exceeds ARIES size limit")
    return run, entry, metadata, log, first if records else None, plan_bytes, offsets, partial


def write_private(path, data):
    with os.fdopen(os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, PRIVATE_FILE), "wb") as file:
        os.fchmod(file.fileno(), PRIVATE_FILE)
        file.write(data)


def publish(output, trials, run=None):
    with tempfile.TemporaryDirectory(prefix=".harbor-", dir=output.parent) as temporary:
        stage = Path(temporary)
        stage.chmod(PRIVATE_DIR)
        entries = []
        for trial in trials:
            try:
                trial_run, entry, runtime, calls, first, plan_bytes, offsets, partial = convert(trial)
                if run is None:
                    run = trial_run
                require(trial_run == run, "trial job ID differs from run result")
            except (ValueError, KeyError, TypeError, AttributeError) as error:
                reason = str(error) if isinstance(error, Unsupported) else "incomplete or invalid Harbor fields"
                raise Unsupported(f"trial {trial.name!r}: {reason}") from None
            bridge = stage / trial.name / "bridge"
            for directory in (bridge.parent, bridge):
                directory.mkdir(mode=PRIVATE_DIR)
                directory.chmod(PRIVATE_DIR)
            write_private(bridge / "runtime.json", runtime)
            write_private(bridge / "tool-calls.jsonl", calls)
            del calls
            entries.append((first, entry, plan_bytes, offsets, partial))
        entries.sort(key=lambda item: (item[0] is None, item[0], item[1]["task_id"]))
        if len(entries) > 1:
            origin = entries[0][0]
            unchecked = any(item[4] for item in entries)
            for first, entry, plan_bytes, offsets, _ in entries:
                trial_label = f"trial {entry['task_id']!r}"
                # Match Go preflight: reserve the shared clock and expand each offset.
                plan_bytes += SKIP_RESPONSE_CHECK_BYTES if unchecked else 0
                plan_bytes += REPLAY_ORIGIN_BYTES if first is not None else 0
                require(plan_bytes <= MAX_PLAN_BYTES, f"{trial_label}: global plan exceeds ARIES size limit")
                for offset, call_id, duration in offsets:
                    shared = first - origin + offset
                    label = f"{trial_label}, call {call_id}"
                    require(shared <= MAX_DURATION_NS, f"{label}: global call offset exceeds Go duration limit")
                    require(shared + duration <= MAX_DURATION_NS, f"{label}: global call return exceeds Go duration limit")
                    plan_bytes += len(str(shared)) - len(str(offset))
                    require(plan_bytes <= MAX_PLAN_BYTES, f"{label}: global plan exceeds ARIES size limit")
        index = encoded(dict(run_id=run, tasks=[item[1] for item in entries]))
        require(len(index) <= MAX_METADATA_BYTES, "run index exceeds ARIES size limit")
        write_private(stage / "run-result.json", index)
        # Linux renameat2 publishes atomically without replacing even an empty directory.
        rename = getattr(ctypes.CDLL(None, use_errno=True), "renameat2", None)
        require(rename is not None, "atomic publication requires Linux renameat2")
        at_fdcwd, rename_noreplace = -100, 1
        if rename(at_fdcwd, os.fsencode(stage), at_fdcwd, os.fsencode(output.absolute()), rename_noreplace):
            error = ctypes.get_errno()
            raise OSError(error, os.strerror(error))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sources = parser.add_mutually_exclusive_group(required=True)
    sources.add_argument("--harbor-trial", type=Path)
    sources.add_argument("--harbor-run", type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    output = args.output_dir
    try:
        require(not os.path.lexists(output), "output directory already exists")
        output_parent = output.parent.resolve(strict=True)
        output = output_parent / output.name
        if args.harbor_trial is not None:
            trial = args.harbor_trial.absolute()
            if output_parent in {trial.parent.resolve(), trial.resolve().parent}:
                parent_result = {}
                with suppress(Unsupported):
                    parent_result = load_json(output_parent / "result.json")
                require("n_total_trials" not in parent_result, "output directory would become a Harbor trial")
            publish(output, [trial])
        else:
            root = args.harbor_run.resolve(strict=True)
            require(output_parent != root, "output directory would become a Harbor trial")
            result = load_json(root / "result.json")
            run = result.get("id")
            require(isinstance(run, str) and run, "invalid run result ID")
            started, finished = (job_time(result, field) for field in ("started_at", "finished_at"))
            require((started.tzinfo is None) == (finished.tzinfo is None), "run timestamps mix local time and offsets")
            require(finished >= started, "run finished_at is before started_at")
            trials = sorted(path for path in root.iterdir() if path.is_dir())
            require(trials, "run has no trial directories")
            check_job_counts(result, len(trials))
            publish(output, trials, run)
    except (Unsupported, OSError) as error:
        reason = str(error) if isinstance(error, Unsupported) else error.strerror
        source = args.harbor_run if args.harbor_run is not None else args.harbor_trial
        print(f"{source.name!r}: {reason}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
