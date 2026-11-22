#!/usr/bin/env python3
"""Verify warm/cold KVM calibration against the captured demand path."""
import argparse
import json
from pathlib import Path

from reconcile import json_lines, parse_faults, path_evidence


def verify(config, faults):
    if len(config) != 2 or {r["mode"] for r in config} != {"warm", "cold"}:
        raise ValueError("expected one warm and one cold calibration")
    output = {}
    for action in config:
        mode = action["mode"]
        cached = action["cache_resident_before"]
        if cached != int(mode == "warm") or action["host_pte_present_before"]:
            raise ValueError(f"{mode} did not establish the expected backing and PTE state")
        if not action["guest_write_observed"]:
            raise ValueError(f"{mode} guest did not complete its write")
        pid = action["pid"]
        touches = [f for f in faults if f.pid == pid and f.tdp and not f.prefetch and f.lookups]
        if not touches or any(f.address // 4096 != action["guest_phys_addr"] // 4096 or
                              f.gfn != f.address // 4096 or f.hva != action["hva"] or f.hva_conflicts
                              for f in touches):
            raise ValueError(f"{mode} disagrees with the independent KVM registration")
        if not any(f.cow_calls for f in touches):
            raise ValueError(f"{mode} lacks copy-on-write evidence")
        major = [f for f in touches if f.file_major and f.major_flags and f.major_accounted]
        if bool(major) != (mode == "cold"):
            raise ValueError(f"{mode} did not show the expected major-fault difference")
        output[mode] = {
            "verified_pte_present_before": action["host_pte_present_before"],
            "verified_cache_resident_before": cached,
            "matched_faults": len(touches),
            "paths": [path_evidence(f) for f in touches],
            "filemap_major": [{
                "handler_ns": f.end_ns-f.begin_ns,
                "lookup_ns": f.lookup_ns,
                "io_wait_ns": f.io_wait_ns,
                "lookup_attempts": f.lookups,
                "major_accounted_delta": f.major_accounted,
            } for f in major],
        }
    return output


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("actions", "events", "stderr"):
        parser.add_argument(name, type=Path)
    args = parser.parse_args()
    _, _, faults = parse_faults(args.events, args.stderr)
    print(json.dumps(verify(json_lines(args.actions), faults), indent=2))
