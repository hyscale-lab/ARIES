#!/usr/bin/env python3
"""Check the five live Calendar tool receipts against the reviewed OpenClaw trace."""
import hashlib
import json
from pathlib import Path
import re
import sys


def read_jsonl(path):
    return [json.loads(line) for line in Path(path).read_text().splitlines()]


trace_path, replay_path = sys.argv[1:]
assert hashlib.sha256(Path(trace_path).read_bytes()).hexdigest() == (
    "105845c532ae7051e61f44a6c46367e63081d2973cdb5d8d057c3abc85c44180")
trace, replay = map(read_jsonl, (trace_path, replay_path))
messages = [row["message"] for row in trace if "message" in row]
source_calls = [c["id"] for m in messages if m["role"] == "assistant"
                for c in m["content"] if c["type"] == "toolCall"]
source_replies = {m["toolCallId"]: m for m in messages if m["role"] == "toolResult"}
assert len(source_calls) == len(source_replies) == 5

def events(name):
    return [row for row in replay if row["event"] == name]


config, = events("replay_configuration")
assert config["benchmark_evaluator_enabled"] is False
assert config["runtime"] == "runc" and config["container"] == "zb-workload"
dispatch = events("tool_dispatch")
assert [row["source_call_id"] for row in dispatch] == source_calls
replies = {row["source_call_id"]: row for row in events("tool_reply")}
assert len(replies) == len(events("tool_reply")) == 5

def normalize(sequence, text):
    if sequence == 4:
        text = re.sub(r"got 'root@[^'\n]+'", "got '<git identity>'", text)
    if sequence == 5:
        text = re.sub(r"(\[master \(root-commit\) )[0-9a-f]+\]", r"\1<commit>]", text)
    return text.rstrip("\n")


calls = []
for sequence, call in enumerate(dispatch, 1):
    call_id = call["source_call_id"]
    reply = replies[call_id]
    source = source_replies[call_id]
    expected_exit = [0, 0, 0, 128, 0][sequence - 1]
    assert call["sequence"] == reply["sequence"] == sequence
    assert reply["exit_code"] == expected_exit and "native_error" not in reply, reply
    assert "command.started" in reply["guest_process_timing"]["milestones"], reply
    expected = (source["content"][0]["text"] if sequence == 1
                else source["details"]["aggregated"])
    expected = expected.removesuffix(f"\n\n(Command exited with code {expected_exit})")
    observed = reply.get("stdout", "") + reply.get("stderr", "")
    match = "exact_ignoring_trailing_newlines" if observed.rstrip("\n") == expected.rstrip("\n") else "normalized_git_output"
    assert normalize(sequence, observed) == normalize(sequence, expected), (sequence, observed, expected)
    assert reply["monotonic_ns"] >= call["monotonic_ns"]
    calls.append(dict(sequence=sequence, source_call_id=call_id, exit_code=expected_exit,
                      match=match, dispatch_ns=call["monotonic_ns"], reply_ns=reply["monotonic_ns"]))

wait_start = events("wait_start")
wait_end = events("wait_end")
requests = [6_068_000_000, 89_202_000_000, 2_873_000_000, 4_360_000_000]
assert [r["requested_ns"] for r in wait_start] == [r["requested_ns"] for r in wait_end] == requests
waits = [end["monotonic_ns"] - start["monotonic_ns"] for start, end in zip(wait_start, wait_end)]
assert all(observed >= target - 1_000_000 for observed, target in zip(waits, requests))
assert {r["source_time"] for r in dispatch[:3]} == {dispatch[0]["source_time"]}
assert all(r["parallel_size"] == 3 for r in dispatch[:3])
start, = events("session_start")
if start.get("serial_siblings"):
    assert all(calls[i]["dispatch_ns"] >= calls[i - 1]["reply_ns"] for i in (1, 2))
else:
    assert calls[2]["dispatch_ns"] <= min(c["reply_ns"] for c in calls[:3])
end, = events("session_end")
assert start.get("source_playback") is not True
total, covered_until = 0, 0
for call in calls:
    total += max(0, call["reply_ns"] - max(call["dispatch_ns"], covered_until))
    covered_until = max(covered_until, call["reply_ns"])
for tool in events("live_tool_time"):
    assert tool["guest_calls"] == 5 and tool["dispatch_to_first_reply_union_ns"] == total
print(json.dumps(dict(calls=calls, wait_targets_ns=requests, observed_wait_ns=waits,
                      elapsed_ns=end["monotonic_ns"] - start["monotonic_ns"],
                      live_tool_union_ns=total), indent=2))
