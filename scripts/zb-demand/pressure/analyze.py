#!/usr/bin/env python3
"""Summarize an isolated file pressure calibration without publishing addresses."""
from argparse import ArgumentParser
from bisect import bisect_right
import hashlib
import json
from pathlib import Path
from statistics import median


GIB = 1 << 30
NS = 1_000_000_000


def jsonl(path):
    return [json.loads(line) for line in path.read_text().splitlines()]


def named(rows, name):
    result, = [r for r in rows if r["event"] == name]
    return result


def stats_counters(row):
    return row["guest"]["meminfo_kib"], row["guest"]["vmstat"], row["guest"]["damon"]


def integrate(points, begin, end):
    times = [p[0] for p in points]
    if not times[0] <= begin < end <= times[-1]:
        raise ValueError("phase lies outside one-second sample coverage")

    def value(t):
        i = min(bisect_right(times, t)-1, len(times)-2)
        left, x = points[i]
        right, y = points[i+1]
        return x + (y-x)*(t-left)/(right-left)

    segment = [(begin, value(begin)), *[(t, p) for t, p in points if begin < t < end], (end, value(end))]
    integral = sum((t1-t0)*(v0+v1)/2 for (t0, v0), (t1, v1) in zip(segment, segment[1:]))
    return {
        "begin_ns": begin, "end_ns": end, "wall_seconds": (end-begin)/NS,
        "pss_gib_seconds": integral/GIB/NS, "mean_pss_bytes": integral/(end-begin),
        "peak_pss_sampled_bytes": max(v for _, v in segment),
        "interior_sample_count": len(segment)-2,
        "max_covered_segment_ns": max(b-a for (a, _), (b, _) in zip(segment, segment[1:])),
    }


def build(path):
    if (path / "driver.status").read_text().strip() != "0":
        raise ValueError(f"{path.name}: driver did not complete")
    timeline = jsonl(path / "phases.jsonl")
    samples = jsonl(path / "samples.jsonl")
    balloons = jsonl(path / "balloon-metrics.jsonl")
    if len({(r["pid"], r["start_ticks"]) for r in samples}) != 1:
        raise ValueError("host samples must identify one restored VMM")
    fill = named(timeline, "file_fill_and_verify")
    reread = named(timeline, "file_reread")
    if fill["guest"]["sha256"] != reread["guest"]["sha256"]:
        raise ValueError("inconsistent file hash")
    restored = named(timeline, "restored")
    settings = json.loads((path / "runtime.json").read_text())
    if settings.get("root_drive_use_pmem") or restored["machine"]["mem_size_mib"] != 2048:
        raise ValueError("expected matched 2 GiB non-pmem RAM and block-root stack")
    cold_begin = named(timeline, "cold_start")["host_ns"]
    cold_end = named(timeline, "cold_end")["host_ns"]
    origin = fill["host_begin_ns"]
    phase_intervals = {
        "fill_and_verify": (0, fill["host_end_ns"]-origin),
        "cold": (cold_begin-origin, cold_end-origin),
        "first_50_seconds_cold": (cold_begin-origin, cold_begin+50*NS-origin),
        "reread": (reread["host_begin_ns"]-origin, reread["host_end_ns"]-origin),
    }
    pss = [((r["begin_ns"]+r["end_ns"])//2-origin, r["pss_bytes"]) for r in samples]
    if cold_end-cold_begin < 65*NS or cold_end-cold_begin > 70*NS:
        raise ValueError("cold phase outside the planned coverage")
    guest = []
    for row in timeline:
        if row["event"] not in ("pre_fill", "cold_stats", "pre_reread_stats", "post_reread_stats"):
            continue
        mem, vm, damon = stats_counters(row)
        guest.append({
            "stage": row["event"], "t_ns": (row["host_begin_ns"]+row["host_end_ns"])//2-origin,
            "meminfo_kib": mem, "vmstat": vm, "damon": damon,
        })
    early = guest[0]["damon"]
    late = next(r for r in reversed(guest) if r["stage"] == "pre_reread_stats")["damon"]
    counters = {k: int(late[k])-int(early[k]) for k in (
        "nr_reclaim_tried_regions", "nr_reclaimed_regions", "bytes_reclaim_tried_regions",
        "bytes_reclaimed_regions", "nr_quota_exceeds") if k in early and k in late}
    balloon = [{"t_ns": r["monotonic_ns"]-origin, "balloon": r["balloon"]} for r in balloons]
    report_fields = ("free_page_report_count", "free_page_report_freed", "free_page_report_fails")
    balloon_interval_totals = {
        k: sum(row["balloon"][k] for row in balloon)
        for k in report_fields if balloon and all(k in row["balloon"] for row in balloon)
    }
    archive = {name: hashlib.sha256((path / name).read_bytes()).hexdigest() for name in (
        "driver.status", "runtime.json", "phases.jsonl", "samples.jsonl", "host-before.txt",
        "artifact-sha256.txt", "balloon-metrics.jsonl", "balloon-metrics.stderr")}
    return {
        "run": path.name, "variant": restored["variant"], "size": restored["size"],
        "file_size_bytes": fill["guest"]["bytes"], "file_sha256": fill["guest"]["sha256"],
        "machine": restored["machine"], "balloon": restored["balloon"],
        "file_cache_resident_bytes": {
            k: named(timeline, k)["guest"]["file_cache_resident_bytes"] for k in
            ("post_fill_cache", "pre_reread_cache", "post_reread_cache")
        },
        "guest_io_ns": {**fill["guest"], "after_idle_reread_ns": reread["guest"]["guest_read_ns"]},
        "phases": {key: integrate(pss, *bounds) for key, bounds in phase_intervals.items()},
        "damon_counters_before_fill_to_reread": counters,
        "pss_points": pss, "guest_points": guest,
        "balloon_points": balloon, "balloon_metrics_observed": bool(balloon),
        "balloon_report_interval_totals": balloon_interval_totals,
        "raw_input_sha256": archive,
    }


def main():
    parser = ArgumentParser(description=__doc__)
    parser.add_argument("runs", type=Path, nargs="+")
    args = parser.parse_args()
    runs = [build(path) for path in args.runs]
    groups = {}
    for size in ("small", "large"):
        for version in ("native-v2", "v3"):
            chosen = [r for r in runs if r["size"] == size and r["variant"] == version]
            if chosen:
                groups[size+"/"+version] = {
                    "n": len(chosen),
                    "first_50_cold_pss_gib_seconds_median": median(
                        r["phases"]["first_50_seconds_cold"]["pss_gib_seconds"] for r in chosen
                    ),
                    "first_50_cold_mean_pss_bytes_median": median(
                        r["phases"]["first_50_seconds_cold"]["mean_pss_bytes"] for r in chosen
                    ),
                    "pre_reread_file_cache_bytes_median": median(
                        r["file_cache_resident_bytes"]["pre_reread_cache"] for r in chosen
                    ),
                }
    print(json.dumps({"schema": "aries.zb.file-pressure.v1", "groups": groups, "runs": runs}, indent=2))


if __name__ == "__main__":
    main()
