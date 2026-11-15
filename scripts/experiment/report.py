#!/usr/bin/env python3
"""Analyses a campaign's valid runs and writes its report.

    python3 scripts/experiment/report.py experiments/coldwarm/results/NAME [--no-plots]

Reads the campaign's manifest.tsv (scripts/experiment/run.py), keeps the runs
whose status is valid, and writes:

  analysis/tasks.csv     one row per task run (scripts/microbench/analyze.py)
  analysis/analysis.md   analyze.py's summary of means per variant
  figures/               the cold and warm figures, with figures.csv
                         (scripts/microbench/plot_coldwarm.py, warm experiments)
  recordings/*.md        each recording decoded (scripts/microbench/decode_recordings.py)
  report.md              high level: setup, stages, figures, headline results
                         and overhead against native, run counts,
                         provenance, then the campaign's notes.md
  tasks/<task>.md        fine grained, per task: every phase, model, tool
                         execution, harness logic and overhead, per-call tool
                         cost, after the answer, resources, each repetition

report.md is generated: write interpretation in notes.md beside it, which is
copied in. Every table is drawn from analysis/tasks.csv, so rerunning after a
mark or a collect updates it.
"""

import argparse
import csv
import glob
import json
import os
import subprocess
import sys
from collections import defaultdict

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import task_reports  # noqa: E402

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SCRIPTS = os.path.join(ROOT, "scripts", "microbench")
SETUPS = [("native", "Native"), ("docker", "ARIES Docker"), ("k8s", "ARIES K8s")]
HARNESSES = [("openclaw", "OpenClaw"), ("hermes", "Hermes")]
ANALYSE_COLUMNS = ["harness", "backend", "mode", "repetition", "run_dir", "requests", "variant"]


def read_tsv(path):
    with open(path) as handle:
        return list(csv.DictReader(handle, delimiter="\t"))


def number(text):
    try:
        return float(text)
    except (TypeError, ValueError):
        return None


def mean(values):
    values = [v for v in values if v is not None]
    return sum(values) / len(values) if values else None


def fmt(value):
    return "" if value is None else "%.2f" % value


def analyse(campaign, rows):
    """Runs analyze.py over the valid runs, from analysis/, so the manifest it
    reads holds paths relative to the campaign."""
    analysis = os.path.join(campaign, "analysis")
    os.makedirs(analysis, exist_ok=True)
    with open(os.path.join(analysis, "analyze-manifest.tsv"), "w") as handle:
        handle.write("\t".join(ANALYSE_COLUMNS) + "\n")
        for row in rows:
            values = dict(row, backend=row["setup"],
                          run_dir=os.path.join("..", row["run_dir"]),
                          requests=os.path.join("..", row["requests"]) if row["requests"] else "")
            handle.write("\t".join(values.get(column, "") for column in ANALYSE_COLUMNS) + "\n")
    with open(os.path.join(analysis, "analysis.md"), "w") as out:
        subprocess.run([sys.executable, os.path.join(SCRIPTS, "analyze.py"), "analyze-manifest.tsv"],
                       cwd=analysis, stdout=out, check=True)
    with open(os.path.join(analysis, "tasks.csv")) as handle:
        tasks = [row for row in csv.DictReader(handle) if row["mode"] == "replay"]
    for row in tasks:
        for key in list(row):
            if key not in ("harness", "backend", "mode", "task", "error", "variant"):
                row[key] = number(row[key])
    return tasks


def stage_values(row, measure):
    """A cold or warm run split as in the end-to-end figure: start-up not
    spent starting the harness, harness start, model, tool execution, and
    harness logic, which add up to the latency."""
    model = row.get(measure + "_model_s") or 0
    tools = row.get(measure + "_tool_exec_s") or 0
    run = row.get(measure + "_run_s")
    if run is None:
        return None
    values = [model, tools, run - model - tools]
    if measure == "cold":
        harness = row.get("harness_start_s") or 0
        return [(row.get("cold_startup_s") or 0) - harness, harness] + values
    return values


SETUP_TEXT = {
    "native": "The harness's own Docker sandbox, with no ARIES involved: the baseline.",
    "docker": "ARIES creates the sandbox as a Docker container; the harness reaches it through ARIES's tool bridge.",
    "k8s": "The same, with the harness, sandbox and bridge as Kubernetes pods.",
}


def task_descriptions():
    """The microbenchmark README's task table: what the agent does, what the
    verifier checks, and what the task isolates."""
    descriptions = {}
    path = os.path.join(ROOT, "benchmarks", "microbench", "README.md")
    if os.path.exists(path):
        for line in open(path):
            cells = [c.strip() for c in line.strip().strip("|").split("|")]
            if len(cells) == 4 and cells[0].startswith("`mb-"):
                descriptions[cells[0].strip("`")] = tuple(cells[1:])
    return descriptions


def resources():
    """The comparison profiles' harness and sandbox limits."""
    path = os.path.join(ROOT, "profiles", "comparison", "fixed-2cpu-4gib-overrides.json")
    if not os.path.exists(path):
        return None
    with open(path) as handle:
        overrides = json.load(handle)
    harness, sandbox = overrides.get("harness_resources", {}), overrides.get("agent_sandbox_resources", {})
    return "harness: %s CPUs, %s MiB; sandbox: %s CPUs, %s MiB" % (
        harness.get("cpu"), harness.get("memory_mb"), sandbox.get("cpu"), sandbox.get("memory_mb"))


def overview_section(meta, spec, descriptions):
    out = ["## Setup", "", spec.get("description", ""), "", "| Setup | How tools run |", "|---|---|"]
    out += ["| **%s** | %s |" % (name, SETUP_TEXT[setup]) for setup, name in SETUPS if setup in spec["setups"]]
    workers = meta.get("workers") or {}
    cpus = sorted({"%s, %s CPUs, SMT %s, turbo %s" % (w.get("cpu", "?"), w.get("cpus", "?"), w.get("smt", "?"),
                                                     w.get("turbo", "?")) for w in workers.values()})
    out += ["", "**Controls**",
            "- **Hardware:** %s. Each harness runs on a worker of its own, and every setup of a harness on that "
            "worker." % ("; ".join(cpus) or "not recorded"),
            "- **Resources:** %s." % (resources() or "as in profiles/comparison"),
            "- **Model:** each harness's replies were recorded once and replayed with their original timing, "
            "so model time is the same in every setup.",
            "- **Resource measurement:** CPU and memory come from cAdvisor through Prometheus, sampled every "
            "second, the same way in every setup.",
            "- **Runs:** %d repetitions of every setup, each task run cold%s." % (
                spec["repetitions"], " and then warm on the same harness and sandbox" if spec.get("warm") else "")]
    if descriptions:
        out += ["", "| Task | The agent | Isolates |", "|---|---|---|"]
        out += ["| `%s` | %s | %s |" % (task, descriptions[task][0], descriptions[task][2])
                for task in spec["tasks"] if task in descriptions]
    return out


STAGES_TEXT = [
    "## Stages of a task", "",
    "- **Cold** is the first request on a fresh harness and sandbox, from the first start-up step to the answer:",
    "  - **Start-up:** the sandbox and its bridge endpoint are reserved, the harness's container or pod is "
    "created and the harness started, while the sandbox starts and the bridge binds it.",
    "  - **Harness start:** launching the harness and waiting until it reports ready, usually most of start-up.",
    "  - **Model:** how long the agent waited on the (replayed) model.",
    "  - **Tool execution:** the tool calls as the harness timed them, the trip to the sandbox included.",
    "  - **Harness logic:** the rest, the harness's own work. With the harness start, it is the harness's overhead.",
    "- **Warm** is the same task again on the running harness and sandbox: model, tool execution and harness logic.",
    "- **After the answer**, which the user does not wait for: collecting the harness's artifacts, stopping it, "
    "evaluating and cleaning up.",
]


def means_of(rows, key):
    values = [row.get(key) for row in rows if row.get(key) is not None]
    return sum(values) / len(values) if values else None


def results_section(tasks, spec):
    warm = spec.get("warm")
    out = ["## Results", "", "Means over valid runs, in seconds. Each task's full breakdown is in its own report "
           "(below).", ""]
    present = [(s, n) for s, n in SETUPS if any(r["backend"] == s for r in tasks)]
    for harness, label in HARNESSES:
        mine = [row for row in tasks if row["harness"] == harness]
        if not mine:
            continue
        heads = ["%s cold" % n for _, n in present] + (["%s warm" % n for _, n in present] if warm else [])
        out += ["### %s" % label, "", "| Task | " + " | ".join(heads) + " |", "|---|" + "---|" * len(heads)]
        for task in spec["tasks"]:
            cells = []
            for key in ("cold_latency_s", "warm_latency_s") if warm else ("cold_latency_s",):
                cells += [fmt(means_of([r for r in mine if r["task"] == task and r["backend"] == s], key))
                          for s, _ in present]
            out.append("| [`%s`](tasks/%s.md) | %s |" % (task, task, " | ".join(cells)))
        out.append("")
    if not any(s == "native" for s, _ in present):
        return out
    out += ["### Overhead against native", "",
            "Each ARIES setup's mean minus native's, averaged over the tasks. Per tool call is the warm "
            "`mb-ten-calls` run's harness-timed tool execution divided by its calls.", "",
            "| Harness | Setup | Start-up | Cold latency | Warm latency | Per tool call (ms) | Warm harness logic |",
            "|---|---|---|---|---|---|---|"]
    for harness, label in HARNESSES:
        mine = [row for row in tasks if row["harness"] == harness]
        if not mine:
            continue
        for setup, name in present:
            def delta(key):
                pairs = [(means_of([r for r in mine if r["task"] == t and r["backend"] == setup], key),
                          means_of([r for r in mine if r["task"] == t and r["backend"] == "native"], key))
                         for t in spec["tasks"]]
                pairs = [a - b for a, b in pairs if a is not None and b is not None]
                return "%+.2f" % (sum(pairs) / len(pairs)) if pairs and setup != "native" else (
                    "–" if setup != "native" else "")
            # mb-ten-calls isolates the cost per call; mb-cpu's one long call
            # would swamp an average over every task.
            calls = [r for r in mine if r["backend"] == setup and r["task"] == "mb-ten-calls"
                     and (r.get("warm_harness_tool_calls") or 0) > 0]
            per_call = (sum(r["warm_tool_exec_s"] for r in calls) * 1000 / sum(r["warm_harness_tool_calls"] for r in calls)
                        if calls else None)
            logic = means_of([r for r in mine if r["backend"] == setup], "warm_harness_logic_s")
            out.append("| %s | %s | %s | %s | %s | %s | %s |" % (
                label, name,
                fmt(means_of([r for r in mine if r["backend"] == setup], "cold_startup_s")) if setup == "native" else delta("cold_startup_s"),
                "" if setup == "native" else delta("cold_latency_s"),
                "" if setup == "native" or not warm else delta("warm_latency_s"),
                "%.0f" % per_call if per_call is not None else "–", fmt(logic)))
    out += ["", "The native row's start-up column is native's own start-up; the other rows are differences from "
            "it. Per tool call and warm harness logic are absolute values in every row.", ""]
    return out


def provenance_section(meta, spec):
    out = ["## Provenance", "",
           "| | |", "|---|---|",
           "| Experiment spec | `%s` |" % meta.get("spec", ""),
           "| ARIES commit | `%s`%s |" % (meta.get("aries_commit", "unknown"), " (uncommitted changes)" if meta.get("aries_dirty") else ""),
           "| Bridge image | `%s` |" % meta.get("bridge_image", "unknown"),
           "| vHive setup tool | `%s` |" % (meta.get("vhive_revision") or "not used"),
           "| Started, finished | %s, %s |" % (meta.get("started_at") or "?", meta.get("finished_at") or "not finished"),
           "| Harnesses | %s |" % ", ".join(spec["harnesses"]),
           "| Setups | %s |" % ", ".join(spec["setups"]),
           "| Tasks | %s |" % ", ".join("`%s`" % t for t in spec["tasks"]),
           "| Repetitions | %d, each task run cold%s |" % (spec["repetitions"], " then warm" if spec.get("warm") else ""),
           "| Recordings | %s |" % meta.get("recordings", {}).get("source", "?")]
    for harness, digest in sorted(meta.get("recordings", {}).get("sha256", {}).items()):
        out.append("| Recording `%s` | sha256 `%s` |" % (harness, digest[:16]))
    if meta.get("note"):
        out.append("| Note | %s |" % meta["note"])
    workers = meta.get("workers") or {}
    if workers:
        out += ["", "| Worker | Harnesses | ARIES commit | CPU | CPUs | Memory (GiB) | SMT | Turbo | Kernel | Kubelet | Docker |",
                "|---|---|---|---|---|---|---|---|---|---|---|"]
        for label, facts in sorted(workers.items()):
            commit = facts.get("aries_commit") or meta.get("aries_commit") or "?"
            out.append("| %s | %s | `%s` | %s | %s | %s | %s | %s | %s | %s | %s |" % (
                label, ", ".join(facts.get("harnesses", [])), commit[:12], facts.get("cpu", "?"), facts.get("cpus", "?"),
                facts.get("memory_gib", "?"), facts.get("smt", "?"), facts.get("turbo", "?"), facts.get("kernel", "?"),
                facts.get("kubelet", "?"), facts.get("docker", "?")))
    return out


def runs_section(manifest, tasks):
    out = ["## Runs", "", "Runs per harness and setup, by status. Only valid runs are analysed.", "",
           "| Harness | Setup | Valid | Failed | Superseded | Task runs solved |", "|---|---|---|---|---|---|"]
    counts = defaultdict(lambda: defaultdict(int))
    for row in manifest:
        if row["mode"] != "record":
            counts[(row["harness"], row["setup"])][row["status"]] += 1
    for harness, label in HARNESSES:
        for setup, name in SETUPS:
            if (harness, setup) not in counts:
                continue
            mine = [row for row in tasks if row["harness"] == harness and row["backend"] == setup]
            solved = sum(1 for row in mine if row.get("reward") == 1)
            c = counts[(harness, setup)]
            out.append("| %s | %s | %d | %d | %d | %d/%d |" % (label, name, c["valid"], c["failed"], c["superseded"],
                                                              solved, len(mine)))
    excluded = [row for row in manifest if row["status"] != "valid"]
    if excluded:
        out += ["", "Runs left out:", ""]
        out += ["- `%s` (%s): %s" % (row["run_dir"] or "%s/%s/%s" % (row["harness"], row["setup"], row["repetition"]),
                                     row["status"], row["reason"] or "no reason given") for row in excluded]
    misses = sum((row.get("cold_model_misses") or 0) + (row.get("warm_model_misses") or 0) for row in tasks)
    out += ["", "Model requests not answered from the recording: %d." % misses]
    return out


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("campaign")
    parser.add_argument("--no-plots", action="store_true", help="skip the figures, which need matplotlib")
    args = parser.parse_args()
    campaign = os.path.abspath(args.campaign)
    with open(os.path.join(campaign, "meta.json")) as handle:
        meta = json.load(handle)
    with open(os.path.join(campaign, "experiment.json")) as handle:
        spec = json.load(handle)
    manifest = read_tsv(os.path.join(campaign, "manifest.tsv"))
    valid = [row for row in manifest if row["status"] == "valid"]
    if not valid:
        sys.exit("no valid runs in " + os.path.join(campaign, "manifest.tsv"))
    tasks = analyse(campaign, valid)

    figures = os.path.join(campaign, "figures")
    plotted = False
    if spec.get("warm") and not args.no_plots:
        subprocess.run([sys.executable, os.path.join(SCRIPTS, "plot_coldwarm.py"),
                        os.path.join(campaign, "analysis", "tasks.csv"), figures], check=True)
        plotted = True
    for path in sorted(glob.glob(os.path.join(campaign, "recordings", "*.jsonl"))):
        subprocess.run([sys.executable, os.path.join(SCRIPTS, "decode_recordings.py"), path, "-o", path[:-len(".jsonl")] + ".md"],
                       check=True, stderr=subprocess.DEVNULL)

    descriptions = task_descriptions()
    task_pages = task_reports.write(campaign, tasks, spec, descriptions)
    out = ["# %s: campaign %s" % (spec["name"], meta["campaign"]), "",
           "*Generated by `scripts/experiment/report.py` from this campaign's runs; edit `notes.md`, not this file.*", ""]
    out += overview_section(meta, spec, descriptions) + [""]
    out += STAGES_TEXT + [""]
    if plotted:
        out += ["## Figures", "", "PDFs and every plotted number (`figures.csv`) are beside the PNGs in `figures/`.", "",
                "### End-to-end time, cold and warm", "", "![Cold and warm end-to-end time per setup and task](figures/end-to-end.png)", "",
                "### After the answer", "", "![Time after the answer per setup](figures/after-answer.png)", ""]
    out += results_section(tasks, spec)
    out += ["## Per-task reports", "",
            "Every start-up phase, the cold and warm runs split into model, tool execution and harness logic, the "
            "harness's overhead, each tool call as the harness and the bridge timed it, what follows the answer, "
            "CPU and memory, and every repetition:", ""]
    out += ["- [`%s`](%s)" % (os.path.splitext(os.path.basename(page))[0], page) for page in task_pages] + [""]
    out += runs_section(manifest, tasks) + [""]
    out += provenance_section(meta, spec) + [""]
    out += ["The full per-variant analysis is in `analysis/analysis.md`, one row per task run in `analysis/tasks.csv`.", ""]
    notes = os.path.join(campaign, "notes.md")
    if os.path.exists(notes):
        with open(notes) as handle:
            text = handle.read().strip()
        # notes.md is copied as written, so its headings start at ###.
        out += ["## Notes", "", text, ""]
    with open(os.path.join(campaign, "report.md"), "w") as handle:
        handle.write("\n".join(out))
    print("wrote " + os.path.relpath(os.path.join(campaign, "report.md"), os.getcwd()))


if __name__ == "__main__":
    main()
