#!/usr/bin/env python3
"""Compare CompCert source clocks, APT transactions and first native attempts."""
import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import re

ATTEMPTS = {
    "1d11a94f": (1, 2), "921ac918": (1, 2), "bf3c1acf": (1, 2),
    "d116000a": (4, 5), "dc2b7aed": (1, 2),
}


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def apt(text):
    packages = re.search(r"The following NEW packages will be installed:\n((?:  [^\n]+\n)+)", text)
    header = re.search(r"\d+ upgraded, \d+ newly installed, \d+ to remove and \d+ not upgraded", text)
    versions = dict(re.findall(r"([A-Za-z][a-z0-9+.-]+) is already the newest version \(([^)]+)\)", text))
    if not packages or not header:
        raise ValueError("missing initial package transaction")
    return {"summary": header[0], "new": sorted(packages[1].split()), "reported_existing": versions}


def inspect(source, cache, attempts):
    atif, rollout = source / "trajectory.json", source / "rollout.jsonl"
    steps = json.loads(atif.read_bytes())["steps"]
    calls, outputs, source_request = {}, {}, {}
    for s in steps:
        for c in s.get("tool_calls", []):
            if c["function_name"] in {"exec_command", "write_stdin"}:
                calls[c["tool_call_id"]] = (s["step_id"], c)
        outputs.update({x["source_call_id"]: x["content"] for x in s.get("observation", {}).get("results", []) if x.get("source_call_id")})
    for row in map(json.loads, rollout.open()):
        p = row["payload"]
        if row["type"] == "response_item" and p.get("type") == "function_call":
            source_request[p["call_id"]] = datetime.fromisoformat(row["timestamp"])
    strict_no, diagnostic_no = ATTEMPTS[source.name[:8]]
    prefix = f"compile-compcert-{source.name}-"
    strict_path = attempts / f"{prefix}{strict_no:02}.json"
    diag_path = attempts / f"{prefix}{diagnostic_no:02}.json"
    strict = json.loads(strict_path.read_bytes())
    diag = json.loads(diag_path.read_bytes())
    logfile = cache / "logs" / f"{prefix}{diagnostic_no:02}.jsonl"
    if sha(logfile) != diag["diagnostic_log_sha256"]:
        raise ValueError(f"diagnostic log mismatch for {source.name}")
    if sha(atif) != strict["trajectory_sha256"] or sha(rollout) != strict["rollout_sha256"]:
        raise ValueError(f"source digest mismatch for {source.name}")
    events = list(map(json.loads, logfile.open()))
    responses = [r for r in events if r["event"] == "tool_reply"]
    req = {e["source_call_id"]: e for e in events if e["event"] == "tool_dispatch"}
    source_text = "\n".join(outputs.values())
    live_text = "\n".join(e.get("stdout","") + "\n" + e.get("stderr","") for e in responses)
    source_apt = apt(source_text)
    live_apt = apt(live_text)
    installed = dict(re.findall(r"∗ installed ([\w.-]+?)\.([0-9][\w+~.-]*)", source_text))
    final = next((e["checks"] for e in events if e["event"] == "final_task_state"), "")
    live_opam = {k: match[1] for k in installed
                 if (match := re.search(r"^" + re.escape(k) + r"\s+(\S+)\s*$", final, re.M))}
    opam = dict(source_packages=installed, readback=live_opam,
                differing_versions={k: [installed[k], live_opam[k]] for k in live_opam if installed[k] != live_opam[k]})
    reported_real = lambda z: [float(t) for t in re.findall(r"^real\s+([0-9.]+)\s*$", z, re.M)]
    process_origins = {}
    for cid, body in outputs.items():
        found = re.search(r"Process running with session ID (\d+)", body.split("\nOutput:\n", 1)[0])
        if cid in calls and calls[cid][1]["function_name"] == "exec_command" and found:
            process_origins[int(found[1])] = cid

    def difference(tool):
        cid = tool.get("source_call_id") or next(c for c,(step,call) in calls.items()
            if step == tool["step_id"] and c in req and req[c]["sequence"] == tool["sequence"])
        step, c = calls[cid]
        parsed = re.search(r"Wall time: ([\d.]+) seconds", outputs[cid])
        parent = process_origins.get(c["arguments"].get("session_id"))
        row = dict(step=step, sequence=tool["sequence"], call_id=cid,
                   operation=c["function_name"], requested_yield_ms=c["arguments"].get("yield_time_ms"),
                   original_wall_s=float(parsed[1]) if parsed else None,
                   original_status=tool["expected_exit_code"], live_status=tool["exit_code"],
                   live_wall_s=round(tool["duration_ns"]/1e9, 6))
        if parent:
            row["process_started_at_step"] = calls[parent][0]
            row["original_start_to_request_s"] = (source_request[cid]-source_request[parent]).total_seconds()
        return row

    def mismatches(receipt):
        return [difference(t) for t in receipt["tools"] if t["exit_code"] != t["expected_exit_code"]]

    # A terminal source exec uses the foreground runner. Flag any contribution
    # to elapsed background work that exceeds that tool's original yield budget.
    long_terminal = []
    polls = []
    for e in responses:
        cid = e["source_call_id"]
        step, c = calls[cid]
        elapsed = (e["monotonic_ns"] - req[cid]["monotonic_ns"])/1e9
        if e.get("operation") == "Exec":
            limit_ms = min(30000, max(250, c["arguments"].get("yield_time_ms", 10000)))
            if 1000 * elapsed > limit_ms:
                long_terminal.append({"step":step,"call_id":cid,"elapsed_s":elapsed,"yield_ms":limit_ms})
        if c["function_name"] == "write_stdin" and e["exit_code"] == e["expected_exit_code"] == -1:
            header = outputs[cid].split("\nOutput:\n",1)[0]
            wall = float(re.search(r"Wall time: ([\d.]+) seconds", header)[1])
            polls.append((max(5000, c["arguments"].get("yield_time_ms",0))/1000,wall,elapsed))
    return dict(trial=source.name, trajectory_sha256=sha(atif), rollout_sha256=sha(rollout),
        source_guest_calls=len(calls), original_APT=source_apt, old_live_APT=live_apt,
        extra_new_in_live=sorted(set(live_apt["new"])-set(source_apt["new"])),
        missing_new_in_live=sorted(set(source_apt["new"])-set(live_apt["new"])),
        strict_receipt=strict_path.name, observed_strict_calls=len(strict["tools"]), strict_differences=mismatches(strict),
        diagnostic_receipt=diag_path.name, diagnostic_calls=len(diag["tools"]), diagnostic_session_completed=diag["session_completed"],
        diagnostic_differences=mismatches(diag),
        original_usr_bin_time_real_s=reported_real(source_text), diagnostic_usr_bin_time_real_s=reported_real(live_text),
        installed_opam_validation=opam, matched_running_polls=len(polls),
        matched_running_polls_max_abs_source_minus_budget_s=round(max(abs(s-b) for b,s,l in polls),6),
        matched_running_polls_max_abs_live_minus_budget_s=round(max(abs(l-b) for b,s,l in polls),6),
        terminal_foreground_calls_exceeding_original_budget=long_terminal)


if __name__ == "__main__":
    cli = argparse.ArgumentParser(description=__doc__)
    cli.add_argument("cache", type=Path)
    cli.add_argument("attempts", type=Path)
    a = cli.parse_args()
    rows = [inspect(p,a.cache,a.attempts) for p in sorted((a.cache/"sources/compile-compcert").iterdir()) if p.is_dir()]
    assert len(rows)==len(ATTEMPTS)
    print(json.dumps(rows, indent=2))
