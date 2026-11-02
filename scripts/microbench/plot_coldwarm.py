#!/usr/bin/env python3
"""Draws the cold- and warm-start figures for the report.

    python3 scripts/microbench/plot_coldwarm.py TASKS_CSV OUT_DIR

Reads the tasks.csv that scripts/microbench/analyze.py writes for warm-mode
runs (each task run cold, then again warm on the same harness and sandbox) and
writes, as PNG and PDF, to OUT_DIR:

  end-to-end     cold and warm latency for every setup, split into stages
  after-answer   what follows the answer: collection, stop, evaluation, cleanup

and figures.csv, every plotted number. Uses plot.py's palette and styling.
"""

import csv
import os
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from plot import HARNESSES, INK, SETUPS, STAGES, SURFACE, TASKS, median, number, plt, save, style  # noqa: E402

E2E_STAGES = [
    ("Sandbox, bridge and harness container", STAGES[0][1]),
    ("Harness start", STAGES[1][1]),
    ("Model (replayed)", STAGES[2][1]),
    ("Tools and harness logic", STAGES[3][1]),
]
AFTER_STAGES = [
    ("Collect artifacts", STAGES[0][1]),
    ("Stop harness, revoke bridge access", STAGES[1][1]),
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
    return rows


def cold_values(row):
    """Start-up split by what is on its critical path: steps can overlap, so
    harness start is its own phase and everything else before the first
    request is the rest."""
    harness = row.get("harness_start_s") or 0
    model = row.get("cold_model_s") or 0
    return [row["cold_startup_s"] - harness, harness, model, row["cold_run_s"] - model]


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


def end_to_end_values(row, measure):
    """Stages of a cold or warm run in E2E_STAGES order; a warm run has no
    start-up."""
    model = row.get(measure + "_model_s") or 0
    rest = [model, row[measure + "_run_s"] - model]
    return cold_values(row) if measure == "cold" else [0, 0] + rest


def plot_end_to_end(rows, out, table, record):
    """Cold and warm end-to-end time per setup, split into stages."""
    per = defaultdict(list)
    for row in rows:
        for measure in ("cold", "warm"):
            if row.get(measure + "_latency_s") is not None:
                per[(row["harness"], row["task"], row["backend"], measure)].append(end_to_end_values(row, measure))
    bars = [(backend, measure, "%s · %s" % (name, measure)) for backend, name in SETUPS for measure in ("cold", "warm")]
    positions = [i * 2.4 + j for i in range(len(SETUPS)) for j in (0, 1)]
    figure, axes = plt.subplots(len(HARNESSES), len(TASKS), figsize=(15, 6.6), sharey=True)
    for r, (harness, harness_label) in enumerate(HARNESSES):
        limit = max(sum(median(column) for column in zip(*runs))
                    for (h, _, _, _), runs in per.items() if h == harness) * 1.22
        for c, task in enumerate(TASKS):
            axis = axes[r][c]
            style(axis)
            for y, (backend, measure, _) in zip(positions, bars):
                runs = per.get((harness, task, backend, measure))
                if not runs:
                    continue
                values = [median(column) for column in zip(*runs)]
                left = 0
                for (stage, color), value in zip(E2E_STAGES, values):
                    if value > 0:
                        axis.barh(y, value, left=left, height=0.82, color=color, edgecolor=SURFACE, linewidth=1)
                        if value >= limit * 0.16:
                            axis.text(left + value / 2, y, "%.1f" % value, ha="center", va="center", fontsize=6.5,
                                      color=INK)
                    table.append(["end-to-end", harness, task, backend, measure + ": " + stage, round(value, 3)])
                    left += value
                axis.text(left, y, " %.1f" % left, va="center", fontsize=7, color=INK, fontweight="bold")
            axis.set_xlim(0, limit)
            axis.set_yticks(positions)
            axis.set_yticklabels([label for _, _, label in bars])
            axis.set_title("%s · %s" % (harness_label, task), loc="left")
            if r == len(HARNESSES) - 1 and c == len(TASKS) // 2:
                axis.set_xlabel("seconds (median of 5 runs per stage)")
    # The panels share the y-axis, so it is inverted once for all of them.
    axes[0][0].invert_yaxis()
    stage_legend(figure, E2E_STAGES, 1.04)
    figure.tight_layout()
    save(figure, out, "end-to-end", record)


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
    plot_end_to_end(rows, out, table, record)
    plot_after(rows, out, table, record)
    with open(os.path.join(out, "figures.csv"), "w", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["figure", "harness", "task", "setup", "measure", "value"])
        writer.writerows(table)
    print("wrote " + ", ".join(record) + " to " + out)


if __name__ == "__main__":
    main()
