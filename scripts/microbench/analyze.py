#!/usr/bin/env python3
"""Breaks microbenchmark runs down into where each task's time and resources went.

    python3 scripts/microbench/analyze.py [.cache/microbench-runs/manifest.tsv]

Reads the manifest scripts/microbench/run.sh writes and, for every task run,
combines the runner's phase timeline (run-result.json), the bridge's tool-call
log, aries-replay's request log and the resource samples. Writes one row per
task run to tasks.csv beside the manifest and prints a Markdown summary of
medians per variant, with the Kubernetes minus Docker difference.

A variant names how harnesses started (parallel, alongside their sandboxes,
or sequential) and whether a warm-up ran first. The cold run is the first
request on a fresh harness and sandbox: cold_latency_s is what a user waits
for, from the first start-up step to its response, and cold_startup_s the part
before the request. With a warm-up, which by default is the task itself, the
warm run is the task after it on the same harness and sandbox, and
warm_latency_s its run alone.

Each run is split three ways: model time, the requests aries-replay served
during it; tool time, what the bridge measured executing commands in the
sandbox; and the remainder, the harness's own work plus the transport between
harness, bridge and sandbox. Standard library only, so it runs on the cluster
nodes.
"""

import csv
import datetime
import glob
import json
import os
import statistics
import sys
from collections import defaultdict

PHASES = ["sandbox_start", "sandbox_prepare", "bridge_register", "harness_prepare", "harness_start",
          "harness_warmup", "harness_run", "isolation", "evaluation", "cleanup"]
# Phases before the first request.
STARTUP = ["sandbox_start", "sandbox_prepare", "bridge_register", "harness_prepare", "harness_start"]


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


def within(text, window):
    """Whether a timestamp falls in the agent's run; no window or no
    timestamp keeps the record."""
    if window is None or not text:
        return True
    return window[0] <= parse_time(text) <= window[1]


def harness_tool_calls(task_dir, window):
    """Tool calls as the harness saw them, from OpenClaw's session log: each
    tool result records how long the call took on the harness's side. The same
    log exists with and without ARIES, so this compares all deployments alike."""
    durations = []
    for path in glob.glob(os.path.join(task_dir, "harness", "telemetry", "*.jsonl")):
        if "trajectory" in os.path.basename(path):
            continue
        for entry in read_jsonl(path):
            message = entry.get("message") or {}
            if message.get("role") == "toolResult" and within(entry.get("timestamp"), window):
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
        # Runs before the rename called bridge registration bridge_start.
        if "bridge_start" in phases:
            phases.setdefault("bridge_register", phases.pop("bridge_start"))
        row = {key: entry[key] for key in ("harness", "backend", "mode", "repetition")}
        row["variant"] = entry.get("variant") or "cold"
        row["task"] = task["task_id"].rsplit("-", 1)[0]
        row["reward"] = task["evaluation"].get("reward")
        row["error"] = task.get("error", "")
        for name in PHASES:
            row[name + "_s"] = phases[name]["duration"] / 1e9 if name in phases else None
        row["task_s"] = task["duration"] / 1e9
        # The cold run is the first request on the fresh harness and sandbox:
        # the warm-up when there is one, else the task. The warm run is the
        # task after it, on the same harness and sandbox.
        warmups = task.get("warmups") or []
        row["warmups"] = len(warmups)
        runs = {}
        if warmups:
            first_start = parse_time(warmups[0]["started_at"])
            runs["cold"] = (first_start, first_start + warmups[0]["duration"] / 1e9)
        if "harness_run" in phases:
            run_start = parse_time(phases["harness_run"]["started_at"])
            runs["warm" if warmups else "cold"] = (run_start, run_start + phases["harness_run"]["duration"] / 1e9)
        if "cold" in runs:
            first = min(parse_time(phases[name]["started_at"]) for name in STARTUP if name in phases)
            row["provisioning_s"] = sum(row[name + "_s"] or 0 for name in ("sandbox_start", "sandbox_prepare", "bridge_register"))
            row["cold_startup_s"] = runs["cold"][0] - first
            row["cold_latency_s"] = runs["cold"][1] - first
        if "warm" in runs:
            row["warm_latency_s"] = runs["warm"][1] - runs["warm"][0]
        # OpenClaw's own measure of the agent's run, which the native baseline
        # records; its wall-clock harness_run also includes starting the CLI.
        if task.get("agent_internal_ms") is not None:
            row["agent_internal_s"] = task["agent_internal_ms"] / 1000

        all_calls = read_jsonl(os.path.join(run_dir, task["task_id"], "bridge", "tool-calls.jsonl"))
        chats = [r for r in requests if r["path"].endswith("/chat/completions")]
        for name, window in runs.items():
            calls = [call for call in all_calls if within(call.get("timestamp"), window)]
            chat = [r for r in chats if within(r["received_at"], window)]
            row[name + "_run_s"] = window[1] - window[0]
            row[name + "_model_s"] = sum(r["duration_ms"] for r in chat) / 1000
            row[name + "_model_calls"] = len(chat)
            row[name + "_model_misses"] = sum(1 for r in chat if r.get("hit") is False)
            row[name + "_tool_calls"] = len(calls)
            row[name + "_tool_rejected"] = sum(1 for call in calls if call.get("status") != "completed")
            row[name + "_tool_s"] = sum(call.get("duration_ms", 0) for call in calls) / 1000
            row[name + "_tool_mean_ms"] = statistics.mean(c.get("duration_ms", 0) for c in calls) if calls else None
            row[name + "_other_s"] = row[name + "_run_s"] - row[name + "_model_s"] - row[name + "_tool_s"]
            harness_calls = harness_tool_calls(os.path.join(run_dir, task["task_id"]), window)
            row[name + "_harness_tool_calls"] = len(harness_calls)
            row[name + "_harness_tool_mean_ms"] = statistics.mean(harness_calls) if harness_calls else None
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

    shown = ["cold_latency_s", "cold_startup_s", "provisioning_s", "harness_prepare_s", "harness_start_s",
             "cold_run_s", "cold_model_s", "cold_tool_s", "cold_other_s",
             "warm_latency_s", "warm_model_s", "warm_tool_s", "warm_other_s", "agent_internal_s",
             "task_s", "isolation_s", "evaluation_s", "cleanup_s", "cold_tool_mean_ms", "warm_tool_mean_ms",
             "cold_harness_tool_mean_ms", "warm_harness_tool_mean_ms",
             "sandbox_cpu_mean", "sandbox_cpu_peak", "harness_cpu_peak", "harness_memory_peak_mib"]
    groups = defaultdict(list)
    for row in rows:
        if row["mode"] == "replay":
            groups[(row["harness"], row["task"], row["variant"], row["backend"])].append(row)
    print("Medians over replayed runs; times in seconds, CPU in percent of one core.\n")
    for harness, task, variant in sorted({key[:3] for key in groups}):
        native = groups.get((harness, task, variant, "native"), [])
        docker = groups.get((harness, task, variant, "docker"), [])
        k8s = groups.get((harness, task, variant, "k8s"), [])
        solved = lambda rows: "%d/%d" % (sum(1 for r in rows if r["reward"] == 1), len(rows))
        print("### %s, %s, %s (solved: native %s, ARIES Docker %s, ARIES Kubernetes %s)\n" % (
            harness, task, variant, solved(native), solved(docker), solved(k8s)))
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
