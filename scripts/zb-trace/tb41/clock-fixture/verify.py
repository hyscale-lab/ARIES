"""Validate captured Firecracker timing and print a matched-arm summary."""
from pathlib import Path
import json
import sys

NS = 1_000_000_000
PERIOD = 10 * NS


def union(spans, start, end):
    spans = sorted((max(a, start), min(b, end)) for a, b in spans)
    cursor = start
    total = 0
    for a, b in spans:
        if b > max(a, cursor):
            total += b - max(a, cursor)
            cursor = b
    return total


def verify(path, traced):
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    def events(event):
        return [row for row in rows if row["event"] == event]

    def one(event):
        row, = events(event)
        return row

    def get(event):
        return one(event)["monotonic_ns"]
    meta = one("fixture")
    assert meta["has_evaluator"] is False and meta["traced"] is traced
    dispatch = events("tool_dispatch")
    reply = events("tool_reply")
    assert [r["source_call_id"] for r in dispatch] == [r["source_call_id"] for r in reply] == ["long", "readback"]
    assert all(r["exit_code"] == r["expected_exit_code"] == 0 for r in reply)
    start, end = get("session_start"), get("session_end")
    tool1_start, tool2_start = (r["monotonic_ns"] for r in dispatch)
    tool1_reply = reply[0]["monotonic_ns"]
    assert get("readiness_passed") < get("snapshot_complete") < get("restore_complete") < tool1_start < tool1_reply < tool2_start < end < get("removed")
    wait = one("wait_end")
    assert wait["requested_ns"] == meta["modeled_wait_ns"] == 12 * NS
    assert dispatch[1]["target_runnable_ns"] == wait["target_runnable_ns"]
    captures = events("trace_capture")
    freezes = [(r["pause_ack_monotonic_ns"], r["resume_request_monotonic_ns"]) for r in captures if "pause_ack_monotonic_ns" in r]
    blocked = [(r["monotonic_ns"], r["capture_end_monotonic_ns"]) for r in events("trace_blocked")]
    phases = [r["phase"] for r in captures if "timer" in r["triggers"]]
    assert {"tool", "wait"} <= set(phases)
    timer_intervals = []
    for previous, capture in zip(captures, captures[1:]):
        if capture["triggers"] == ["timer"]:
            a, b = previous["monotonic_ns"], capture["monotonic_ns"]
            interval = b - a - union(freezes, a, b)
            assert PERIOD <= interval < PERIOD + 0.25 * NS
            timer_intervals.append(interval / NS)
    if traced:
        assert len(freezes) == len(captures) == len(events("fixture_capture"))
        assert all(b - a >= 0.18 * NS for a, b in freezes)
    else:
        assert not freezes and not events("fixture_capture")
    exporter = 0
    for row, receipt in zip(events("fixture_capture"), captures):
        times = [
            receipt["monotonic_ns"], row["pause_request_monotonic_ns"], row["pause_ack_monotonic_ns"],
            row["resume_request_monotonic_ns"], row["resume_ack_monotonic_ns"], row["export_start_monotonic_ns"],
            row["export_end_monotonic_ns"], receipt["capture_end_monotonic_ns"],
        ]
        assert times == sorted(times)
        assert row["pause_http_status"] == row["resume_http_status"] == 204
        assert (row["phase"], row["triggers"]) == (receipt["phase"], receipt["triggers"])
        if row["phase"] == "post-tool" and row["step_id"] == 1:
            assert times[0] >= tool1_reply
            exporter = times[6] - times[5]
            assert exporter >= 0.30 * NS and union(blocked, times[5], times[6]) == 0
    wait_end = wait["monotonic_ns"]
    frozen_wait = union(freezes, tool1_reply, wait_end)
    wait_wall = wait_end - tool1_reply
    assert abs(wait_wall - frozen_wait - wait["requested_ns"]) < 0.25 * NS
    drift = wait["actual_runnable_ns"] - wait["target_runnable_ns"]
    assert 0 <= drift < 0.25 * NS
    tool_wall = tool1_reply - tool1_start
    tool_runnable = tool_wall - union(freezes, tool1_start, tool1_reply)
    assert PERIOD < tool_runnable < meta["source_tool_ns"]
    freeze_ns = union(freezes, start, end)
    blocked_ns = union(blocked, start, end)
    combined = union(freezes + blocked, start, end)
    assert combined == union(freezes * 2 + blocked * 2, start, end)
    assert combined == freeze_ns + blocked_ns
    return {
        "source_expected_tool_s": meta["source_tool_ns"] / NS,
        "modeled_wait_s": meta["modeled_wait_ns"] / NS,
        "live_long_tool_wall_s": tool_wall / NS,
        "live_long_tool_runnable_s": tool_runnable / NS,
        "observed_wait_wall_s": wait_wall / NS,
        "observed_wait_freeze_s": frozen_wait / NS,
        "wait_runnable_drift_s": drift / NS,
        "next_dispatch_runnable_drift_s": (dispatch[1]["actual_runnable_ns"] - dispatch[1]["target_runnable_ns"]) / NS,
        "session_wall_s": (end - start) / NS,
        "session_freeze_s": freeze_ns / NS,
        "session_additional_trace_block_s": blocked_ns / NS,
        "session_adjusted_s": (end - start - combined) / NS,
        "snapshot_s": (get("snapshot_complete") - get("snapshot_start")) / NS,
        "restore_s": (get("restore_complete") - get("restore_start")) / NS,
        "hidden_export_s": exporter / NS,
        "captures": len(captures),
        "periodic_phases": phases,
        "periodic_runnable_intervals_s": timer_intervals,
    }


if __name__ == "__main__":
    directory = Path(sys.argv[1])
    light = verify(directory / "light.jsonl", False)
    traced = verify(directory / "traced.jsonl", True)
    print(json.dumps(
        {"light": light, "traced": traced, "adjusted_traced_minus_light_s": traced["session_adjusted_s"] - light["session_adjusted_s"]},
        indent=2,
    ))
