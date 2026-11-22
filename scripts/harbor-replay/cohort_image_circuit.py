"""Fidelity and attribution checks for reviewed image and circuit trials."""
import hashlib
import json
from pathlib import Path
import re


IMAGE_HELPER = "b06faaf4f96c04e41d6808ee8e38317ea1fd19c68b20693b2045c8fe7e712bc9"
TASKS = {"code-from-image", "circuit-fibsqrt"}


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def boundaries(trajectory):
    """Determine union-only and active-process gaps from original call results."""
    active = set()
    rows = []
    for step in json.loads(Path(trajectory).read_bytes())["steps"]:
        calls = step.get("tool_calls", [])
        operations = [c for c in calls if c["function_name"] in {"exec_command", "view_image", "write_stdin"}]
        if not operations:
            continue
        outputs = {r["source_call_id"]: r for r in step["observation"]["results"] if r.get("source_call_id")}
        before = sorted(active)
        for call in operations:
            content = outputs[call["tool_call_id"]]["content"]
            header = content.split("\nOutput:\n", 1)[0]
            running = re.search(r"(?m)^Process running with session ID (\d+)\r?$", header)
            if running:
                active.add(int(running[1]))
            elif call["function_name"] == "write_stdin":
                active.discard(call["arguments"]["session_id"])
        rows.append({"step": step["step_id"], "guest_calls": len(operations),
                     "page_observation": "union_of_batch" if len(operations) > 1 else "guest_call_window",
                     "background_handles_at_dispatch": before,
                     "background_handles_after_reply": sorted(active),
                     "next_inter_event_interval": "background_execution_possible" if active else "no_recorded_background_handle"})
    if active:
        raise ValueError("unclosed source process handles")
    return rows


def verify(run, receipt, *, sorted_final=False):
    events = [json.loads(line) for line in (run / "events.jsonl").read_bytes().splitlines()]
    if not events or events[-1]["event"] != "removed" or not any(e["event"] == "session_end" for e in events):
        raise ValueError("incomplete native session")
    first = events[0]
    for key in ("task", "trial_id", "job_id", "row_id", "image", "trajectory_sha256", "rollout_sha256", "memory_mib", "storage_mib", "vcpus"):
        if first[key] != receipt[key]:
            raise ValueError(f"run identity mismatch for {key}")
    actual = {e["sequence"]: e for e in events if e["event"] == "tool_reply"}
    expected = {e["sequence"]: e for e in receipt["tools"]}
    if actual.keys() != expected.keys():
        raise ValueError("measured calls differ from validated set")
    images = 0
    for seq, e in actual.items():
        original = expected[seq]
        if (e["step_id"], e["exit_code"], e["expected_exit_code"]) != (original["step_id"], original["exit_code"], original["expected_exit_code"]):
            raise ValueError(f"step or process status mismatch for occurrence {seq}")
        if e.get("operation", "Exec") != original["operation"]:
            raise ValueError(f"operation mismatch for occurrence {seq}")
        if original["operation"] == "ViewImage":
            images += 1
            if e["stdout_sha256"] != original["source_output_sha256"] or e["stdout_sha256"] != e["source_output_sha256"]:
                raise ValueError("guest image bytes differ from the original source")
    if receipt["task"] == "code-from-image":
        helpers = [e["guest_sha256"] for e in events if e["event"] == "guest_prepared" and e["helper"] == "aries-harbor-image"]
        if helpers != [IMAGE_HELPER] or not images:
            raise ValueError("guest codec identity or image call unavailable")
    final, = [e["checks"] for e in events if e["event"] == "final_task_state"]
    reference, = [e["checks"] for e in receipt["source_setup_and_final_state"] if e["event"] == "final_task_state"]
    final_matches = sorted(final.splitlines()) == sorted(reference.splitlines()) if sorted_final else final == reference
    if not final_matches:
        raise ValueError("final input/output hashes differ from validated state")
    return {"trial": receipt["trial_id"], "guest_calls": len(actual), "wire_identical_images": images, "final_checks": final}
