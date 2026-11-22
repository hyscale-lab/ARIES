#!/usr/bin/env python3
"""Compare foreground host dispatch-to-reply envelopes to the original Codex yield."""
import argparse
import hashlib
import json
from pathlib import Path
import re


def records(path):
    with path.open() as stream:
        return [json.loads(line) for line in stream]


def audit(run, sources):
    events = records(run / "events.jsonl")
    origin, = [event for event in events if event["event"] == "create_start"]
    source = sources / origin["task"] / origin["trial_id"]
    for filename, key in (("trajectory.json","trajectory_sha256"),("rollout.jsonl","rollout_sha256")):
        with (source / filename).open("rb") as stream:
            if hashlib.file_digest(stream,"sha256").hexdigest() != origin[key]:
                raise ValueError(f"{run.name}: source {filename} differs")
    atif = json.loads((source / "trajectory.json").read_bytes())
    calls = {}
    outputs = {}
    for step in atif["steps"]:
        for call in step.get("tool_calls",[]):
            if call["function_name"] in {"exec_command","write_stdin","view_image"}:
                calls[call["tool_call_id"]] = call
        for result in step.get("observation",{}).get("results",[]):
            if "source_call_id" in result:
                outputs[result["source_call_id"]] = result.get("content","")
    rejected = {e["source_call_id"] for e in events if e.get("source_kind") == "harness_rejection_request"}
    sequence = []
    for item in records(source / "rollout.jsonl"):
        payload = item.get("payload",{})
        ident = payload.get("call_id")
        if item["type"] == "response_item" and payload.get("type") == "function_call" and ident in calls and ident not in rejected:
            sequence.append(ident)
    dispatched = {e["sequence"]:e for e in events if e["event"] == "tool_dispatch"}
    rows = []
    for reply in events:
        if reply["event"] != "tool_reply" or reply.get("operation","Exec") != "Exec":
            continue
        seq = reply["sequence"]
        ident = sequence[seq-1]
        if calls[ident]["function_name"] != "exec_command" or dispatched[seq].get("source_call_id",ident) != ident:
            raise ValueError(f"{run.name}: foreground source binding differs at {seq}")
        args = calls[ident]["arguments"]
        limit = min(30000,max(250,args.get("yield_time_ms",10000)))
        elapsed = reply["monotonic_ns"]-dispatched[seq]["monotonic_ns"]
        wall = re.search(r"(?m)^Wall time: ([.0-9]+) seconds",outputs[ident])
        rows.append(dict(run_id=run.name,task=origin["task"],trial=origin["trial_id"],step=reply["step_id"],
                         sequence=seq,call_id=ident,original_yield_ms=limit,original_wall_seconds=float(wall[1]) if wall else None,
                         host_elapsed_ns=elapsed,host_envelope_exceeds_yield=elapsed>limit*1_000_000))
    return rows


if __name__ == "__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sources",type=Path)
    parser.add_argument("runs",nargs="+",type=Path)
    args=parser.parse_args()
    rows=[r for run in args.runs for r in audit(run,args.sources)]
    print(json.dumps({"schema":"aries.harbor.foreground-yield-audit.v1","foreground_calls":len(rows),
                      "host_envelopes_above_original_yield":sum(r["host_envelope_exceeds_yield"] for r in rows),
                      "scope":"The host interval includes guest transport and process startup. Guest-only elapsed time is unavailable.",
                      "calls":rows},indent=2))
