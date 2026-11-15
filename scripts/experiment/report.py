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
  report.md              setup, run counts, figures and stage tables, then
                         the campaign's notes.md

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


def stage_tables(tasks, spec_tasks, warm):
    out = []
    cold_heads = ["Cold: sandbox, bridge, container", "Cold: harness start", "Cold: model", "Cold: tool execution",
                  "Cold: harness logic", "**Cold total**"]
    warm_heads = ["Warm: model", "Warm: tool execution", "Warm: harness logic", "**Warm total**"] if warm else []
    for harness, label in HARNESSES:
        mine = [row for row in tasks if row["harness"] == harness]
        if not mine:
            continue
        out += ["### %s" % label, "", "| Task | Setup | " + " | ".join(cold_heads + warm_heads) + " |",
                "|---|---|" + "---|" * (len(cold_heads) + len(warm_heads))]
        for task in spec_tasks:
            first = True
            for setup, name in SETUPS:
                runs = [row for row in mine if row["task"] == task and row["backend"] == setup]
                if not runs:
                    continue
                cells = []
                for measure in ("cold", "warm") if warm else ("cold",):
                    stages = [values for values in (stage_values(row, measure) for row in runs) if values]
                    means = [mean(column) for column in zip(*stages)] if stages else []
                    width = 5 if measure == "cold" else 3
                    means = means or [None] * width
                    total = sum(m for m in means if m is not None) if stages else None
                    cells += [fmt(m) for m in means] + ["**%s**" % fmt(total)]
                out.append("| %s | %s | %s |" % ("`%s`" % task if first else "", "%s (%d)" % (name, len(runs)),
                                                 " | ".join(cells)))
                first = False
        out.append("")
    return out


def after_table(tasks):
    keys = [("harness_collect_s", "Collect artifacts"), ("isolation_s", "Stop harness, revoke bridge access"),
            ("evaluation_s", "Evaluate"), ("cleanup_s", "Clean up")]
    out = ["| Harness | Setup | " + " | ".join(name for _, name in keys) + " | **Total** |",
           "|---|---|" + "---|" * (len(keys) + 1)]
    for harness, label in HARNESSES:
        for setup, name in SETUPS:
            runs = [row for row in tasks if row["harness"] == harness and row["backend"] == setup]
            if not runs:
                continue
            means = [mean(row.get(key) for row in runs) for key, _ in keys]
            out.append("| %s | %s | %s | **%s** |" % (label, name, " | ".join(fmt(m) for m in means),
                                                     fmt(sum(m for m in means if m is not None))))
    return out


def startup_table(tasks):
    keys = [("sandbox_reserve_s", "Sandbox reservation (`sandbox_reserve`)"),
            ("bridge_reserve_s", "Bridge reservation (`bridge_reserve`)"),
            ("harness_prepare_s", "Harness container or pod creation (`harness_prepare`)"),
            ("harness_start_s", "Harness start, until ready (`harness_start`)"),
            ("startup_other_s", "*Alongside:* sandbox start, preparation and bridge binding"),
            ("cold_startup_s", "**Start-up to the first request**")]
    columns = [(h, hl, s, sl) for h, hl in HARNESSES for s, sl in SETUPS
               if any(row["harness"] == h and row["backend"] == s for row in tasks)]
    out = ["| Stage | " + " | ".join("%s: %s" % (hl, sl) for _, hl, _, sl in columns) + " |",
           "|---|" + "---|" * len(columns)]
    for key, name in keys:
        cells = [fmt(mean(row.get(key) for row in tasks if row["harness"] == h and row["backend"] == s))
                 for h, _, s, _ in columns]
        out.append("| %s | %s |" % (name, " | ".join(cells)))
    return out


def setup_section(meta, spec):
    out = ["## Setup", "", spec.get("description", ""), "",
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
        out += ["", "| Worker | Harnesses | CPU | CPUs | Memory (GiB) | SMT | Turbo | Kernel | Kubelet | Docker |",
                "|---|---|---|---|---|---|---|---|---|---|"]
        for label, facts in sorted(workers.items()):
            out.append("| %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |" % (
                label, ", ".join(facts.get("harnesses", [])), facts.get("cpu", "?"), facts.get("cpus", "?"),
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

    out = ["# %s: campaign %s" % (spec["name"], meta["campaign"]), "",
           "*Generated by `scripts/experiment/report.py` from this campaign's runs; edit `notes.md`, not this file.*", ""]
    out += setup_section(meta, spec) + [""]
    out += runs_section(manifest, tasks) + [""]
    if plotted:
        out += ["## Figures", "", "PDFs and every plotted number (`figures.csv`) are beside the PNGs in `figures/`.", "",
                "### End-to-end time, cold and warm", "", "![Cold and warm end-to-end time per setup and task](figures/end-to-end.png)", "",
                "### After the answer", "", "![Time after the answer per setup](figures/after-answer.png)", ""]
    out += ["## Stage breakdown", "",
            "Means over valid runs, in seconds; the number of runs is beside each setup. Cold runs from the first "
            "start-up step to the first answer; warm is the same task again on the running harness and sandbox. "
            "\"Sandbox, bridge, container\" is the start-up not spent starting the harness. Tool execution is timed "
            "by the harness, the same way in every setup; harness logic is the rest of the run beside model time.", ""]
    out += stage_tables(tasks, spec["tasks"], spec.get("warm"))
    out += ["### Start-up stages", ""] + startup_table(tasks) + [""]
    out += ["### After the answer (mean over all tasks)", ""] + after_table(tasks) + [""]
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
