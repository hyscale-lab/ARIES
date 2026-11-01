#!/usr/bin/env python3
"""Draws the cold- and warm-start figures for the report.

    python3 scripts/microbench/plot_coldwarm.py TASKS_CSV OUT_DIR

Reads the tasks.csv that scripts/microbench/analyze.py writes for warm-mode
runs (each task run cold, then again warm on the same harness and sandbox) and
writes, as PNG and PDF, to OUT_DIR:

  cold-latency   time from the first start-up step to the first answer
  warm-latency   the same task again on the started harness and sandbox
  first-request  how much longer the first request takes than the repeat
  after-answer   what follows the answer: collection, stop, evaluation, cleanup

and figures.csv, every plotted number. Uses plot.py's palette and styling.
"""

import csv
import os
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from plot import (HARNESSES, INK, SETUPS, STAGES, SURFACE, median, number, plot_per_task, plt,  # noqa: E402
                  save, style)

COLD_STAGES = [
    ("Sandbox and bridge", STAGES[0][1]),
    ("Harness start", STAGES[1][1]),
    ("First request: model", STAGES[2][1]),
    ("First request: tools and harness", STAGES[3][1]),
]
AFTER_STAGES = [
    ("Collect artifacts", STAGES[0][1]),
    ("Stop harness and bridge", STAGES[1][1]),
    ("Evaluate", STAGES[2][1]),
    ("Clean up", STAGES[3][1]),
]


def load(path):
    with open(path) as handle:
        rows = [row for row in csv.DictReader(handle) if row["mode"] == "replay"]
    for row in rows:
        for key, value in row.items():
            if key not in ("harness", "backend", "mode", "task", "error", "variant"):
                row[key] = number(value)
        if row.get("cold_run_s") is not None and row.get("warm_run_s") is not None:
            row["first_extra_s"] = row["cold_run_s"] - row["warm_run_s"]
    return rows


def cold_values(row):
    provisioning = row.get("provisioning_s") or 0
    model = row.get("cold_model_s") or 0
    return [provisioning, row["cold_startup_s"] - provisioning, model, row["cold_run_s"] - model]


def after_values(row):
    return [row.get(key) or 0 for key in ("harness_collect_s", "isolation_s", "evaluation_s", "cleanup_s")]


def stacked_rows(axis, stacks, stages, table, label_parts):
    """One horizontal stacked bar per setup, from per-run stage lists."""
    for y, (backend, _) in enumerate(SETUPS):
        runs = stacks.get(backend)
        if not runs:
            continue
        values = [median(column) for column in zip(*runs)]
        left = 0
        for (stage, color), value in zip(stages, values):
            if value > 0:
                axis.barh(y, value, left=left, height=0.62, color=color, edgecolor=SURFACE, linewidth=1.2)
            table.append(label_parts + [backend, stage, round(value, 3)])
            left += value
        axis.text(left, y, "  %.1f s" % left, va="center", fontsize=8, color=INK, fontweight="bold")
    axis.set_yticks(range(len(SETUPS)))
    axis.invert_yaxis()


def stage_legend(figure, stages, y):
    handles = [plt.Rectangle((0, 0), 1, 1, color=color) for _, color in stages]
    figure.legend(handles, [stage for stage, _ in stages], loc="upper center", ncol=len(stages),
                  bbox_to_anchor=(0.5, y), handlelength=1, handleheight=1, columnspacing=1.4)


def plot_cold(rows, out, table, record):
    tasks = ["mb-noop", "mb-ten-calls", "mb-cpu"]
    per = defaultdict(lambda: defaultdict(list))
    for row in rows:
        if row.get("cold_latency_s") is not None:
            per[(row["harness"], row["task"])][row["backend"]].append(cold_values(row))
    figure, axes = plt.subplots(len(HARNESSES), len(tasks), figsize=(11, 4.6), sharex=True)
    for r, (harness, harness_label) in enumerate(HARNESSES):
        for c, task in enumerate(tasks):
            axis = axes[r][c]
            style(axis)
            stacked_rows(axis, per[(harness, task)], COLD_STAGES, table, ["cold-latency", harness, task])
            axis.set_yticklabels([label for _, label in SETUPS] if c == 0 else [])
            axis.set_title("%s · %s" % (harness_label, task), loc="left")
            if r == len(HARNESSES) - 1 and c == len(tasks) // 2:
                axis.set_xlabel("seconds to the first answer (median of 5 runs per stage)")
    axes[0][0].set_xlim(0, 62)
    stage_legend(figure, COLD_STAGES, 1.04)
    figure.tight_layout()
    save(figure, out, "cold-latency", record)


def plot_after(rows, out, table, record):
    per = defaultdict(lambda: defaultdict(list))
    for row in rows:
        per[row["harness"]][row["backend"]].append(after_values(row))
    figure, axes = plt.subplots(1, len(HARNESSES), figsize=(11, 2.4), sharex=True)
    for c, (axis, (harness, label)) in enumerate(zip(axes, HARNESSES)):
        style(axis)
        stacked_rows(axis, per[harness], AFTER_STAGES, table, ["after-answer", harness, "all tasks"])
        axis.set_yticklabels([name for _, name in SETUPS] if c == 0 else [])
        axis.set_title(label, loc="left")
        axis.set_xlabel("seconds after the answer (median over all tasks)")
    stage_legend(figure, AFTER_STAGES, 1.1)
    figure.tight_layout()
    save(figure, out, "after-answer", record)


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    rows = load(sys.argv[1])
    out = sys.argv[2]
    os.makedirs(out, exist_ok=True)
    table, record = [], []
    plot_cold(rows, out, table, record)
    plot_per_task(rows, "warm_latency_s", "seconds (median of 5 runs)", "%.1f", "warm-latency", out, table, record)
    plot_per_task(rows, "first_extra_s", "first request minus the repeat, seconds", "%.1f", "first-request",
                  out, table, record)
    plot_after(rows, out, table, record)
    with open(os.path.join(out, "figures.csv"), "w", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["figure", "harness", "task", "setup", "measure", "value"])
        for entry in table:
            if entry[0] in ("warm-latency", "first-request"):
                # plot_per_task records figure, harness, backend, task, key, value.
                figure_name, harness, backend, task, key, value = entry
                entry = [figure_name, harness, task, backend, key, value]
            writer.writerow(entry)
    print("wrote " + ", ".join(record) + " to " + out)


if __name__ == "__main__":
    main()
