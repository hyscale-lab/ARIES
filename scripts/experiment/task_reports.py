"""Fine-grained per-task reports for scripts/experiment/report.py.

Each task gets tasks/<task>.md: for every harness, every start-up phase, the
cold and warm runs split into model time, tool execution and harness logic,
the harness's own overhead, the cost of each tool call as the harness and the
bridge timed it, what follows the answer, CPU and memory, and every
repetition. Values are means with their standard deviation over the valid
runs; the last columns are each ARIES setup's difference from native.
"""

import os
import statistics

SETUPS = [("native", "Native"), ("docker", "ARIES Docker"), ("k8s", "ARIES K8s")]
HARNESSES = [("openclaw", "OpenClaw"), ("hermes", "Hermes")]


# The bridge's measures; a native run has no bridge.
BRIDGE = {"cold_tool_calls", "warm_tool_calls", "cold_tool_s", "warm_tool_s"}


def value(row, key):
    """A measure of one task run: a column of tasks.csv, or one derived."""
    if row.get("backend") == "native" and (key in BRIDGE or "bridge" in key or "transport" in key):
        return None
    if key in DERIVED:
        return DERIVED[key](row)
    return row.get(key)


def per_call(row, measure, total, calls):
    count = row.get("%s_%s" % (measure, calls))
    seconds = row.get("%s_%s" % (measure, total))
    return seconds * 1000 / count if count and seconds is not None else None


def split_known(row, measure):
    """Whether every command the bridge ran belongs to an agent tool call.
    A cold run that ran more commands than its warm run also ran the
    harness's own set-up through the bridge, such as Hermes's first-use
    checks, so its bridge time cannot be divided over the agent's calls."""
    if measure != "cold" or row.get("warm_tool_calls") is None:
        return True
    return (row.get("cold_tool_calls") or 0) <= row["warm_tool_calls"]


def bridge_per_call(row, measure):
    return per_call(row, measure, "tool_s", "harness_tool_calls") if split_known(row, measure) else None


def transport(row, measure):
    """Per agent tool call, the harness-timed execution beyond the time the
    bridge spent running commands for it: the round trips between harness,
    bridge and sandbox."""
    harness = per_call(row, measure, "tool_exec_s", "harness_tool_calls")
    bridge = bridge_per_call(row, measure)
    return harness - bridge if harness is not None and bridge is not None else None


def overhead(row, measure):
    logic = row.get(measure + "_harness_logic_s")
    if logic is None:
        return None
    return logic + ((row.get("harness_start_s") or 0) if measure == "cold" else 0)


DERIVED = {
    "after_answer_s": lambda r: sum(r.get(k) or 0 for k in ("harness_collect_s", "isolation_s", "evaluation_s", "cleanup_s")),
}
for _measure in ("cold", "warm"):
    DERIVED[_measure + "_harness_overhead_s"] = lambda r, m=_measure: overhead(r, m)
    DERIVED[_measure + "_harness_ms_per_call"] = lambda r, m=_measure: per_call(r, m, "tool_exec_s", "harness_tool_calls")
    DERIVED[_measure + "_bridge_ms_per_call"] = lambda r, m=_measure: bridge_per_call(r, m)
    DERIVED[_measure + "_transport_ms_per_call"] = lambda r, m=_measure: transport(r, m)

# (key, label, unit); a None key starts a group, with the label as its name.
COLD = [
    (None, "Start-up, until the first request", ""),
    ("sandbox_reserve_s", "Sandbox reservation (`sandbox_reserve`)", "s"),
    ("bridge_reserve_s", "Bridge endpoint reservation (`bridge_reserve`)", "s"),
    ("harness_prepare_s", "Harness container or pod creation (`harness_prepare`)", "s"),
    ("harness_start_s", "Harness start, until it reports ready (`harness_start`)", "s"),
    ("sandbox_start_s", "*Alongside:* sandbox start (`sandbox_start`)", "s"),
    ("sandbox_prepare_s", "*Alongside:* sandbox sanitising (`sandbox_prepare`)", "s"),
    ("bridge_register_s", "*Alongside:* bridge binding the sandbox (`bridge_register`)", "s"),
    ("cold_startup_s", "**Start-up to the first request**", "s"),
    (None, "Cold run, from the request to the answer", ""),
    ("cold_model_s", "Model (replayed)", "s"),
    ("cold_tool_exec_s", "Tool execution, as the harness timed it", "s"),
    ("cold_harness_logic_s", "Harness logic", "s"),
    ("cold_run_s", "**Cold run**", "s"),
    ("cold_latency_s", "**Cold latency: start-up and run**", "s"),
    ("cold_harness_overhead_s", "Harness overhead: harness start and logic", "s"),
    ("cold_model_calls", "Model calls the agent waited on", ""),
]
WARM = [
    (None, "Warm run, the same task on the running harness and sandbox", ""),
    ("warm_model_s", "Model (replayed)", "s"),
    ("warm_tool_exec_s", "Tool execution, as the harness timed it", "s"),
    ("warm_harness_logic_s", "Harness logic, its overhead when warm", "s"),
    ("warm_latency_s", "**Warm latency**", "s"),
    ("warm_model_calls", "Model calls the agent waited on", ""),
]
TOOLS = [
    (None, "Tool calls, cold run", ""),
    ("cold_harness_tool_calls", "Agent tool calls", ""),
    ("cold_tool_calls", "Commands the bridge ran for them", ""),
    ("cold_harness_ms_per_call", "Per call, as the harness timed it", "ms"),
    ("cold_bridge_ms_per_call", "Per call, the bridge running its commands", "ms"),
    ("cold_transport_ms_per_call", "Per call, transport between harness and sandbox", "ms"),
    (None, "Tool calls, warm run", ""),
    ("warm_harness_tool_calls", "Agent tool calls", ""),
    ("warm_tool_calls", "Commands the bridge ran for them", ""),
    ("warm_harness_ms_per_call", "Per call, as the harness timed it", "ms"),
    ("warm_bridge_ms_per_call", "Per call, the bridge running its commands", "ms"),
    ("warm_transport_ms_per_call", "Per call, transport between harness and sandbox", "ms"),
]
AFTER = [
    (None, "After the answer", ""),
    ("harness_collect_s", "Collect the harness's artifacts (`harness_collect`)", "s"),
    ("isolation_s", "Stop the harness, revoke bridge access (`isolation`)", "s"),
    ("evaluation_s", "Evaluate (`evaluation`)", "s"),
    ("cleanup_s", "Clean up (`cleanup`)", "s"),
    ("after_answer_s", "**After the answer**", "s"),
]
RESOURCES = [
    (None, "Resources, over the whole task", ""),
    ("harness_cpu_mean", "Harness CPU, mean (% of one core)", "%"),
    ("harness_cpu_peak", "Harness CPU, peak (% of one core)", "%"),
    ("harness_memory_peak_mib", "Harness memory, peak (MiB)", "MiB"),
    ("sandbox_cpu_mean", "Sandbox CPU, mean (% of one core)", "%"),
    ("sandbox_cpu_peak", "Sandbox CPU, peak (% of one core)", "%"),
    ("sandbox_memory_peak_mib", "Sandbox memory, peak (MiB)", "MiB"),
]


def numbers(rows, key):
    return [v for v in (value(row, key) for row in rows) if v is not None]


def cell(values, unit):
    if not values:
        return "–"
    digits = {"ms": 0, "%": 0, "MiB": 0, "": 1}.get(unit, 2)
    mean = statistics.fmean(values)
    if len(values) < 2:
        return "%.*f" % (digits, mean)
    return "%.*f ± %.*f" % (digits, mean, digits, statistics.stdev(values))


def difference(values, base, unit):
    if not values or not base:
        return "–"
    digits = {"ms": 0, "%": 0, "MiB": 0, "": 1}.get(unit, 2)
    return "%+.*f" % (digits, statistics.fmean(values) - statistics.fmean(base))


def table(rows_by_setup, spec_rows):
    setups = [(s, name) for s, name in SETUPS if rows_by_setup.get(s)]
    others = [(s, name) for s, name in setups if s != "native"]
    has_native = bool(rows_by_setup.get("native"))
    heads = [name for _, name in setups] + (["%s − native" % name for _, name in others] if has_native else [])
    out = ["| Measure | " + " | ".join(heads) + " |", "|---|" + "---|" * len(heads)]
    for key, label, unit in spec_rows:
        if key is None:
            out.append("| ***%s*** |%s" % (label, " |" * len(heads)))
            continue
        cells = [cell(numbers(rows_by_setup[s], key), unit) for s, _ in setups]
        if not any(c != "–" for c in cells):
            continue
        if has_native:
            base = numbers(rows_by_setup["native"], key)
            cells += [difference(numbers(rows_by_setup[s], key), base, unit) for s, _ in others]
        out.append("| %s | %s |" % (label, " | ".join(cells)))
    return out


def repetitions(rows_by_setup, warm):
    setups = [(s, name) for s, name in SETUPS if rows_by_setup.get(s)]
    count = max(len(rows) for rows in rows_by_setup.values())
    heads = []
    for _, name in setups:
        heads += ["%s cold" % name] + (["%s warm" % name] if warm else [])
    out = ["| Repetition | " + " | ".join(heads) + " |", "|---|" + "---|" * len(heads)]
    for index in range(count):
        cells = []
        for setup, _ in setups:
            rows = sorted(rows_by_setup[setup], key=lambda r: r.get("repetition") or 0)
            row = rows[index] if index < len(rows) else None
            for key in ("cold_latency_s", "warm_latency_s") if warm else ("cold_latency_s",):
                cells.append("%.2f" % row[key] if row and row.get(key) is not None else "–")
        out.append("| %d | %s |" % (index + 1, " | ".join(cells)))
    return out


def figures(campaign, task):
    """Links the task's figures that scripts/microbench/plot_coldwarm.py drew."""
    out = []
    for name, title, alt in (
            ("breakdown", "Time breakdown", "Cold and warm stages, and what follows the answer, per setup"),
            ("tool-calls", "Cost of a tool call", "Each agent tool call: the bridge's commands and the transport")):
        path = "figures/tasks/%s-%s.png" % (task, name)
        if os.path.exists(os.path.join(campaign, path)):
            out += ["### %s" % title, "", "![%s](../%s)" % (alt, path), ""]
    return (["## Figures", "", "PDFs are beside the PNGs in `../figures/tasks/`.", ""] + out) if out else []


def write(campaign, tasks, spec, descriptions):
    """Writes tasks/<task>.md for every task and returns their paths,
    relative to the campaign."""
    directory = os.path.join(campaign, "tasks")
    os.makedirs(directory, exist_ok=True)
    written = []
    warm = spec.get("warm")
    for task in spec["tasks"]:
        mine = [row for row in tasks if row["task"] == task]
        if not mine:
            continue
        out = ["# `%s`: time breakdown" % task, "",
               "*Generated by `scripts/experiment/report.py`; back to the [campaign report](../report.md).*", ""]
        if task in descriptions:
            agent, checks, isolates = descriptions[task]
            out += ["The agent %s; the verifier checks %s. It isolates %s." % (agent, checks, isolates), ""]
        out += ["Means ± standard deviation over the valid runs, in seconds unless marked; the last columns are "
                "each ARIES setup's mean minus native's.",
                "",
                "- **Start-up:** the phases before the first request. The harness starts while the sandbox "
                "starts, so the *alongside* phases overlap the harness start, and start-up is less than their sum.",
                "- **Run:** model time is how long a request offering tools was open. Tool execution is timed by "
                "the harness, the same way in every setup: Hermes's tool spans, or OpenClaw's session log from the "
                "tool-calling reply to the tool's result. Harness logic is the rest of the run: the harness's own "
                "work.",
                "- **Harness overhead:** the harness's own time, beside the model and the tools. Cold, it is the "
                "harness start plus the cold run's harness logic; warm, the warm run's harness logic.",
                "- **Tool calls:** the bridge runs more commands than the agent makes calls, because the harness "
                "also runs its own, such as OpenClaw's workspace sync, so its time is divided by the agent's calls. "
                "**Transport** is the harness-timed time per call minus the bridge's: the round trips between "
                "harness, bridge and sandbox. Native runs have no bridge. A cold run that ran more bridge commands "
                "than its warm run also ran the harness's own set-up through the bridge, outside any agent call, so "
                "its bridge share and transport are left out (–).",
                ""]
        out += figures(campaign, task)
        for harness, label in HARNESSES:
            rows = [row for row in mine if row["harness"] == harness]
            if not rows:
                continue
            by_setup = {setup: [row for row in rows if row["backend"] == setup] for setup, _ in SETUPS}
            counts = ", ".join("%s %d" % (name, len(by_setup[s])) for s, name in SETUPS if by_setup[s])
            out += ["## %s" % label, "", "Runs: %s." % counts, ""]
            out += table(by_setup, COLD + (WARM if warm else [])) + [""]
            out += ["### Tool calls", ""] + table(by_setup, TOOLS) + [""]
            out += ["### After the answer and resources", ""] + table(by_setup, AFTER + RESOURCES) + [""]
            out += ["### Each repetition (latency, seconds)", ""] + repetitions(by_setup, warm) + [""]
        path = os.path.join(directory, task + ".md")
        with open(path, "w") as handle:
            handle.write("\n".join(out))
        written.append(os.path.relpath(path, campaign))
    return written
