#!/usr/bin/env python3
"""Reconcile stock live replies against the source identities bound in Study 2."""

import argparse
import base64
import gzip
import hashlib
import json
import re
import struct
from collections import Counter
from pathlib import Path


def rows(path):
    opener = path.open if path.exists() else lambda: gzip.open(str(path)+".gz", "rt")
    with opener() as stream:
        return [json.loads(line) for line in stream]


def uncompressed_size(path):
    if path.exists():
        return path.stat().st_size
    with gzip.open(str(path)+".gz", "rb") as data:
        return sum(len(chunk) for chunk in iter(lambda: data.read(1024*1024), b""))


def nextjs_checks(run, live, baseline, replies):
    historical = next(d["checks"] for d in baseline if d["event"] == "final_task_state")
    current = (run / "final-task-state.txt").read_text()
    files = {}
    for expected, actual in zip(historical.splitlines(), current.splitlines(), strict=True):
        match = re.fullmatch(r"[0-9a-f]{64}  (/tmp/warehouse-check/(?:inventory\.png|audit-events\.jsonl))", expected)
        if match:
            digest, name = actual.split("  ")
            assert name == match[1] and re.fullmatch(r"[0-9a-f]{64}", digest)
            files[name] = {"study2_sha256": expected[:64], "live_sha256": digest}
        else:
            assert actual == expected, expected
    assert len(files) == 2
    for phase in ("before-snapshot", "after-restore"):
        health = json.loads((run / "prepare" / f"{phase}-health.json").read_text())
        assert health["warehouse_health"] == {"4100": 200, "4101": 200}
    [view] = [d for d in replies if d["operation"] == "ViewImage"]
    [source_view] = [d for d in baseline if d["event"] == "tool_reply" and d["operation"] == "ViewImage"]
    assert view["exit_code"] == view["expected_exit_code"] == 0
    assert view["source_output_sha256"] == source_view["source_output_sha256"]
    assert hashlib.sha256(view["stdout"].encode()).hexdigest() == view["stdout_sha256"]
    [item] = json.loads(view["stdout"])
    prefix, encoded = item["image_url"].split(",", 1)
    assert (item["type"], item["detail"], prefix) == ("input_image", "high", "data:image/png;base64")
    png = base64.b64decode(encoded, validate=True)
    assert png[:16] == b"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR" and struct.unpack(">II", png[16:24]) == (1280, 720)
    # The selected rollout contains one 1280 x 720 inventory attachment at sequence 40.
    divergences = [d for d in live if d["event"] == "image_attachment_divergence"]
    matched = view["stdout_sha256"] == view["source_output_sha256"]
    assert len(divergences) == (0 if matched else 1)
    if divergences:
        [event] = divergences
        assert event["status_basis"] == "matching_dimensions_and_media_type_different_attachment_bytes"
        assert all(event[k] == view[k] for k in ("source_call_id", "sequence", "stdout_sha256", "source_output_sha256"))
    final = next(d for d in replies if d["sequence"] == 54)
    assert final["step_id"] == 63 and final["exit_code"] == final["expected_exit_code"] == 0
    passed = ["All browser checks passed", "Cold chunk loading and mobile layout checks passed"]
    assert all(message in final["stdout"].splitlines() for message in passed)
    return {"final_files": files, "view_sequence": view["sequence"], "view_exit": view["exit_code"],
            "source_wire_sha256": view["source_output_sha256"], "live_wire_sha256": view["stdout_sha256"],
            "live_png_sha256": hashlib.sha256(png).hexdigest(), "png_dimensions": [1280, 720],
            "attachment_bytes_matched": matched, "final_browser_checks": passed}


def validate(run, reference):
    live = rows(run / "events.jsonl")
    baseline = rows(reference / "events.jsonl")
    config = live[0]
    original = baseline[0]
    assert (
        config["event"] == "replay_configuration"
        and config["benchmark_evaluator_enabled"] is False
    )
    for key in ["trial_id", "trajectory_sha256", "rollout_sha256"]:
        assert config[key] == original[key], key
    source = [d for d in baseline if d["event"] == "tool_reply"]
    replies = [d for d in live if d["event"] == "tool_reply"]
    ids = [d["source_call_id"] for d in sorted(source, key=lambda d: d["sequence"])]
    dispatches = [d for d in live if d["event"] == "tool_dispatch"]
    assert [d["source_call_id"] for d in dispatches] == ids
    assert len(replies) == len(set(ids)) == len(ids)
    starts = {d["source_call_id"]: d["monotonic_ns"] for d in dispatches}
    intervals = sorted(
        (starts[d["source_call_id"]], d["monotonic_ns"]) for d in replies
    )
    union = end = 0
    for begin, reply in intervals:
        assert reply >= begin
        union += max(0, reply - max(end, begin))
        end = max(end, reply)
    measured = next(d for d in live if d["event"] == "live_tool_time")
    assert measured["dispatch_to_first_reply_union_ns"] == union and measured[
        "guest_calls"
    ] == len(source)
    keys = [
        "sequence",
        "step_id",
        "source_call_id",
        "source_process_session_id",
        "source_yield_ms",
        "expected_exit_code",
        "operation",
    ]
    expected = {d["source_call_id"]: d for d in source}
    for d in replies:
        assert all(d.get(k) == expected[d["source_call_id"]].get(k) for k in keys), d[
            "source_call_id"
        ]
    assert any(d["event"] == "session_end" for d in live)
    final = next(
        d["checks"].splitlines()[0]
        for d in baseline
        if d["event"] == "final_task_state"
    )
    actual_output = (run / "output.sha256").read_text().strip()
    output_mode = "sha256"
    if config["task"] in {"freecad-platform-drawing", "freecad-spring-clip"}:
        validation = json.loads((run / "cad-validation.json").read_text())
        expected_sha, expected_file = final.split(maxsplit=1)
        actual_sha, actual_file = actual_output.split(maxsplit=1)
        assert actual_file == expected_file and validation["status"] == "passed"
        assert (
            validation["reference_sha256"] == expected_sha
            and validation["actual_sha256"] == actual_sha
        )
        output_mode = validation["mode"]
    elif config["task"] == "intrastat-meldung":
        checks = (run / "final-task-state.txt").read_text().splitlines()
        historical = next(d["checks"] for d in baseline if d["event"] == "final_task_state").splitlines()
        assert checks[0] == actual_output and checks[-2:] == historical[-2:]
        assert len([c for c in checks if c.startswith("live archive digest ")]) == 4
        output_mode = "four_live_archive_digests_and_stable_reconciliation_fields"
    elif config["task"] == "music-harmony":
        assert actual_output.split(maxsplit=1)[1] == final.split(maxsplit=1)[1] == "/app/harmony.mxl"
        assert all(r["output_comparison"]=="matched_ignoring_trailing_newlines" and r["exit_code"]==0 for r in replies[-2:])
        output_mode = "original_composition_and_reopened_score_checks"
    else:
        assert actual_output == final
    captures = rows(run / "deep/stock-deep/boundaries.jsonl")
    traced = [d for d in live if d["event"] == "trace_capture"]
    assert (
        captures[0]["phase"] == "session-start"
        and captures[-1]["phase"] == "session-end"
    )
    timed_end = next(d["monotonic_ns"] for d in live if d["event"] == "session_end")
    inspection = None
    if len(captures) == len(traced) + 1:
        final_sandbox = (run / "final-sandbox-id").read_text().strip()
        lifecycle = rows(run / "lifecycle.jsonl")
        [restored] = [
            d
            for d in lifecycle
            if d["event"] == "restore_ready" and d["phase"] == "inspection"
        ]
        last = captures[-1]
        generation = (
            run
            / "deep/stock-deep"
            / f"generation-{last['generation']}"
            / "metadata.json"
        )
        assert (
            restored["sandbox_id"] == final_sandbox
            and restored["generation"] == last["generation"]
        )
        assert restored["pid"] == json.loads(generation.read_text())["pid"]
        assert (
            last["triggers"] == ["session-end"]
            and timed_end <= restored["time_ns"] <= last["hook_begin_ns"]
        )
        inspection = {
            key: last[key]
            for key in ("index", "generation", "hook_begin_ns", "hook_end_ns")
        }
        inspection["restore_ready_ns"] = restored["time_ns"]
    else:
        assert len(captures) == len(traced)
    for b, event in zip(captures, traced):
        assert (
            b["index"] == event["capture_index"] and b["triggers"] == event["triggers"]
        )
    for b in captures:
        assert b["status"] == "ok"
        path = run / "deep/stock-deep" / f"generation-{b['generation']}"
        assert uncompressed_size(path / b["pte_file"]) == sum(
            b["pte_code_counts"].values()
        )
        assert uncompressed_size(path / b["dirty_file"]) > 0
    host, guest = rows(run / "host.jsonl"), rows(run / "guest.jsonl")
    assert len(host) > 1 and len(guest) > 1
    live_host = [d for d in host if d["status"] == "live"]
    assert live_host and all(
        d["pss_bytes"] > 0 and d["rss_bytes"] > 0 for d in live_host
    )
    # A running first reply leaves the original guest process alive until a terminal poll.
    checkpoints = [d for d in live if d["event"] == "checkpoint_start"]
    handles = {}
    held = []
    for reply in replies:
        sid = reply.get("source_process_session_id")
        if not sid:
            continue
        if reply["operation"] == "StartProcess" and reply["exit_code"] == -1:
            handles[sid] = reply["monotonic_ns"]
        elif (
            reply["operation"] == "PollProcess"
            and reply["exit_code"] >= 0
            and sid in handles
        ):
            begin = handles.pop(sid)
            end = reply["monotonic_ns"]
            assert not any(begin < c["monotonic_ns"] < end for c in checkpoints), sid
            held.append({"session_id": sid, "running_to_terminal_ns": end - begin})
    divergences = [
        d["sequence"] for d in replies if d["exit_code"] != d["expected_exit_code"]
    ]
    checks = None
    if config["task"] == "wal-recovery-ordering":
        text = "".join(
            d.get("stdout", "")
            for d in replies
            if d.get("source_process_session_id") == 86276
        )
        result = re.search(r"30,000-entry recovery: passed in ([0-9.]+)s", text)
        assert result and text.count(": passed") == 5
        # Trial 23163f44 printed 0.982s in the original poll. Check every other output byte.
        original_poll = text[: result.start(1)] + "0.982" + text[result.end(1) :]
        poll = next(d for d in source if d["operation"] == "PollProcess")
        assert (
            hashlib.sha256(original_poll.encode()).hexdigest()
            == poll["source_output_sha256"]
        )
        checks = {"passed_lines": 5, "recovery_30000_seconds": float(result.group(1))}
    if config["task"] == "payments-pipeline-fix":
        final_text = (run / "final-task-state.txt").read_text()
        reference_text = next(d["checks"] for d in baseline if d["event"] == "final_task_state")
        file_pattern = r"^([0-9a-f]{64})  (\S+)$"
        files = re.findall(file_pattern, final_text, re.MULTILINE)
        assert len(files) == 6 and files == re.findall(file_pattern, reference_text, re.MULTILINE)
        prep = run / "prepare"
        for phase in ("before-snapshot", "after-restore"):
            health = json.loads((prep / f"payments-{phase}-health.json").read_text())
            assert health["customer"] == "ok" and "transaction-worker_00   RUNNING" in health["worker_status"]
            assert health["kafka_watermarks"] == [[0, n] for n in (301720, 309928, 307192, 297160)]
            for role in ("workload", "kafka", "customer"):
                state = json.loads((prep / "payments" / f"{role}-{phase}-state.json").read_text())
                assert state["id"] == f"zb-{role}" and state["status"] == "running"
        ending = next(d for d in live if d["event"] == "session_end_processes")
        original_ending = next(d for d in baseline if d["event"] == "session_end_processes")
        assert ending["recorded_ending_handles"] == original_ending["recorded_ending_handles"]
        assert set(ending["source_process_states"]) == set(original_ending["source_process_states"])
        checks = {
            "source_files_matched": len(files),
            "ending_handle_states": ending["source_process_states"],
            "original_ending_handle_states": original_ending["source_process_states"],
            "integration_logs": {},
        }
        for name, body in re.findall(r"/tmp/worker-integration-(results|final)\.log\n(.*?)(?=/tmp/worker-integration-|\Z)", final_text, re.S):
            latency = re.findall(r"^PASS; max notification latency ([\d.]+)", body, re.M)
            checks["integration_logs"][name] = {
                "pass": bool(latency),
                "max_notification_latency_s": float(latency[-1]) if latency else None,
                "failures": [line for line in body.splitlines() if line.startswith(("Traceback", "AssertionError"))],
            }
        assert set(checks["integration_logs"]) == {"results", "final"}
    if config["task"] == "nextjs-performance":
        checks = nextjs_checks(run, live, baseline, replies)
    if config["task"] == "medical-claims-processing":
        final_checks = (run / "final-task-state.txt").read_text().splitlines()
        reference_checks = next(d["checks"] for d in baseline if d["event"] == "final_task_state").splitlines()
        assert final_checks == reference_checks
        checks = json.loads(final_checks[-1])
        for phase in ("before-snapshot", "after-restore"):
            health = json.loads((run / "prepare" / (phase + "-health.json")).read_text())
            assert health["workspace_cases"] == 21 and health["workspace_decisions"] == 0
            assert health["chromium_workspace_page"] and health["playwright_sse_http"] == 200
            assert health["guest_interfaces"] == ["lo"] and health["uid"] == 65534
            for role in ("workspace", "playwright-mcp"):
                state = json.loads((run / "prepare" / role / (phase + ".json")).read_text())
                assert state["status"] == "running" and state["id"] == "zb-" + role
    return {
        "task": config["task"],
        "trial_id": config["trial_id"],
        "evaluator_enabled": False,
        "source_calls": len(source),
        "live_tool_union_ns": union,
        "first_reply_status_matches": len(source) - len(divergences),
        "divergent_sequences": divergences,
        "handles": [
            {k: d.get(k) for k in (*keys, "exit_code")}
            for d in replies
            if d.get("source_process_session_id")
        ],
        "workload_self_checks": checks,
        "checkpoint_count": len(checkpoints),
        "uninterrupted_handle_intervals": held,
        "diagnostic_events": dict(
            Counter(d["event"] for d in live if d["event"].startswith("diagnostic_"))
        ),
        "final_output": actual_output,
        "reference_output": final,
        "output_mode": output_mode,
        "live_output_comparisons": dict(
            Counter(d["output_comparison"] for d in replies)
        ),
        "captures": len(captures),
        "timed_captures": len(traced),
        "timed_session_end_ns": timed_end,
        "inspection_capture": inspection,
        "capture_triggers": dict(Counter(t for d in captures for t in d["triggers"])),
        "host_samples": len(host),
        "host_status": dict(Counter(d["status"] for d in host)),
        "host_missed_ticks": sum(d.get("missed_intervals_before", 0) for d in host),
        "guest_samples": len(guest),
        "guest_missed_ticks": sum(d.get("missed_ticks", 0) for d in guest),
        "guest_host_ticks_missed": sum(d.get("missed_host_ticks", 0) for d in guest),
        "guest_status": dict(
            Counter(
                d.get("status", "sampled" if "source" in d else "unavailable")
                for d in guest
            )
        ),
        "guest_unsampled": dict(
            Counter(
                d["unavailable"] for d in guest if isinstance(d.get("unavailable"), str)
            )
        ),
        "guest_unavailable": sorted(
            {
                key
                for d in guest
                if isinstance(d.get("unavailable"), dict)
                for key in d["unavailable"]
            }
        ),
    }


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("run", type=Path)
    p.add_argument("reference", type=Path)
    print(json.dumps(validate(**vars(p.parse_args())), indent=2))
