"""Summarize an actual Harbor fidelity diagnostic, preserving per-step evidence."""
import argparse
import collections
import hashlib
import json
from pathlib import Path


def summarize(log, trajectory, host, task, revision, binary_sha, configuration="v1"):
    events = [json.loads(line) for line in log.read_bytes().splitlines()]
    first = events[0]
    original = trajectory.read_bytes()
    if hashlib.sha256(original).hexdigest() != first["trajectory_sha256"]:
        raise ValueError("ATIF input differs from the native diagnostic")
    rejected_id = "call_zA4CJyQ1w3PXPk6iaDhcWYyN"
    rejected = []
    if first["trial_id"] == "720d990f-65c3-4e3a-8613-c813937dd7d4":
        step, = [s for s in json.loads(original)["steps"] if s["step_id"] == 11]
        result, = step["observation"]["results"]
        if (result["source_call_id"] != rejected_id or hashlib.sha256(result["content"].encode()).hexdigest()
                != "60697f48d70cc5dd5d41067d17ba36871845c0b68266b4cfa8e8d1368ada7e03"):
            raise ValueError("original pre-dispatch rejection differs")
        rejected = [dict(step_id=11, call_id=rejected_id, reason="original harness refused before guest dispatch")]
    expected_steps = [step["step_id"] for step in json.loads(original)["steps"]
                      for call in step.get("tool_calls", [])
                      if call["function_name"] not in {"update_plan", "web_search_call"}
                      and not (rejected and call["tool_call_id"] == rejected_id)]
    external = [{"step_id": s["step_id"], "arguments": t["arguments"], "response_status": "unavailable_in_original_source"}
                for s in json.loads(original)["steps"] for t in s.get("tool_calls", [])
                if t["function_name"] == "web_search_call"]
    dispatch = {}
    waits = collections.Counter()
    lifecycle = {}
    replies = []
    ready = []
    network = []
    external_observed = []
    for e in events:
        kind = e["event"]
        if kind in ("create_start", "snapshot_start", "snapshot_complete", "restore_start",
                    "restore_complete", "session_start", "session_end", "removed"):
            lifecycle[kind] = e["monotonic_ns"]
        elif kind == "validated_guest_network":
            network.append(e)
        elif kind == "source_event" and e.get("source_kind") == "web_search_call":
            external_observed.append({key: e[key] for key in ("step_id", "source_call_id", "rollout_line", "basis")})
        elif kind.startswith("source_") and kind.endswith("_ready"):
            ready.append(e)
        elif kind == "final_task_state":
            ready.append(e)
        elif kind == "wait_end":
            waits[e["basis"]] += e["requested_ns"]
        elif kind == "tool_dispatch":
            dispatch[e["sequence"]] = e
        elif kind == "tool_reply":
            row = {k: e[k] for k in ("step_id", "sequence", "rollout_line", "exit_code",
                   "expected_exit_code", "stdout_sha256", "stderr_sha256", "source_output_sha256",
                   "output_comparison")}
            row["operation"] = e.get("operation", "Exec")
            if "status_basis" in e:
                row["status_basis"] = e["status_basis"]
            row["dispatch_monotonic_ns"] = dispatch[e["sequence"]]["monotonic_ns"]
            row["reply_monotonic_ns"] = e["monotonic_ns"]
            row["duration_ns"] = row["reply_monotonic_ns"] - row["dispatch_monotonic_ns"]
            replies.append(row)

    last = next((e for e in reversed(events) if e["event"] == "tool_reply"), {})
    observed_steps = [row["step_id"] for row in replies]
    full = ("session_end" in lifecycle and "removed" in lifecycle
            and observed_steps == expected_steps and len(dispatch) == len(replies))
    if rejected:
        boundaries = [e.get("source_kind") for e in events if e["event"] == "source_event"
                      and e.get("source_call_id") == rejected_id]
        full = full and boundaries == ["harness_rejection_request", "harness_rejection_reply"]
    exit_match = all(e["exit_code"] == e["expected_exit_code"] for e in replies)
    # Durable outcome and final guest checks appear in the last recorded tool.
    # Keep the final returned streams as evidence of what checks actually ran.
    final_stdout = last.get("stdout", "")
    if last.get("operation") == "ViewImage":
        final_stdout = f"Image attachment of {len(final_stdout)} serialized bytes; digest in tools."
    return {
        "schema": "aries.harbor.replay-validation.v1",
        **{k: first[k] for k in ("row_id", "job_id", "trial_id", "image", "trajectory_sha256",
                                 "rollout_sha256", "memory_mib", "storage_mib", "vcpus")},
        "task": task, "host": host, "configuration": configuration, "adapter_revision": revision,
        "adapter_binary_sha256": binary_sha, "diagnostic_log_sha256": hashlib.sha256(log.read_bytes()).hexdigest(),
        "observer": first["observer"], "session_completed": bool(full),
        "diagnostic_video_images": first.get("diagnostic_video_images",False),
        "all_observed_image_payloads_match": all(r["stdout_sha256"]==r["source_output_sha256"] for r in replies if r["operation"]=="ViewImage"),
        "guest_network_checks": network, "external_completion_events": external_observed,
        "expected_step_ids": expected_steps, "rejected_before_guest": rejected, "external_actions": external, "all_observed_tool_exits_match": exit_match, "lifecycle_monotonic_ns": lifecycle,
        "modeled_wait_ns_by_basis": dict(waits), "source_setup_and_final_state": ready, "tools": replies,
        "final_tool_stdout": final_stdout, "final_tool_stderr": last.get("stderr", ""),
    }


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("log", type=Path)
    p.add_argument("output", type=Path)
    p.add_argument("--trajectory", type=Path, required=True)
    p.add_argument("--host", required=True)
    p.add_argument("--task", required=True)
    p.add_argument("--revision", required=True)
    p.add_argument("--binary-sha256", required=True)
    p.add_argument("--configuration", choices=("v1","v2","v3","v4"), default="v1")
    a = p.parse_args()
    data = summarize(a.log, a.trajectory, a.host, a.task, a.revision, a.binary_sha256, a.configuration)
    with a.output.open("x") as out:
        json.dump(data, out, indent=2)
        out.write("\n")
