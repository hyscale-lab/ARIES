#!/usr/bin/env python3
"""Breaks microbenchmark runs down into where each task's time and resources went.

    python3 scripts/microbench/analyze.py [.cache/microbench-runs/manifest.tsv]

Reads the manifest scripts/microbench/run.sh writes and, for every task run,
combines the runner's phase timeline (run-result.json), the bridge's tool-call
log, aries-replay's request log and the resource samples. Writes one row per
task run to tasks.csv beside the manifest and prints a Markdown summary of
medians, with the Kubernetes minus Docker difference.

The agent's run is split three ways: model time, the requests aries-replay
served during it; tool time, what the bridge measured executing commands in
the sandbox; and the remainder, the harness's own work plus the transport
between harness, bridge and sandbox. Standard library only, so it runs on the
cluster nodes.
"""

import csv
import datetime
import glob
import json
import os
import statistics
import sys
from collections import defaultdict

PHASES = ["sandbox_start", "sandbox_prepare", "bridge_start", "harness_start",
          "harness_run", "isolation", "evaluation", "cleanup"]


def parse_time(text):
    # Go writes nanoseconds; Python keeps microseconds.
    text = text.replace("Z", "+00:00")
    if "." in text:
        head, rest = text.split(".", 1)
        digits = rest[:len(rest) - len(rest.lstrip("0123456789"))]
        zone = rest[len(digits):]
        text = "%s.%s%s" % (head, digits[:6].ljust(6, "0"), zone)
    return datetime.datetime.fromisoformat(text).timestamp()


def read_jsonl(path):
    if not path or not os.path.exists(path):
        return []
    with open(path) as handle:
        return [json.loads(line) for line in handle if line.strip()]


def harness_tool_calls(task_dir):
    """Tool calls as the harness saw them, from OpenClaw's session log: each
    tool result records how long the call took on the harness's side. The same
    log exists with and without ARIES, so this compares all deployments alike."""
    durations = []
    for path in glob.glob(os.path.join(task_dir, "harness", "telemetry", "*.jsonl")):
        if "trajectory" in os.path.basename(path):
            continue
        for entry in read_jsonl(path):
            message = entry.get("message") or {}
            if message.get("role") == "toolResult":
                duration = (message.get("details") or {}).get("durationMs")
                if isinstance(duration, (int, float)):
                    durations.append(duration)
    return durations


def resource_stats(samples):
    stats = {}
    by_component = defaultdict(list)
    for sample in samples:
        by_component[sample["component"]].append(sample)
    for component, rows in by_component.items():
        cpu = [row["cpu_percent"] for row in rows if row.get("cpu_percent") is not None]
        memory = [row["memory_usage_bytes"] for row in rows]
        mean = statistics.mean(cpu) if cpu else 0.0
        peak = max(cpu) if cpu else 0.0
        stats[component] = {
            "samples": len(rows),
            "cpu_mean": mean,
            "cpu_peak": peak,
            # Burstiness: how far the peak stands above the mean, and how much
            # the load varies from sample to sample.
            "cpu_peak_to_mean": peak / mean if mean > 0 else 0.0,
            "cpu_cov": statistics.pstdev(cpu) / mean if mean > 0 and len(cpu) > 1 else 0.0,
            "memory_peak_mib": max(memory) / 2**20 if memory else 0.0,
        }
    return stats


def task_rows(entry, requests):
    run_dir = entry["run_dir"]
    result_path = os.path.join(run_dir, "run-result.json")
    if not run_dir or not os.path.exists(result_path):
        return []
    with open(result_path) as handle:
        result = json.load(handle)
    rows = []
    for task in result["tasks"]:
        phases = {phase["name"]: phase for phase in task.get("phases", [])}
        row = {key: entry[key] for key in ("harness", "backend", "mode", "repetition")}
        row["task"] = task["task_id"].rsplit("-", 1)[0]
        row["reward"] = task["evaluation"].get("reward")
        row["error"] = task.get("error", "")
        for name in PHASES:
            row[name + "_s"] = phases[name]["duration"] / 1e9 if name in phases else None
        row["task_s"] = task["duration"] / 1e9
        # OpenClaw's own measure of the agent's run, which the native baseline
        # records; its wall-clock harness_run also includes starting the CLI.
        if task.get("agent_internal_ms") is not None:
            row["agent_internal_s"] = task["agent_internal_ms"] / 1000

        calls = read_jsonl(os.path.join(run_dir, task["task_id"], "bridge", "tool-calls.jsonl"))
        row["tool_calls"] = len(calls)
        row["tool_rejected"] = sum(1 for call in calls if call.get("status") != "completed")
        row["tool_exec_s"] = sum(call.get("duration_ms", 0) for call in calls) / 1000
        row["tool_exec_mean_ms"] = statistics.mean(c.get("duration_ms", 0) for c in calls) if calls else None

        model_s = None
        if "harness_run" in phases:
            start = parse_time(phases["harness_run"]["started_at"])
            end = start + phases["harness_run"]["duration"] / 1e9
            chat = [r for r in requests if r["path"].endswith("/chat/completions")
                    and start <= parse_time(r["received_at"]) <= end]
            model_s = sum(r["duration_ms"] for r in chat) / 1000
            row["model_calls"] = len(chat)
            row["model_misses"] = sum(1 for r in chat if r.get("hit") is False)
            row["model_s"] = model_s
            row["harness_other_s"] = phases["harness_run"]["duration"] / 1e9 - model_s - row["tool_exec_s"]
        harness_calls = harness_tool_calls(os.path.join(run_dir, task["task_id"]))
        row["harness_tool_calls"] = len(harness_calls)
        row["harness_tool_s"] = sum(harness_calls) / 1000
        row["harness_tool_mean_ms"] = statistics.mean(harness_calls) if harness_calls else None
        samples = read_jsonl(os.path.join(run_dir, task["task_id"], "monitor", "resources.jsonl"))
        for component, values in resource_stats(samples).items():
            for key, value in values.items():
                row["%s_%s" % (component, key)] = value
        rows.append(row)
    return rows


def median(values):
    values = [v for v in values if isinstance(v, (int, float))]
    return statistics.median(values) if values else None


def main():
    manifest = sys.argv[1] if len(sys.argv) > 1 else ".cache/microbench-runs/manifest.tsv"
    with open(manifest) as handle:
        entries = list(csv.DictReader(handle, delimiter="\t"))
    rows = []
    for entry in entries:
        rows.extend(task_rows(entry, read_jsonl(entry["requests"])))
    if not rows:
        sys.exit("no completed runs in " + manifest)

    columns = []
    for row in rows:
        columns.extend(key for key in row if key not in columns)
    output = os.path.join(os.path.dirname(manifest), "tasks.csv")
    with open(output, "w", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=columns)
        writer.writeheader()
        writer.writerows(rows)

    shown = ["task_s", "sandbox_start_s", "harness_start_s", "harness_run_s", "agent_internal_s", "model_s",
             "tool_exec_s", "harness_other_s", "cleanup_s", "tool_exec_mean_ms",
             "harness_tool_calls", "harness_tool_mean_ms",
             "sandbox_cpu_mean", "sandbox_cpu_peak", "harness_cpu_peak", "harness_memory_peak_mib"]
    groups = defaultdict(list)
    for row in rows:
        if row["mode"] == "replay":
            groups[(row["harness"], row["task"], row["backend"])].append(row)
    print("Medians over replayed runs; times in seconds, CPU in percent of one core.\n")
    for harness in sorted({key[0] for key in groups}):
        for task in sorted({key[1] for key in groups if key[0] == harness}):
            native = groups.get((harness, task, "native"), [])
            docker = groups.get((harness, task, "docker"), [])
            k8s = groups.get((harness, task, "k8s"), [])
            solved = lambda rows: "%d/%d" % (sum(1 for r in rows if r["reward"] == 1), len(rows))
            print("### %s, %s (solved: native %s, ARIES Docker %s, ARIES Kubernetes %s)\n" % (
                harness, task, solved(native), solved(docker), solved(k8s)))
            print("| Measure | Native | ARIES Docker | ARIES Kubernetes | Kubernetes − Docker |")
            print("| --- | --- | --- | --- | --- |")
            fmt = lambda v: "" if v is None else "%.2f" % v
            for column in shown:
                n = median(r.get(column) for r in native)
                d, k = median(r.get(column) for r in docker), median(r.get(column) for r in k8s)
                diff = "" if d is None or k is None else "%+.2f" % (k - d)
                print("| %s | %s | %s | %s | %s |" % (column, fmt(n), fmt(d), fmt(k), diff))
            print()
    print("Per-run rows: " + output)


if __name__ == "__main__":
    main()
