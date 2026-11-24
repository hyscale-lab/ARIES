"""Convert one recorded Harbor Codex trial into a private ARIES replay source."""

import argparse
from collections import defaultdict
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
MAX_DURATION_NS = (1 << 63) - 1
NANOSECONDS_PER_SECOND = 10**9
SECONDS_PER_DAY = 24 * 60 * 60
GO_JSON_ESCAPES = {character: len(f"\\u{ord(character):04x}") - len(character.encode())
                   for character in "<>&\u2028\u2029"}
CALL_ID = re.compile(r"[A-Za-z0-9._:-]{1,128}")  # ARIES tool call ID contract.
UTC = re.compile(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.([0-9]{1,9}))?(?:Z|\+00:00)")
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


def exec_record(item, args, output):
    script, workdir = args.get("cmd"), args.get("workdir")
    require(isinstance(script, str) and "\0" not in script, "missing or invalid shell script")
    require(isinstance(workdir, str) and (workdir == "/" or (
        re.fullmatch(r"(?:/[a-zA-Z0-9._-]+)+", workdir)
        and not {".", ".."}.intersection(workdir.split("/"))
    )), "missing or invalid workdir")
    require(all(type(args[key]) is int for key in ("yield_time_ms", "max_output_tokens") if key in args),
            "invalid shell capture or wait limit")
    argv = shell_argv(item)
    require(argv in ([BASH, "-c", script], [BASH, "-l", "-c", script]),
            "shell command differs from codex.txt")
    login = argv[1] == "-l"
    require(args.get("login", login) is login, "shell login flag differs from codex.txt")
    code = item.get("exit_code")
    require(type(code) is int and 0 <= code <= MAX_EXIT_CODE
            and item.get("status") == ("completed" if code == 0 else "failed"),
            "missing or invalid transcript exit result")
    require(item.get("aggregated_output") == output, "shell output differs from codex.txt")
    return dict(operation_class="exec", path=BASH, command=script, argv=argv, workdir=workdir,
                command_hash=digest(script.encode()), stdin="", stdin_bytes=0, exit_code=code, harbor_output=output)


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


def check_trajectory(path, thread, calls):
    if not os.path.lexists(path):
        return
    document = load_json(path)
    require(document.get("schema_version") == "ATIF-v1.7" and document.get("session_id") == thread,
            "trajectory.json: unsupported format or session mismatch")
    observed = {}
    for step in document["steps"]:
        returned = [value.get("source_call_id") for value in (step.get("observation") or {}).get("results", [])]
        for call in step.get("tool_calls", []):
            call_id = call.get("tool_call_id")
            require(isinstance(call_id, str) and CALL_ID.fullmatch(call_id), "trajectory.json: invalid call ID")
            require(call_id not in observed and call.get("function_name") == "exec",
                    f"call {call_id}: ambiguous or unsupported trajectory request")
            require(returned.count(call_id) == 1, f"call {call_id}: missing or ambiguous trajectory observation")
            returned.remove(call_id)
            observed[call_id] = call["arguments"]["input"]
        require(not returned, "trajectory.json: observation has no request")
    for request, *_ in calls:
        call_id = request["call_id"]
        require(observed.pop(call_id, None) == request["input"], f"call {call_id}: trajectory request differs")
    require(not observed, "trajectory.json: request missing from rollout")


def item_key(item):
    if item["type"] == "command_execution":
        argv = shell_argv(item)
        script, = argv[-1:]
        require(argv in ([BASH, "-c", script], [BASH, "-l", "-c", script]), "unsupported transcript shell argv")
        output = item.get("aggregated_output")
        require(isinstance(output, str), "missing transcript shell output")
        return ("exec", script, output), argv[1] == "-l"
    change, = item["changes"]
    require(isinstance(change.get("path"), str), "invalid transcript patch path")
    return ("patch", change["path"]), None


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


def join_calls(items, calls):
    """Use exact request and output evidence, with order only for sequential duplicates."""
    item_groups = defaultdict(list)
    for item, start, end in items:
        try:
            key, login = item_key(item)
            item_groups[key].append((start, end, item, login))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "invalid transcript evidence"
            raise Unsupported(f"codex.txt item {item['id']}: {reason}") from None
    call_groups = defaultdict(list)
    for evidence in calls:
        request, _, response, _, _, start, end = evidence
        call_id = request["call_id"]
        try:
            key, inputs, output = call_input(request, response)
            call_groups[key].append((start, end, evidence, inputs, output))
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "incomplete or invalid call fields"
            raise Unsupported(f"call {call_id}: {reason}") from None
    pairs = []
    intervals = []
    for key, group in call_groups.items():
        candidates = item_groups.pop(key, [])
        call_id = group[0][2][0]["call_id"]
        require(len(group) == len(candidates), f"call {call_id}: missing matching codex.txt command or output")
        group.sort()
        candidates.sort()
        for calls, items in serial_blocks(group, candidates):
            for (start, end, evidence, inputs, output), (item_start, item_end, item, _) in pair_group(calls, items):
                pairs.append((evidence[-1], item, evidence, inputs, output))
                intervals.append((item_start, item_end, start, end, evidence[0]["call_id"]))
    if item_groups:
        item = next(iter(item_groups.values()))[0][2]
        raise Unsupported(f"codex.txt item {item['id']}: missing matching rollout request")
    check_precedence(intervals)
    return sorted(pairs)


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
        not isinstance(call[3], dict) or call[3].get("login", item[-1]) is item[-1]
        for call, item in zip(group, candidates)
    ):
        return list(zip(group, candidates))
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


def check_precedence(intervals):
    # Of the calls already finished in the transcript, only the latest rollout
    # start can contradict the current call's rollout completion.
    completed = iter(sorted(intervals, key=lambda interval: interval[1]))
    earlier = next(completed, None)
    latest = (-1, "")
    for item_start, _, _, end, call_id in sorted(intervals):
        while earlier and earlier[1] < item_start:
            latest = max(latest, (earlier[2], earlier[4]))
            earlier = next(completed, None)
        require(latest[0] < end, f"calls {latest[1]} and {call_id}: contradictory transcript and rollout precedence")


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


def staged_call_size(record, sequence, call_id, offset):
    call = dict(sequence=sequence, tool_call_id=call_id, start_offset=offset)
    if record["operation_class"] == "exec":
        call.update(operation="Exec", workdir=record["workdir"], harbor_output=record["harbor_output"])
        if record["command"]:
            call["script"] = record["command"]
        if "-l" in record["argv"][:-1]:
            call["login"] = True
        if record["exit_code"]:
            call["exit_code"] = record["exit_code"]
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


def convert(trial):
    occurrence = trial.name
    # Go uses a bridge child to distinguish an occurrence from a run root.
    require(occurrence != "bridge", "reserved trial directory name")
    require(occurrence not in {"", ".", "..", "run-result.json"} and "\\" not in occurrence,
            "invalid trial directory name")
    config = load_json(trial / "config.json")
    agent_name = config["agent"]["name"]
    require(agent_name == "codex", f"unsupported recording agent {agent_name!r} (expected Codex)")
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
    thread, transcript_turns = transcript_calls(agent / "codex.txt")
    rollouts = list((agent / "sessions").rglob("rollout-*.jsonl"))
    require(len(rollouts) == 1, "rollout: absent or ambiguous session trace")
    rollout_turns = rollout_calls(rollouts[0], thread)
    require(len(transcript_turns) == len(rollout_turns), "codex.txt and rollout have different turn counts")
    calls = [evidence for turn in rollout_turns for evidence in turn]
    pairs = [pair for items, turn in zip(transcript_turns, rollout_turns) for pair in join_calls(items, turn)]
    first = min((evidence[1][1] for evidence in calls), default=completed[1])
    latest = max((evidence[3][1] for evidence in calls), default=completed[1])
    records = []
    call_bytes = 0
    for sequence, (_, item, evidence, inputs, output) in enumerate(pairs, 1):
        request, started, _, returned, patch, *_ = evidence
        call_id = request["call_id"]
        try:
            require(returned[1] <= completed[1], "call return is after agent phase end")
            offset = started[1] - first
            require(offset <= MAX_DURATION_NS, "start offset exceeds Go duration limit")
            if item["type"] == "command_execution":
                require(patch is None, "unexpected patch result for shell command")
                record = exec_record(item, inputs, output)
            else:
                record = patch_record(item, inputs, output, patch)
            line = encoded(dict(record, sequence=sequence, run_id=run, task_id=occurrence,
                                sandbox_id=thread, tool_call_id=call_id, status="completed",
                                started_at=started[0], timestamp=returned[0], stdin_encoding="utf-8"))
            require(len(line) <= MAX_RECORD_BYTES + len(b"\n"), "call record exceeds ARIES size limit")
            call_bytes += staged_call_size(record, sequence, call_id, offset)
            records.append(line)
        except (ValueError, KeyError, TypeError, AttributeError) as error:
            reason = str(error) if isinstance(error, Unsupported) else "incomplete or invalid call fields"
            raise Unsupported(f"call {call_id}: {reason}") from None
    check_trajectory(agent / "trajectory.json", thread, calls)
    final_wait = completed[1] - latest
    require(final_wait <= MAX_DURATION_NS, "final wait exceeds Go duration limit")
    grades = recorded_grades(result)
    require(staged_plan_size(run, occurrence, package, grades, final_wait, len(records), call_bytes) <= MAX_PLAN_BYTES,
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
    index, metadata = encoded(dict(run_id=run, tasks=[entry])), encoded(runtime)
    require(max(len(index), len(metadata)) <= MAX_METADATA_BYTES, "metadata exceeds ARIES size limit")
    return index, metadata, log


def publish(output, occurrence, index, runtime, calls):
    with tempfile.TemporaryDirectory(prefix=".harbor-", dir=output.parent) as temporary:
        stage = Path(temporary)
        bridge = stage / occurrence / "bridge"
        for directory in (stage, bridge.parent, bridge):
            if directory != stage:
                directory.mkdir(mode=PRIVATE_DIR)
            directory.chmod(PRIVATE_DIR)
        for path, data in ((stage / "run-result.json", index), (bridge / "runtime.json", runtime),
                           (bridge / "tool-calls.jsonl", calls)):
            with os.fdopen(os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, PRIVATE_FILE), "wb") as file:
                os.fchmod(file.fileno(), PRIVATE_FILE)
                file.write(data)
        # Linux renameat2 publishes atomically without replacing even an empty directory.
        rename = getattr(ctypes.CDLL(None, use_errno=True), "renameat2", None)
        require(rename is not None, "atomic publication requires Linux renameat2")
        at_fdcwd, rename_noreplace = -100, 1
        if rename(at_fdcwd, os.fsencode(stage), at_fdcwd, os.fsencode(output.absolute()), rename_noreplace):
            error = ctypes.get_errno()
            raise OSError(error, os.strerror(error))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--harbor-trial", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    trial, output = args.harbor_trial.absolute(), args.output_dir
    try:
        require(not os.path.lexists(output), "output directory already exists")
        publish(output, trial.name, *convert(trial))
    except (ValueError, KeyError, TypeError, AttributeError, OSError) as error:
        if isinstance(error, Unsupported):
            reason = str(error)
        elif isinstance(error, OSError):
            reason = f"cannot publish output: {error.strerror}"
        else:
            reason = "incomplete or invalid Harbor fields"
        print(f"trial {trial.name!r}: {reason}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
