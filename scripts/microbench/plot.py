#!/usr/bin/env python3
"""Draws the microbenchmark comparison figures for the report.

    python3 scripts/microbench/plot.py [--data DIR] [--out DIR]

Reads what scripts/microbench/analyze.py leaves under DIR (default: this
checkout): .cache/microbench-runs/tasks.csv, the manifest, and each listed
run's run-result.json and monitor/resources.jsonl. Writes, as PNG and PDF, to
--out (default .cache/microbench-report):

  stages       where a task's time goes, per setup (medians per stage)
  task-time    end-to-end time of every task, per setup
  tool-call    time per tool call, at the bridge and as OpenClaw sees it
  cpu-timeline CPU over time for mb-cpu, aligned at the start of the agent's run
  cpu-seconds  CPU time a task costs, harness and sandbox
  memory       peak memory of the harness (sandboxes' are in figures.csv)

and figures.csv, every plotted number, for a table beside the figures.

CPU comes from the cumulative usage counter, not the per-sample percentage:
a task's CPU time is the counter's last value, and the timeline is its rate
between timestamped samples, so irregular sample spacing cannot inflate it.
Only replayed runs are used, so the model's replies and their timing are the
same in every setup. Needs matplotlib.
"""

import argparse
import csv
import datetime
import json
import os
import statistics
from collections import defaultdict

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

SETUPS = [("native", "Native (own Docker)"), ("docker", "ARIES + Docker"), ("k8s", "ARIES + Kubernetes")]
HARNESSES = [("hermes", "Hermes"), ("openclaw", "OpenClaw")]
TASKS = ["mb-noop", "mb-one-call", "mb-ten-calls", "mb-cpu", "mb-output"]
# Categorical slots 1-3 of the validated reference palette (all-pairs safe).
SETUP_COLOR = {"native": "#2a78d6", "docker": "#eb6834", "k8s": "#1baf7a"}
# Slots 1-6 in order, validated for adjacent pairs (stacked segments).
STAGES = [
    ("Sandbox start and register", "#2a78d6"),
    ("Harness start", "#eb6834"),
    ("Model (replayed)", "#1baf7a"),
    ("Tools and harness logic", "#eda100"),
    ("Stop and evaluate", "#e87ba4"),
    ("Cleanup", "#008300"),
]
INK, INK_2, MUTED, GRID, SURFACE = "#0b0b0b", "#52514e", "#8a8984", "#e6e5e1", "#ffffff"
CPU_LIMIT = 2.0

plt.rcParams.update({
    "font.family": "sans-serif", "font.size": 9, "axes.titlesize": 10, "axes.titleweight": "bold",
    "axes.labelcolor": INK_2, "axes.edgecolor": GRID, "axes.linewidth": 0.8, "axes.titlecolor": INK,
    "xtick.color": INK_2, "ytick.color": INK_2, "xtick.major.size": 0, "ytick.major.size": 0,
    "figure.facecolor": SURFACE, "axes.facecolor": SURFACE, "savefig.facecolor": SURFACE,
    "legend.frameon": False, "text.color": INK,
})


def number(text):
    try:
        return float(text)
    except (TypeError, ValueError):
        return None


def median(values):
    values = [v for v in values if v is not None]
    return statistics.median(values) if values else None


def parse_time(text):
    # Go writes nanoseconds; Python keeps microseconds.
    text = text.replace("Z", "+00:00")
    if "." in text:
        head, rest = text.split(".", 1)
        digits = rest[:len(rest) - len(rest.lstrip("0123456789"))]
        text = "%s.%s%s" % (head, digits[:6].ljust(6, "0"), rest[len(digits):])
    return datetime.datetime.fromisoformat(text).timestamp()


def task_name(task_id):
    return task_id.rsplit("-", 1)[0]


def load(data):
    runs = os.path.join(data, ".cache", "microbench-runs")
    with open(os.path.join(runs, "tasks.csv")) as handle:
        rows = [row for row in csv.DictReader(handle) if row["mode"] == "replay"]
    for row in rows:
        for key, value in row.items():
            if key not in ("harness", "backend", "mode", "task", "error"):
                row[key] = number(value)
    with open(os.path.join(runs, "manifest.tsv")) as handle:
        entries = [entry for entry in csv.DictReader(handle, delimiter="\t") if entry["mode"] == "replay"]
    for entry in entries:
        entry["dir"] = os.path.join(data, "runs", os.path.basename(entry["run_dir"]))
    return rows, entries


def samples(path):
    """Samples per component, then per container: a component can span several
    containers (native Hermes starts two sandboxes), each with its own counter."""
    by_component = defaultdict(lambda: defaultdict(list))
    if not os.path.exists(path):
        return by_component
    with open(path) as handle:
        for line in handle:
            sample = json.loads(line)
            if sample.get("cpu_usage_nanoseconds") is not None:
                sample["t"] = parse_time(sample["time"])
                series = sample.get("runtime_id") or sample.get("runtime_name")
                by_component[sample["component"]][series].append(sample)
    for containers in by_component.values():
        for values in containers.values():
            values.sort(key=lambda sample: sample["t"])
    return by_component


def cpu_seconds(containers):
    values = [max(s["cpu_usage_nanoseconds"] for s in series) / 1e9 for series in containers.values() if series]
    return sum(values) if values else None


def cpu_rate(containers, origin):
    """CPU in cores per one-second bin, from each container's cumulative
    counter, summed over the component's containers."""
    bins = defaultdict(float)
    for values in containers.values():
        per_bin = defaultdict(list)
        for before, after in zip(values, values[1:]):
            elapsed = after["t"] - before["t"]
            used = after["cpu_usage_nanoseconds"] - before["cpu_usage_nanoseconds"]
            if elapsed > 0 and used >= 0:
                per_bin[int((after["t"] - origin) // 1)].append(used / 1e9 / elapsed)
        for second, rates in per_bin.items():
            bins[second] += sum(rates) / len(rates)
    seconds = sorted(bins)
    return [second + 0.5 for second in seconds], [bins[second] for second in seconds]


def resource_rows(entries):
    """Per task run: CPU-seconds and the rate-based peak, per component."""
    rows = []
    for entry in entries:
        result_path = os.path.join(entry["dir"], "run-result.json")
        if not os.path.exists(result_path):
            continue
        with open(result_path) as handle:
            result = json.load(handle)
        for task in result.get("tasks", []):
            by_component = samples(os.path.join(entry["dir"], task["task_id"], "monitor", "resources.jsonl"))
            row = {"harness": entry["harness"], "backend": entry["backend"], "repetition": entry["repetition"],
                   "task": task_name(task["task_id"])}
            for component in ("harness", "sandbox"):
                containers = by_component.get(component, {})
                row[component + "_cpu_s"] = cpu_seconds(containers)
                rates = cpu_rate(containers, 0)[1]
                row[component + "_cpu_peak_cores"] = max(rates) if rates else None
            rows.append(row)
    return rows


def group(rows, key):
    grouped = defaultdict(list)
    for row in rows:
        if row.get(key) is not None:
            grouped[(row["harness"], row["backend"], row["task"])].append(row[key])
    return {k: median(v) for k, v in grouped.items()}


def stage_values(row):
    def get(*keys):
        return sum(row.get(key) or 0 for key in keys)
    run = row.get("harness_run_s") or 0
    model = row.get("model_s") or 0
    return [get("sandbox_start_s", "sandbox_prepare_s", "bridge_register_s"), get("harness_start_s"), model,
            max(run - model, 0), get("isolation_s", "evaluation_s"), get("cleanup_s")]


def style(axis, grid_axis="x"):
    axis.grid(axis=grid_axis, color=GRID, linewidth=0.6)
    axis.set_axisbelow(True)
    for side in ("top", "right"):
        axis.spines[side].set_visible(False)


def save(figure, out, name, record):
    for extension in ("png", "pdf"):
        figure.savefig(os.path.join(out, "%s.%s" % (name, extension)), dpi=300, bbox_inches="tight")
    plt.close(figure)
    record.append(name)


def setup_legend(figure, y=1.0):
    handles = [plt.Rectangle((0, 0), 1, 1, color=SETUP_COLOR[key]) for key, _ in SETUPS]
    figure.legend(handles, [label for _, label in SETUPS], loc="upper center", ncol=3, bbox_to_anchor=(0.5, y),
                  handlelength=1, handleheight=1, columnspacing=1.6)


def plot_stages(rows, out, table, record):
    tasks = ["mb-noop", "mb-ten-calls", "mb-cpu"]
    per = defaultdict(list)
    for row in rows:
        per[(row["harness"], row["backend"], row["task"])].append(stage_values(row))
    figure, axes = plt.subplots(len(HARNESSES), len(tasks), figsize=(11, 4.6), sharex=True)
    for r, (harness, harness_label) in enumerate(HARNESSES):
        for c, task in enumerate(tasks):
            axis = axes[r][c]
            style(axis)
            for y, (backend, label) in enumerate(SETUPS):
                stacks = per.get((harness, backend, task))
                if not stacks:
                    continue
                values = [median(column) for column in zip(*stacks)]
                left = 0
                for (stage, color), value in zip(STAGES, values):
                    if value > 0:
                        axis.barh(y, value, left=left, height=0.62, color=color, edgecolor=SURFACE, linewidth=1.2)
                        if value >= 3.2:
                            axis.text(left + value / 2, y, "%.1f" % value, ha="center", va="center", fontsize=7, color=INK)
                    table.append(["stages", harness, backend, task, stage, round(value, 3)])
                    left += value
                axis.text(left + 0.6, y, "%.1f s" % left, va="center", fontsize=8, color=INK, fontweight="bold")
            axis.set_yticks(range(len(SETUPS)))
            axis.set_yticklabels([label for _, label in SETUPS] if c == 0 else [])
            axis.invert_yaxis()
            axis.set_title("%s · %s" % (harness_label, task), loc="left")
            if r == len(HARNESSES) - 1:
                axis.set_xlabel("seconds (median of 5 runs per stage)")
    axes[0][0].set_xlim(0, 70)
    handles = [plt.Rectangle((0, 0), 1, 1, color=color) for _, color in STAGES]
    figure.legend(handles, [stage for stage, _ in STAGES], loc="upper center", ncol=6, bbox_to_anchor=(0.5, 1.04),
                  handlelength=1, handleheight=1, columnspacing=1.2)
    figure.tight_layout()
    save(figure, out, "stages", record)


def grouped_bars(axis, groups, values, unit_format, missing="n/a"):
    width = 0.26
    for i, (backend, _) in enumerate(SETUPS):
        for g, key in enumerate(groups):
            value = values.get((key, backend))
            x = g + (i - 1) * width
            if value is None:
                axis.text(x, 0, missing, ha="center", va="bottom", fontsize=6.5, color=MUTED)
                continue
            axis.bar(x, value, width=width - 0.03, color=SETUP_COLOR[backend], edgecolor=SURFACE, linewidth=0.8)
            axis.text(x, value, unit_format % value, ha="center", va="bottom", fontsize=6.5, color=INK)


def plot_per_task(rows, key, ylabel, unit_format, name, out, table, record):
    """One panel per harness, every task, grouped by setup."""
    medians = group(rows, key)
    figure, axes = plt.subplots(1, len(HARNESSES), figsize=(11, 3.4), sharey=True)
    for axis, (harness, label) in zip(axes, HARNESSES):
        style(axis, "y")
        values = {(task, backend): medians.get((harness, backend, task)) for task in TASKS for backend, _ in SETUPS}
        grouped_bars(axis, TASKS, values, unit_format)
        axis.set_xticks(range(len(TASKS)))
        axis.set_xticklabels(TASKS)
        axis.set_title(label, loc="left")
        for (task, backend), value in values.items():
            table.append([name, harness, backend, task, key, value])
    axes[0].set_ylabel(ylabel)
    setup_legend(figure, 1.06)
    figure.tight_layout()
    save(figure, out, name, record)


def plot_tool_call(rows, out, table, record):
    bridge = group(rows, "tool_exec_mean_ms")
    inside = group(rows, "harness_tool_mean_ms")
    groups = [("hermes", "bridge", "Hermes\nmeasured at the bridge"), ("openclaw", "bridge", "OpenClaw\nmeasured at the bridge"),
              ("openclaw", "harness", "OpenClaw\nas the harness sees it")]
    values = {}
    for harness, where, label in groups:
        source = bridge if where == "bridge" else inside
        for backend, _ in SETUPS:
            value = source.get((harness, backend, "mb-ten-calls"))
            values[(label, backend)] = value
            table.append(["tool-call", harness, backend, "mb-ten-calls", where + "_ms", value])
    figure, axis = plt.subplots(figsize=(7.5, 3.4))
    style(axis, "y")
    grouped_bars(axis, [label for _, _, label in groups], values, "%.0f", missing="none")
    axis.set_xticks(range(len(groups)))
    axis.set_xticklabels([label for _, _, label in groups])
    axis.set_xlim(-0.6, len(groups) - 0.4)
    axis.set_ylabel("ms per tool call (mb-ten-calls, median)")
    axis.text(0.0, -0.3, "Native runs have no bridge, so only the harness's own timing exists for them.",
              transform=axis.transAxes, fontsize=7, color=INK_2)
    setup_legend(figure, 1.08)
    figure.tight_layout()
    save(figure, out, "tool-call", record)


def representative(rows, harness, backend, task):
    """The repetition whose task time is the median one."""
    candidates = sorted((row["task_s"], row["repetition"]) for row in rows
                        if (row["harness"], row["backend"], row["task"]) == (harness, backend, task) and row["task_s"])
    return candidates[len(candidates) // 2][1] if candidates else None


def plot_cpu_timeline(rows, entries, out, record):
    task = "mb-cpu"
    figure, axes = plt.subplots(len(HARNESSES), 2, figsize=(11, 5), sharex=True, sharey=True)
    for r, (harness, harness_label) in enumerate(HARNESSES):
        for backend, label in SETUPS:
            repetition = representative(rows, harness, backend, task)
            entry = next((e for e in entries if (e["harness"], e["backend"]) == (harness, backend)
                          and number(e["repetition"]) == repetition), None)
            if entry is None:
                continue
            with open(os.path.join(entry["dir"], "run-result.json")) as handle:
                result = json.load(handle)
            record_task = next(t for t in result["tasks"] if task_name(t["task_id"]) == task)
            run_phase = next(p for p in record_task["phases"] if p["name"] == "harness_run")
            origin = parse_time(run_phase["started_at"])
            by_component = samples(os.path.join(entry["dir"], record_task["task_id"], "monitor", "resources.jsonl"))
            for c, component in enumerate(("harness", "sandbox")):
                times, cores = cpu_rate(by_component.get(component, {}), origin)
                axes[r][c].plot(times, cores, color=SETUP_COLOR[backend], linewidth=1.6, label=label)
        for c, component in enumerate(("Harness container", "Sandbox container")):
            axis = axes[r][c]
            style(axis, "y")
            axis.axhline(CPU_LIMIT, color=MUTED, linewidth=0.8, linestyle=(0, (3, 3)))
            axis.axvline(0, color=MUTED, linewidth=0.8)
            axis.text(-31, CPU_LIMIT + 0.05, "2-CPU limit", fontsize=7, color=INK_2, va="bottom")
            axis.text(0.4, 2.3, "agent's run starts", fontsize=7, color=INK_2, va="top")
            axis.set_title("%s · %s" % (harness_label, component), loc="left")
            if c == 0:
                axis.set_ylabel("CPU, cores")
            if r == len(HARNESSES) - 1:
                axis.set_xlabel("seconds from the start of the agent's run (mb-cpu, median run)")
    axes[0][0].set_xlim(-32, 22)
    axes[0][0].set_ylim(0, 2.4)
    setup_legend(figure, 1.04)
    figure.tight_layout()
    save(figure, out, "cpu-timeline", record)


def plot_components(values, tasks, ylabel, unit_format, name, title_format, out, record):
    figure, axes = plt.subplots(1, len(HARNESSES) * len(tasks), figsize=(11, 3.4), sharey=True)
    axes = list(axes) if hasattr(axes, "__len__") else [axes]
    index = 0
    for harness, harness_label in HARNESSES:
        for task in tasks:
            axis = axes[index]
            index += 1
            style(axis, "y")
            groups = ["harness", "sandbox"]
            data = {(component, backend): values.get((harness, backend, task, component))
                    for component in groups for backend, _ in SETUPS}
            grouped_bars(axis, groups, data, unit_format)
            axis.set_xticks(range(len(groups)))
            axis.set_xticklabels(["Harness", "Sandbox"])
            axis.set_title(title_format % (harness_label, task), loc="left")
    axes[0].set_ylabel(ylabel)
    setup_legend(figure, 1.08)
    figure.tight_layout()
    save(figure, out, name, record)


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    root = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
    parser.add_argument("--data", default=root)
    parser.add_argument("--out", default=os.path.join(root, ".cache", "microbench-report"))
    args = parser.parse_args()
    os.makedirs(args.out, exist_ok=True)
    rows, entries = load(args.data)
    resources = resource_rows(entries)
    table, record = [], []

    plot_stages(rows, args.out, table, record)
    plot_per_task(rows, "task_s", "end-to-end task time, s (median of 5)", "%.1f", "task-time", args.out, table, record)
    plot_tool_call(rows, args.out, table, record)
    plot_cpu_timeline(rows, entries, args.out, record)

    cpu = {}
    for component in ("harness", "sandbox"):
        for key, value in group(resources, component + "_cpu_s").items():
            cpu[key + (component,)] = value
            table.append(["cpu-seconds", key[0], key[1], key[2], component + "_cpu_s", value])
        for key, value in group(resources, component + "_cpu_peak_cores").items():
            table.append(["cpu-peak", key[0], key[1], key[2], component + "_cpu_peak_cores", value])
    plot_components(cpu, ["mb-ten-calls", "mb-cpu"], "CPU time per task, CPU-seconds (median)", "%.1f",
                    "cpu-seconds", "%s · %s", args.out, record)

    # Sandboxes peak at a few MiB, too small to share a scale with the
    # harnesses; their values are in figures.csv.
    plot_per_task(rows, "harness_memory_peak_mib", "harness peak memory, MiB (median)", "%.0f", "memory",
                  args.out, table, record)
    for (harness, backend, task), value in group(rows, "sandbox_memory_peak_mib").items():
        table.append(["memory", harness, backend, task, "sandbox_memory_peak_mib", value])

    with open(os.path.join(args.out, "figures.csv"), "w", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["figure", "harness", "setup", "task", "measure", "value"])
        writer.writerows([[f, h, b, t, m, "" if v is None else round(v, 3)] for f, h, b, t, m, v in table])
    print("wrote %s to %s" % (", ".join(record), args.out))


if __name__ == "__main__":
    main()
