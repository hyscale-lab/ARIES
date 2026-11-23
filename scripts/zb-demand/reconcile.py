#!/usr/bin/env python3
"""Join stock KVM TDP handler events to Firecracker replay observations."""
import argparse
from bisect import bisect_right
from collections import Counter
from dataclasses import dataclass, fields
import json
from pathlib import Path
import re


@dataclass(frozen=True)
class Fault:
    begin_ns: int
    end_ns: int
    pid: int
    tid: int
    vcpu: int
    address: int
    gfn: int
    tdp: int
    prefetch: int
    error_code: int
    ret: int
    hva: int
    lookups: int
    hva_conflicts: int
    lookup_ns: int
    mm_faults: int
    major_flags: int
    file_faults: int
    file_major: int
    swap_faults: int
    swap_major: int
    cow_calls: int
    major_accounted: int
    minor_accounted: int
    io_wait_ns: int


def parse_faults(path, errors_path, *, pid=None, interval=None):
    readiness = termination = None
    faults = []
    latest_end = None
    width = len(fields(Fault))
    with path.open() as stream:
        for line in stream:
            if not line.strip():
                continue
            tag, *payload = line.split()
            if tag == "ready" and len(payload) == 1 and readiness is None and termination is None:
                readiness = int(payload[0])
            elif tag == "fault" and len(payload) == width and readiness is not None and termination is None:
                fault = Fault(*map(int, payload))
                if not readiness <= fault.begin_ns <= fault.end_ns:
                    raise ValueError("fault timestamps fall outside collector lifetime")
                latest_end = max(latest_end or fault.end_ns, fault.end_ns)
                if pid is not None and fault.pid != pid:
                    continue
                if interval is not None and not interval[0] <= fault.begin_ns <= fault.end_ns <= interval[1]:
                    continue
                faults.append(fault)
            elif tag == "end" and len(payload) == 2 and termination is None:
                termination = tuple(map(int, payload))
            else:
                raise ValueError(f"unexpected demand trace record {tag}")
    errors = errors_path.read_text()
    loss = re.search(r"(?im)\blost\b[^\n]*\bevents?\b|(?:^|\n)error:", errors)
    if loss or readiness is None or termination is None or termination[1] != 0:
        raise ValueError("demand trace startup, shutdown or loss check failed")
    if latest_end is not None and latest_end > termination[0]:
        raise ValueError("fault timestamps fall outside collector lifetime")
    faults.sort(key=lambda f: (f.begin_ns, f.end_ns))
    return readiness, termination[0], faults


def by_step(receipts, name):
    result = {}
    for row in receipts:
        if row["event"] == name:
            if row["step_id"] in result:
                raise ValueError(f"duplicate {name} step")
            result[row["step_id"]] = row
    return result


def source_boundary(receipts, dispatch):
    if dispatch is None:
        return None
    matched = [
        r["monotonic_ns"] for r in receipts
        if r["event"] == "source_event" and r.get("step_id") == dispatch["step_id"]
        and r.get("rollout_line") == dispatch.get("rollout_line")
        and r["monotonic_ns"] <= dispatch["monotonic_ns"]
    ]
    if len(matched) > 1:
        raise ValueError("command has more than one availability boundary")
    return matched[0] if matched else None


def path_evidence(f):
    if f.lookup_ns > f.end_ns - f.begin_ns or f.io_wait_ns > f.lookup_ns:
        return "timing-inconsistent"
    if f.swap_major:
        return "swap-major-path"
    if f.file_major:
        return "filemap-major-path"
    if f.major_flags or f.major_accounted:
        return "other-major-path"
    if f.cow_calls:
        return "copy-on-write-path"
    if f.minor_accounted or f.mm_faults:
        return "minor-or-unclassified-mm-fault"
    return "no-mm-fault-evidence"


def contains(ranges, page):
    ix = bisect_right(ranges, (page, float("inf"))) - 1
    return ix >= 0 and page < ranges[ix][1]


def classify(f, slots, absent):
    page = f.address & -4096
    matches = [s for s in slots if s["guest_phys_addr"] <= page <
               s["guest_phys_addr"] + s["memory_size"]]
    if len(matches) != 1 or not f.tdp or f.gfn != page // 4096 or f.prefetch:
        return "outside-guest-ram-or-not-demand", page
    if not contains(absent, page):
        return "not-absent-at-arm", page
    if not f.lookups or f.hva_conflicts:
        return "absent-tdp-without-consistent-hva-proof", page
    slot = matches[0]
    expected = slot["userspace_addr"] + page - slot["guest_phys_addr"]
    if f.hva != expected:
        return "absent-tdp-hva-mismatch", page
    return "matched-absent-at-arm", page


def freeze_upper_ns(boundaries, lo, hi):
    intersections = []
    for boundary in boundaries:
        info = boundary.get("freeze", {})
        start = info.get("pause_request_ns")
        end = info.get("resume_ack_ns", info.get("running_confirmed_ns"))
        if start is None or end is None:
            continue
        left, right = max(lo, start), min(hi, end)
        if left < right:
            intersections.append((left, right))
    total = 0
    cursor = lo
    for left, right in sorted(intersections):
        total += max(0, right-max(left, cursor))
        cursor = max(cursor, right)
    return total


def reconcile(readiness, termination, faults, steps, boundaries, *, on_match=None):
    dispatches = by_step(steps, "tool_dispatch")
    replies = by_step(steps, "tool_reply")
    waits = [(r["monotonic_ns"], other["monotonic_ns"])
             for i, r in enumerate(steps) if r["event"] == "wait_start"
             for other in steps[i+1:i+2]
             if other["event"] == "wait_end" and other.get("rollout_line") == r.get("rollout_line")]
    windows = [r for r in boundaries if r["status"] == "ok" and "window" in r]
    if not windows or len({(w["vmm_pid"], w["vmm_start_ticks"]) for w in windows}) != 1:
        raise ValueError("the idle trace must cover one restored VMM")
    pid, identity = windows[0]["vmm_pid"], windows[0]["vmm_start_ticks"]
    if readiness > min(w["window"]["arm_begin_ns"] for w in windows) or termination < max(
            w["window"]["scan_end_ns"] for w in windows):
        raise ValueError("the demand collector must span every observed idle window")
    owned = [f for f in faults if f.pid == pid]
    counts = Counter()
    matches = []
    record = matches.append if on_match is None else on_match
    idx = 0
    first = set()
    preceding_scan = 0
    for w in sorted(windows, key=lambda x: x["window"]["arm_end_ns"]):
        obs = w["window"]
        begin, end = obs["arm_end_ns"], obs["scan_begin_ns"]
        if begin < preceding_scan or end < begin:
            raise ValueError("observed idle windows overlap or run backwards")
        preceding_scan = end
        ranges = sorted(tuple(x) for x in obs["not_resident_at_arm_gpa_ranges"])
        end_ranges = sorted(tuple(x) for x in obs["not_resident_at_end_gpa_ranges"])
        step = w["step"] if w["window_kind"] == "tool" else None
        dispatch, reply = dispatches.get(step), replies.get(step)
        if step is not None and (dispatch is None or reply is None):
            counts["tool-window-without-complete-replay-boundary"] += 1
            continue
        available = source_boundary(steps, dispatch)
        while idx < len(owned) and owned[idx].begin_ns < begin:
            idx += 1
        while idx < len(owned) and owned[idx].begin_ns < end:
            f = owned[idx]
            idx += 1
            if f.end_ns > end:
                counts["fault-crossed-boundary"] += 1
                continue
            state, page = classify(f, w["ram_slots"], ranges)
            counts[state] += 1
            if state != "matched-absent-at-arm":
                continue
            key = (begin, page)
            first_in_window = key not in first
            first.add(key)
            in_tool = dispatch is not None and dispatch["monotonic_ns"] <= f.begin_ns <= reply["monotonic_ns"]
            in_wait = any(a <= f.begin_ns <= b for a, b in waits)
            scope = "tool" if in_tool else "wait" if in_wait else "between-recorded-boundaries"
            evidence = path_evidence(f)
            counts[f"path:{evidence}"] += 1
            counts[f"scope:{scope}"] += 1
            availability_lead = f.begin_ns-available if in_tool and available is not None else None
            freeze_upper = freeze_upper_ns(boundaries, available, f.begin_ns) if availability_lead is not None else None
            record({
                "step": step, "window_from_step": w["from_step"], "window_kind": w["window_kind"],
                "scope": scope, "first_match_for_page_in_window": first_in_window, "path": evidence,
                "absent_at_end": contains(end_ranges, page),
                "gpa_page": page, "vcpu_id": f.vcpu, "vcpu_tid": f.tid, "handler_result": f.ret,
                "amd_npf_error_code": f.error_code, "begin_ns": f.begin_ns, "end_ns": f.end_ns,
                "handler_elapsed_ns": f.end_ns-f.begin_ns,
                "host_pfn_lookups": f.lookups, "host_pfn_lookup_elapsed_ns": f.lookup_ns,
                "lookup_in_iowait_offcpu_ns": f.io_wait_ns,
                "major_accounted_delta": f.major_accounted, "minor_accounted_delta": f.minor_accounted,
                "mm_fault_calls": f.mm_faults, "filemap_major_flags": f.file_major,
                "swap_major_flags": f.swap_major, "copy_on_write_calls": f.cow_calls,
                "dispatch_to_fault_ns": f.begin_ns-dispatch["monotonic_ns"] if in_tool else None,
                "source_boundary_to_fault_ns": availability_lead,
                "source_boundary_to_fault_minus_max_freeze_ns": (
                    availability_lead-freeze_upper if availability_lead is not None else None
                ),
            })
    return {
        "schema": "aries.zb.kvm-demand.v1", "clock": "host-CLOCK_MONOTONIC",
        "restored_vmm_pid": pid, "vmm_start_ticks": identity,
        "demand_trace_ready_ns": readiness, "demand_trace_end_ns": termination,
        "vmm_faults_total": len(owned), "non_vmm_faults_excluded": len(faults)-len(owned),
        "coverage": dict(counts), "matches": matches,
        "unresolved": [
            "Absent at arm is a VMM pagemap observation and may change before demand.",
            "An AMD TDP fault identifies a demand or permission event, including guest kernel and background work.",
            "A major filemap/swap path and io_schedule time are separate observations.",
            "Time off the vCPU during lookup does not identify recovery bytes, storage service time, or useful-work delay.",
            "Simulated source boundary exposes when the replay made a command available, including hook time before dispatch.",
        ],
    }


def json_lines(path):
    with path.open() as stream:
        return [json.loads(line) for line in stream if line.strip()]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("faults", "bpf_stderr", "replay", "idle"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    ready, end, faults = parse_faults(args.faults, args.bpf_stderr)
    result = reconcile(ready, end, faults, json_lines(args.replay), json_lines(args.idle))
    print(json.dumps(result, separators=(",", ":")))


if __name__ == "__main__":
    main()
