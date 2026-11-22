"""Summarize the selected call in a separate intra-tool diagnostic run."""

import argparse
from collections import Counter
import json
from pathlib import Path


def records(path):
    with path.open() as stream:
        return [json.loads(line) for line in stream]


def one(rows, predicate):
    matched = [row for row in rows if predicate(row)]
    if len(matched) != 1:
        raise ValueError(f"expected one matching receipt, found {len(matched)}")
    return matched[0]


def summarize(events, boundaries):
    profile = one(events, lambda row: row["event"] == "intra_tool_diagnostic")
    step = profile["step_id"]
    selected = [row for row in events if row.get("step_id") == step]
    dispatch = one(selected, lambda row: row["event"] == "tool_dispatch")["monotonic_ns"]
    result = one(selected, lambda row: row["event"] == "tool_reply")
    reply = result["monotonic_ns"]
    if dispatch >= reply or result["exit_code"] != result["expected_exit_code"]:
        raise ValueError("selected tool lacks a valid live completion")
    if events[-1]["event"] != "removed" or any(row["status"] != "ok" for row in boundaries):
        raise ValueError("replay or trace teardown did not complete")

    tool_bounds = [row for row in boundaries if row["step"] == step]
    captures = [row for row in selected if row["event"] == "intra_tool_capture"]
    if (len(captures) > profile["maximum_captures"]
            or [row["phase"] for row in tool_bounds] !=
            ["pre-tool"] + ["intra-tool"] * len(captures) + ["post-tool"]):
        raise ValueError("selected tool boundaries disagree with the capture budget")
    for index, (capture, boundary) in enumerate(zip(captures, tool_bounds[1:-1]), start=1):
        if (capture["capture_index"] != index
                or not (dispatch <= capture["monotonic_ns"] <= boundary["hook_begin_ns"]
                        <= boundary["hook_capture_and_state_end_ns"] <= capture["capture_end_monotonic_ns"])):
            raise ValueError("capture receipts disagree on timing or order")

    frozen_ns = 0
    previous_resume = dispatch
    for boundary in tool_bounds[1:-1]:
        freeze = boundary["freeze"]
        start, end = freeze["pause_ack_ns"], freeze["resume_request_ns"]
        if previous_resume > start or start > end:
            raise ValueError("invalid or overlapping pause intervals")
        frozen_ns += max(0, min(end, reply) - start)
        previous_resume = freeze["resume_ack_ns"]

    windows = []
    previous_accessed = None
    for boundary in tool_bounds[1:]:
        window = boundary["window"]
        begin, end = window["arm_end_ns"], window["scan_begin_ns"]
        if begin > end:
            raise ValueError("idle window ends before it was armed")
        accessed = set(window["accessed_gpas"])
        sizes = Counter(window["unknown_gpas"].values())
        phase = boundary["phase"]
        active_at_capture = begin < reply and boundary["freeze"]["pause_ack_ns"] < reply
        row = {
            "phase": phase, "begin_ns": begin, "end_ns": end,
            "active_at_pause_ack": active_at_capture,
            "prefix_before_dispatch_ns": max(0, dispatch - begin),
            "suffix_after_live_reply_ns": max(0, end - reply),
            "eligible_bytes": (len(accessed) + len(window["idle_gpas"])) * window["page_bytes"],
            "marked_referenced_bytes": len(accessed) * window["page_bytes"],
            "ambiguous_arrival_bytes": sizes["newly-resident-unarmed"] * window["page_bytes"],
            "unknown_page_counts": dict(sizes),
            "repeated_gpa_bytes": (len(accessed & previous_accessed) * window["page_bytes"]
                                   if previous_accessed is not None else None),
        }
        windows.append(row)
        previous_accessed = accessed
    return {
        "step_id": step, "diagnostic": True,
        "capture_budget": profile["maximum_captures"],
        "capture_interval_ns": profile["interval_ns"],
        "dispatch_ns": dispatch, "reply_ns": reply,
        "tool_wall_ns": reply - dispatch,
        "guaranteed_frozen_tool_ns": frozen_ns,
        "tool_wall_minus_guaranteed_freeze_ns": reply - dispatch - frozen_ns,
        "windows": windows,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("run_directory", type=Path)
    args = parser.parse_args()
    root = args.run_directory
    print(json.dumps(summarize(records(root / "events.jsonl"), records(root / "idle.jsonl")), indent=2))
