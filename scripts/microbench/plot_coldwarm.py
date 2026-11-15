#!/usr/bin/env python3
"""Draws the cold- and warm-start figures for the report.

    python3 scripts/microbench/plot_coldwarm.py TASKS_CSV OUT_DIR

Reads the tasks.csv that scripts/microbench/analyze.py writes for warm-mode
runs (each task run cold, then again warm on the same harness and sandbox) and
writes, as PNG and PDF, to OUT_DIR:

  end-to-end     cold and warm latency for every setup, split into stages
  after-answer   what follows the answer: collection, stop, evaluation, cleanup
  tasks/<task>-breakdown    one task: cold and warm stages, and what follows the answer
  tasks/<task>-tool-calls   one task: each agent tool call's cost, split into the bridge
                            running its commands and the transport around them

and figures.csv, every plotted number. Uses plot.py's palette and styling.
"""

import csv
import os
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from plot import HARNESSES, INK, SETUPS, STAGES, SURFACE, TASKS, number, plt, save, style  # noqa: E402

E2E_STAGES = [
    ("Sandbox, bridge and harness container", STAGES[0][1]),
    ("Harness start", STAGES[1][1]),
    ("Model (replayed)", STAGES[2][1]),
    ("Tool execution", STAGES[3][1]),
    ("Harness logic", STAGES[4][1]),
]
AFTER_STAGES = [
    ("Collect artifacts", STAGES[0][1]),
    ("Stop harness, revoke bridge access", STAGES[1][1]),
    ("Evaluate", STAGES[2][1]),
    ("Clean up", STAGES[3][1]),
]


def mean(values):
    """The average, which a stacked bar's stages add up to exactly."""
    values = [v for v in values if v is not None]
    return sum(values) / len(values) if values else 0.0


def load(path):
    with open(path) as handle:
        rows = [row for row in csv.DictReader(handle) if row["mode"] == "replay"]
    for row in rows:
        for key, value in row.items():
            if key not in ("harness", "backend", "mode", "task", "error", "variant"):
                row[key] = number(value)
    return rows


def run_values(row, measure):
    """A run split into model time, tool execution as the harness timed it,
    and the harness's own logic, the rest."""
    model = row.get(measure + "_model_s") or 0
    tools = row.get(measure + "_tool_exec_s") or 0
    return [model, tools, row[measure + "_run_s"] - model - tools]


def cold_values(row):
    """Start-up split by what is on its critical path: steps can overlap, so
    harness start is its own phase and everything else before the first
    request is the rest."""
    harness = row.get("harness_start_s") or 0
    return [row["cold_startup_s"] - harness, harness] + run_values(row, "cold")


def after_values(row):
    return [row.get(key) or 0 for key in ("harness_collect_s", "isolation_s", "evaluation_s", "cleanup_s")]


def stacked_rows(axis, stacks, stages, table, label_parts):
    """One horizontal stacked bar per setup, from per-run stage lists."""
    for y, (backend, _) in enumerate(SETUPS):
        runs = stacks.get(backend)
        if not runs:
            continue
        values = [mean(column) for column in zip(*runs)]
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
    return cold_values(row) if measure == "cold" else [0, 0] + run_values(row, "warm")


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
        limit = max(sum(mean(column) for column in zip(*runs))
                    for (h, _, _, _), runs in per.items() if h == harness) * 1.22
        for c, task in enumerate(TASKS):
            axis = axes[r][c]
            style(axis)
            for y, (backend, measure, _) in zip(positions, bars):
                runs = per.get((harness, task, backend, measure))
                if not runs:
                    continue
                values = [mean(column) for column in zip(*runs)]
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
                axis.set_xlabel("seconds (mean over runs)")
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
        axis.set_xlabel("seconds after the answer (mean over all tasks)")
    stage_legend(figure, AFTER_STAGES, 1.1)
    figure.tight_layout()
    save(figure, out, "after-answer", record)


CALL_STAGES = [
    ("Bridge running its commands", STAGES[0][1]),
    ("Transport: harness, bridge and sandbox", STAGES[1][1]),
    ("Native call (no bridge)", STAGES[3][1]),
    ("Through ARIES, not split: the bridge also ran the harness's set-up", "#b8b7b1"),
]


def call_values(row, measure):
    """One agent tool call in milliseconds, in CALL_STAGES order; None when
    the run made no call."""
    calls = row.get(measure + "_harness_tool_calls")
    harness = row.get(measure + "_tool_exec_s")
    if not calls or harness is None:
        return None
    whole = harness * 1000 / calls
    if row["backend"] == "native":
        return [0, 0, whole, 0]
    if measure == "cold" and (row.get("cold_tool_calls") or 0) > (row.get("warm_tool_calls") or float("inf")):
        # More commands cold than warm: the harness's own set-up also went
        # through the bridge, outside any agent call (see task_reports.py).
        return [0, 0, 0, whole]
    bridge = (row.get(measure + "_tool_s") or 0) * 1000 / calls
    return [bridge, whole - bridge, 0, 0]


def stacked_panel(axis, per, bars, stages, unit, fmt):
    """Horizontal stacked bars at bars' positions, from per-run stage lists;
    returns the longest bar."""
    longest = 0
    for y, key, _ in bars:
        runs = per.get(key)
        if not runs:
            continue
        values = [mean(column) for column in zip(*runs)]
        total = sum(values)
        longest = max(longest, total)
        left = 0
        for (stage, color), value in zip(stages, values):
            if value > 0:
                axis.barh(y, value, left=left, height=0.82, color=color, edgecolor=SURFACE, linewidth=1)
            left += value
        axis.text(left, y, " " + fmt % total + unit, va="center", fontsize=7.5, color=INK, fontweight="bold")
    return longest


def segment_labels(axis, per, bars, stages, limit, fmt):
    for y, key, _ in bars:
        runs = per.get(key)
        if not runs:
            continue
        left = 0
        for value in (mean(column) for column in zip(*runs)):
            if value >= limit * 0.09:
                axis.text(left + value / 2, y, fmt % value, ha="center", va="center", fontsize=6.5, color=INK)
            left += value


def plot_task(rows, task, out, table, record):
    """One task's figures: its stages, cold and warm, with what follows the
    answer; and, when the agent called tools, what each call cost."""
    mine = [row for row in rows if row["task"] == task]
    if not mine:
        return
    bars = [(i * 2.4 + j, (backend, measure), "%s · %s" % (name, measure))
            for i, (backend, name) in enumerate(SETUPS) for j, measure in enumerate(("cold", "warm"))]
    after_bars = [(i, (backend, "after"), name) for i, (backend, name) in enumerate(SETUPS)]
    figure, axes = plt.subplots(2, len(HARNESSES), figsize=(13, 6.4), sharey="row",
                                gridspec_kw={"height_ratios": [2.3, 1]})
    for c, (harness, harness_label) in enumerate(HARNESSES):
        per = defaultdict(list)
        for row in mine:
            if row["harness"] != harness:
                continue
            for measure in ("cold", "warm"):
                if row.get(measure + "_latency_s") is not None:
                    per[(row["backend"], measure)].append(end_to_end_values(row, measure))
            per[(row["backend"], "after")].append(after_values(row))
        for (backend, measure), runs in per.items():
            names = AFTER_STAGES if measure == "after" else E2E_STAGES
            for (stage, _), value in zip(names, (mean(column) for column in zip(*runs))):
                table.append(["task-breakdown", harness, task, backend, measure + ": " + stage, round(value, 3)])
        for axis, panel, names, title, xlabel in (
                (axes[0][c], bars, E2E_STAGES, "%s · %s: cold and warm" % (harness_label, task),
                 "seconds until the answer; cold includes start-up (mean over runs)"),
                (axes[1][c], after_bars, AFTER_STAGES, "%s · %s: after the answer" % (harness_label, task),
                 "seconds after the answer (mean over runs)")):
            style(axis)
            limit = stacked_panel(axis, per, panel, names, " s", "%.1f") * 1.2 or 1
            segment_labels(axis, per, panel, names, limit, "%.1f")
            axis.set_xlim(0, limit)
            axis.set_yticks([y for y, _, _ in panel])
            axis.set_yticklabels([label for _, _, label in panel])
            axis.set_title(title, loc="left")
            axis.set_xlabel(xlabel)
    axes[0][0].invert_yaxis()
    axes[1][0].invert_yaxis()
    figure.tight_layout(h_pad=4)
    for row_axes, names in ((axes[0], E2E_STAGES), (axes[1], AFTER_STAGES)):
        top = row_axes[0].get_position().y1
        figure.legend([plt.Rectangle((0, 0), 1, 1, color=color) for _, color in names],
                      [stage for stage, _ in names], loc="lower center", ncol=len(names),
                      bbox_to_anchor=(0.5, top + 0.035), handlelength=1, handleheight=1, columnspacing=1.4)
    save(figure, out, task + "-breakdown", record)

    bars = [(i * 2.4 + j, (backend, measure), "%s · %s" % (name, measure))
            for i, (backend, name) in enumerate(SETUPS) for j, measure in enumerate(("cold", "warm"))]
    pers = {}
    for harness, _ in HARNESSES:
        per = defaultdict(list)
        for row in mine:
            if row["harness"] != harness:
                continue
            for measure in ("cold", "warm"):
                values = call_values(row, measure)
                if values is not None:
                    per[(row["backend"], measure)].append(values)
        pers[harness] = per
    if not any(pers.values()):
        return
    figure, axes = plt.subplots(1, len(HARNESSES), figsize=(13, 3.2), sharey=True)
    for c, (harness, harness_label) in enumerate(HARNESSES):
        axis = axes[c]
        style(axis)
        per = pers[harness]
        for (backend, measure), runs in per.items():
            for (stage, _), value in zip(CALL_STAGES, (mean(column) for column in zip(*runs))):
                if value:
                    table.append(["task-tool-calls", harness, task, backend, measure + ": " + stage + " (ms)",
                                  round(value, 1)])
        limit = stacked_panel(axis, per, bars, CALL_STAGES, " ms", "%.0f") * 1.22 or 1
        segment_labels(axis, per, bars, CALL_STAGES, limit, "%.0f")
        axis.set_xlim(0, limit)
        axis.set_yticks([y for y, _, _ in bars])
        axis.set_yticklabels([label for _, _, label in bars])
        axis.set_title("%s · %s" % (harness_label, task), loc="left")
        axis.set_xlabel("milliseconds per agent tool call (mean over runs)")
    axes[0].invert_yaxis()
    used = [stage for i, stage in enumerate(CALL_STAGES)
            if any(any(runs[i] for runs in per_runs) for per in pers.values() for per_runs in per.values())]
    handles = [plt.Rectangle((0, 0), 1, 1, color=color) for _, color in used]
    figure.legend(handles, [stage for stage, _ in used], loc="upper center", ncol=2 if len(used) > 3 else len(used),
                  bbox_to_anchor=(0.5, 1.14 if len(used) > 3 else 1.08), handlelength=1, handleheight=1,
                  columnspacing=1.4)
    figure.tight_layout()
    save(figure, out, task + "-tool-calls", record)


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    rows = load(sys.argv[1])
    out = sys.argv[2]
    os.makedirs(out, exist_ok=True)
    table, record = [], []
    plot_end_to_end(rows, out, table, record)
    plot_after(rows, out, table, record)
    tasks = os.path.join(out, "tasks")
    os.makedirs(tasks, exist_ok=True)
    for task in TASKS:
        plot_task(rows, task, tasks, table, record)
    with open(os.path.join(out, "figures.csv"), "w", newline="") as handle:
        writer = csv.writer(handle)
        writer.writerow(["figure", "harness", "task", "setup", "measure", "value"])
        writer.writerows(table)
    print("wrote " + ", ".join(record) + " to " + out)


if __name__ == "__main__":
    main()
