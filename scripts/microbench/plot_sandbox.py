#!/usr/bin/env python3
"""Draws the figures of experiments/sandbox/report.md.

    python3 scripts/microbench/plot_sandbox.py [--dir experiments/sandbox]

Reads phase0/latency.jsonl, phase0/latency-k8s-c10-nopool.jsonl and
phase1/tasks.tsv under --dir, and writes PNG and PDF figures to its figures/:

  phase0-create      sandbox create time, p50 and p95, at concurrency 1 and 10
  phase0-exec-delete exec and delete time at concurrency 1
  phase1-lifecycle   sandbox start and cleanup in ARIES, per runtime and mode
  phase1-warm-pool   a warm pool's first and later claims against creating
  phase1-resources   mb-cpu's sandbox peak memory and CPU, per runtime and mode

Uses the style of plot.py. Needs matplotlib.
"""

import argparse
import csv
import json
import os
import statistics
from collections import defaultdict

from plot import INK, INK_2, MUTED, SURFACE, plt, save, style

BLUE, ORANGE, GREEN = "#2a78d6", "#eb6834", "#1baf7a"

# Phase 0 backends, fastest-to-create first, with how each was driven.
BACKENDS = [
    ("cube", "CubeSandbox"),
    ("agentsandbox-claim", "agent-sandbox, pool claim"),
    ("docker", "Docker"),
    ("opensandbox", "OpenSandbox"),
    ("agentsandbox-gvisor", "agent-sandbox, gVisor"),
    ("agentsandbox", "agent-sandbox, runc"),
    ("pod", "Pod, runc"),
    ("pod-gvisor", "Pod, gVisor"),
    ("pod-kata", "Pod, Kata"),
    ("agentsandbox-kata", "agent-sandbox, Kata"),
]
CLIENT = {"cube": "in process", "opensandbox": "in process", "docker": "docker CLI"}
RUNTIMES = [("", "runc"), ("-gvisor", "gVisor"), ("-kata", "Kata")]
MODES = [("k8s", "Pod", BLUE), ("k8s-agent-sandbox", "agent-sandbox", ORANGE), ("k8s-warm-pool", "Warm pool", GREEN)]
HARNESSES = [("hermes", "Hermes"), ("openclaw", "OpenClaw")]


def quantile(values, q):
    values = sorted(values)
    return values[min(len(values) - 1, int(round(q * (len(values) - 1))))]


def phase0(directory):
    """p50/p95 per backend and concurrency. The c=10 Kubernetes rows come from
    the rerun without the warm pool, which had held most of the worker's CPUs."""
    rounds = defaultdict(list)
    for line in open(os.path.join(directory, "phase0", "latency.jsonl")):
        row = json.loads(line)
        if "create_s" in row:
            rounds[(row["backend"], row["concurrency"])].append(row)
    stats = {}
    for key, rows in rounds.items():
        stats[key] = {metric: (quantile([r[metric] for r in rows], 0.5), quantile([r[metric] for r in rows], 0.95))
                      for metric in ("create_s", "exec_median_s", "delete_s")}
    for line in open(os.path.join(directory, "phase0", "latency-k8s-c10-nopool.jsonl")):
        summary = json.loads(line)["summary"]
        stats[(summary["backend"], summary["concurrency"])] = {
            metric: (summary[metric]["p50"], summary[metric]["p95"]) for metric in ("create_s", "exec_median_s", "delete_s")}
    return stats


def bar_label(axis, x, y, text, horizontal=True):
    if horizontal:
        axis.text(x, y, " " + text, va="center", ha="left", fontsize=7, color=INK)
    else:
        axis.text(x, y, text, va="bottom", ha="center", fontsize=7, color=INK)


def plot_create(stats, out, record):
    figure, axis = plt.subplots(figsize=(7.6, 4.4))
    style(axis)
    height = 0.36
    for i, (concurrency, color) in enumerate([(1, BLUE), (10, ORANGE)]):
        for y, (backend, _) in enumerate(BACKENDS):
            p50, p95 = stats[(backend, concurrency)]["create_s"]
            offset = y + (i - 0.5) * (height + 0.04)
            axis.barh(offset, p50, height=height, color=color, edgecolor=SURFACE, linewidth=1)
            axis.plot([p50, p95], [offset, offset], color=INK_2, linewidth=1)
            axis.plot([p95], [offset], marker="|", color=INK_2, markersize=6)
            bar_label(axis, p95, offset, "%.2f s" % p50)
    axis.set_xscale("log")
    axis.set_xlim(0.05, 40)
    axis.set_yticks(range(len(BACKENDS)))
    axis.set_yticklabels(["%s (%s)" % (label, CLIENT.get(key, "kubectl")) for key, label in BACKENDS])
    axis.invert_yaxis()
    axis.set_xlabel("seconds, log scale: bar to p50, line to p95; label is p50 (50 rounds)")
    axis.set_title("Phase 0: time to create a usable sandbox", loc="left", pad=22)
    handles = [plt.Rectangle((0, 0), 1, 1, color=c) for c in (BLUE, ORANGE)]
    figure.legend(handles, ["concurrency 1", "concurrency 10"], loc="upper center", ncol=2, bbox_to_anchor=(0.6, 1.04),
                  handlelength=1, handleheight=1, columnspacing=1.6)
    figure.text(0.01, -0.02, "Concurrency 10 for the Kubernetes pods and Sandboxes is the rerun without a warm pool "
                "holding the worker's CPUs; the pool claim's p95 of 8 s is that queueing.", fontsize=7, color=MUTED)
    figure.tight_layout()
    save(figure, out, "phase0-create", record)


def plot_exec_delete(stats, out, record):
    figure, axes = plt.subplots(1, 2, figsize=(8.6, 3.8), sharey=True)
    for axis, (metric, title, limit) in zip(axes, [("exec_median_s", "Exec of one echo", 0.3),
                                                   ("delete_s", "Delete, until absence confirmed", 3.8)]):
        style(axis)
        for y, (backend, _) in enumerate(BACKENDS):
            p50, _ = stats[(backend, 1)][metric]
            axis.barh(y, p50, height=0.62, color=BLUE, edgecolor=SURFACE, linewidth=1)
            label = "%.0f ms" % (p50 * 1000) if metric == "exec_median_s" else "%.2f s" % p50
            if metric == "delete_s" and backend.startswith("agentsandbox"):
                label += "  (Sandbox object only)"
            bar_label(axis, p50, y, label)
        axis.set_xlim(0, limit)
        axis.set_title(title, loc="left")
        axis.set_xlabel("seconds, median at concurrency 1")
    axes[0].set_yticks(range(len(BACKENDS)))
    axes[0].set_yticklabels(["%s (%s)" % (label, CLIENT.get(key, "kubectl")) for key, label in BACKENDS])
    axes[0].invert_yaxis()
    figure.text(0.01, -0.03, "Exec compares only within a client: the command-line tools pay a process start per call. "
                "agent-sandbox's delete here is kubectl's background cascade; ARIES deletes in the foreground.",
                fontsize=7, color=MUTED)
    figure.tight_layout()
    save(figure, out, "phase0-exec-delete", record)


def phase1(directory):
    rows = list(csv.DictReader(open(os.path.join(directory, "phase1", "tasks.tsv")), delimiter="\t"))
    by = defaultdict(list)
    for row in rows:
        by[(row["harness"], row["setup"])].append(row)
    return by


def grouped(axis, values, fmt, ylabel):
    """Bars per runtime (groups) and mode (colours)."""
    style(axis, "y")
    width = 0.26
    for m, (mode, _, color) in enumerate(MODES):
        for g, (suffix, _) in enumerate(RUNTIMES):
            value = values.get(mode + suffix)
            if value is None:
                continue
            x = g + (m - 1) * width
            axis.bar(x, value, width=width - 0.03, color=color, edgecolor=SURFACE, linewidth=1)
            bar_label(axis, x, value, fmt % value, horizontal=False)
    axis.set_xticks(range(len(RUNTIMES)))
    axis.set_xticklabels([label for _, label in RUNTIMES])
    axis.set_ylabel(ylabel)


def mode_legend(figure, y=1.02):
    handles = [plt.Rectangle((0, 0), 1, 1, color=color) for _, _, color in MODES]
    figure.legend(handles, [label for _, label, _ in MODES], loc="upper center", ncol=3, bbox_to_anchor=(0.5, y),
                  handlelength=1, handleheight=1, columnspacing=1.6)


def plot_lifecycle(by, out, record):
    figure, axes = plt.subplots(2, 2, figsize=(8.6, 5.6), sharey="row")
    for c, (harness, harness_label) in enumerate(HARNESSES):
        for r, (field, title) in enumerate([("sandbox_start_s", "Sandbox start"), ("cleanup_s", "Cleanup")]):
            values = {setup: statistics.median(float(row[field]) for row in rows)
                      for (h, setup), rows in by.items() if h == harness}
            grouped(axes[r][c], values, "%.1f", "seconds, median of 5 tasks" if c == 0 else "")
            axes[r][c].set_title("%s · %s" % (harness_label, title), loc="left")
    axes[0][0].set_ylim(0, 8)
    axes[1][0].set_ylim(0, 5.5)
    mode_legend(figure)
    figure.text(0.01, -0.03, "One warm run of the five microbenchmark tasks per setup. A warm pool's median is its "
                "warm claims; its first claim is cold (see phase1-warm-pool).", fontsize=7, color=MUTED)
    figure.tight_layout()
    save(figure, out, "phase1-lifecycle", record)


def plot_warm_pool(by, out, record):
    figure, axes = plt.subplots(1, 2, figsize=(8.6, 3.4), sharey=True)
    series = [("Created per task (agent-sandbox)", BLUE), ("Warm pool, first task (cold)", ORANGE),
              ("Warm pool, tasks 2-5", GREEN)]
    width = 0.26
    for axis, (harness, harness_label) in zip(axes, HARNESSES):
        style(axis, "y")
        for g, (suffix, _) in enumerate(RUNTIMES):
            created = [float(row["sandbox_start_s"]) for row in by[(harness, "k8s-agent-sandbox" + suffix)]]
            pooled = [float(row["sandbox_start_s"]) for row in by[(harness, "k8s-warm-pool" + suffix)]]
            for s, value in enumerate([statistics.median(created), pooled[0], statistics.median(pooled[1:])]):
                x = g + (s - 1) * width
                axis.bar(x, value, width=width - 0.03, color=series[s][1], edgecolor=SURFACE, linewidth=1)
                bar_label(axis, x, value, "%.1f" % value, horizontal=False)
        axis.set_xticks(range(len(RUNTIMES)))
        axis.set_xticklabels([label for _, label in RUNTIMES])
        axis.set_title("%s · sandbox start" % harness_label, loc="left")
        axis.set_ylim(0, 10)
    axes[0].set_ylabel("seconds")
    handles = [plt.Rectangle((0, 0), 1, 1, color=color) for _, color in series]
    figure.legend(handles, [label for label, _ in series], loc="upper center", ncol=3, bbox_to_anchor=(0.5, 1.06),
                  handlelength=1, handleheight=1, columnspacing=1.6)
    figure.tight_layout()
    save(figure, out, "phase1-warm-pool", record)


def plot_resources(by, out, record):
    figure, axes = plt.subplots(1, 2, figsize=(8.6, 3.6))
    for axis, (field, title, fmt, ylabel) in zip(axes, [
            ("sandbox_peak_memory_mib", "mb-cpu sandbox: peak memory", "%.0f", "MiB"),
            ("sandbox_peak_cpu_percent", "mb-cpu sandbox: peak CPU sample (dashed line = one core)", "%.0f%%", "percent of one core")]):
        values = {}
        for (harness, setup), rows in by.items():
            if harness == "hermes":
                cpu = [row for row in rows if row["task"].startswith("mb-cpu")][0]
                values[setup] = float(cpu[field])
        grouped(axis, values, fmt, ylabel)
        axis.set_title("Hermes · " + title.split(": ", 1)[1], loc="left")
    axes[0].set_ylim(0, 400)
    axes[1].set_ylim(0, 220)
    axes[1].axhline(100, color=MUTED, linewidth=0.8, linestyle=(0, (3, 3)))
    mode_legend(figure, y=1.05)
    figure.text(0.01, -0.04, "gVisor's and Kata's readings come from the pod sandbox cgroup and include the runtime "
                "itself (Sentry, VM). Pooled peaks above 100% are a few samples and are not yet explained.",
                fontsize=7, color=MUTED)
    figure.tight_layout()
    save(figure, out, "phase1-resources", record)


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--dir", default=os.path.join(os.path.dirname(__file__), "..", "..", "experiments", "sandbox"))
    args = parser.parse_args()
    out = os.path.join(args.dir, "figures")
    os.makedirs(out, exist_ok=True)
    record = []
    stats = phase0(args.dir)
    plot_create(stats, out, record)
    plot_exec_delete(stats, out, record)
    by = phase1(args.dir)
    plot_lifecycle(by, out, record)
    plot_warm_pool(by, out, record)
    plot_resources(by, out, record)
    print("wrote %s to %s" % (", ".join(record), os.path.relpath(out)))


if __name__ == "__main__":
    main()
