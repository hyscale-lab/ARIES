#!/usr/bin/env python3
"""Summarize retained calibration metadata; never rerun a guest or model."""
import argparse
import collections
import json
import statistics
from pathlib import Path


def summarize(path):
    rows = [json.loads(line) for line in path.read_text().splitlines()]
    cases = [r for r in rows if r.get("event") in ("case", "extended_case")]
    groups = {}
    for name in sorted({r["case"] for r in cases}):
        group = [r for r in cases if r["case"] == name]
        recalls = [r["recall_including_unknown_as_miss"] for r in group
                   if r["recall_including_unknown_as_miss"] is not None]
        groups[name] = {
            "count": len(group), "strict_target_pass_count": sum(r["target_gate_pass"] for r in group),
            "minimum_recall_including_unknown_as_miss": min(recalls) if recalls else None,
            "maximum_false_positive_fraction": max(r["false_positive_fraction"] for r in group),
            "maximum_unknown_target_pages": max(len(r["reading"]["unknown_gpas"]) for r in group),
            "unknown_reasons": dict(collections.Counter(reason for r in group for reason in r["reading"]["unknown_gpas"].values()))}
    full = [{k: v for k, v in r.items() if not k.endswith("_gpas")}
            for r in rows if r.get("event") == "whole_resident_sample"]
    for r in full:
        r["initial_logical_resident_slot_bytes"] = r["initial_resident_pages"] * 4096
        r["observer_maxrss_after_mib"] = r["observer_maxrss_after_kib"] / 1024
    clocks = []
    for cadence in sorted({r["cadence_seconds"] for r in cases}):
        group = [r for r in cases if r["cadence_seconds"] == cadence]
        clocks.append({"wait_seconds": cadence,
                       "median_target_observer_cpu_ms": statistics.median(r["observer_cpu_ns"] for r in group)/1e6,
                       "median_arm_ms": statistics.median(r["reading"]["arm_end_ns"]-r["reading"]["arm_begin_ns"] for r in group)/1e6,
                       "median_scan_ms": statistics.median(r["reading"]["scan_end_ns"]-r["reading"]["scan_begin_ns"] for r in group)/1e6})
    return {"path": str(path), "trace_bytes": path.stat().st_size,
            "target_cases": groups, "target_cost": clocks, "whole_resident_samples": full,
            "machine_configuration": [r for r in rows if r.get("event") == "machine_configuration"],
            "recorded_gates": [r for r in rows if r.get("event") in ("gate", "failure")],
            "caveats": ["Count actual case records; early gate remaining_required lists are not completion evidence",
                         "logical resident slot bytes include shared/zero mappings and are not unique attributable physical bytes",
                         "guest PFNs were measured at range preparation/reuse, not rechecked after every access; unexplained misses are apparent false negatives",
                         "on/off command timings do not include simultaneous whole-observer perturbation",
                         "no workload H1/H2 or safe eviction inference is supported"]}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("paths", nargs="+")
    args = ap.parse_args()
    print(json.dumps({"schema": "aries.tsb.calibration-summary.v1", "runs": [summarize(Path(p)) for p in args.paths]}, indent=2))

if __name__ == "__main__":
    main()
