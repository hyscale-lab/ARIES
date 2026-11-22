"""Reconcile one replay attempt into the Zepbound version-1 measurement format."""
import argparse
from collections import Counter
from guest_classes import cross_tab
import json
from pathlib import Path
import tempfile


def read(path):
    with path.open() as f:
        for line in f:
            yield json.loads(line)


def write(path, rows):
    with path.open("x") as f:
        for r in rows:
            f.write(json.dumps(r, separators=(",", ":"))+"\n")


def ranges(sorted_gpas, page):
    spans = []
    for gpa in sorted(sorted_gpas):
        if spans and spans[-1][1] == gpa:
            spans[-1][1] += page
        else:
            spans.append([gpa, gpa+page])
    return spans


def reconcile(raw, output, meta):
    events = list(read(raw/"events.jsonl"))
    samples = list(read(raw/"samples.jsonl"))
    start, = [e for e in events if e["event"] == "session_start"]
    end, = [e for e in events if e["event"] == "session_end"]
    create, = [e for e in events if e["event"] == "create_start"]
    if create["trial_id"] != meta["source_trial"] or create["job_id"] != meta["job_id"]:
        raise ValueError("run metadata disagrees with replay origin")
    if not any(e["event"]=="restore_complete" for e in events) or events[-1]["event"]!="removed":
        raise ValueError("restore or cleanup incomplete")
    dispatch = [e["step_id"] for e in events if e["event"]=="tool_dispatch"]
    replies = [e for e in events if e["event"]=="tool_reply"]
    if not dispatch or dispatch != [r["step_id"] for r in replies] or any(
            r["exit_code"] != r["expected_exit_code"] for r in replies):
        raise ValueError("incomplete or divergent tool sequence")
    instance = {(s["pid"], str(s["start_ticks"])) for s in samples}
    if len(instance)!=1 or len(samples)<2:
        raise ValueError("missing one-second samples or changed VMM")
    meta = dict(meta, session_begin_ns=start["monotonic_ns"], session_end_ns=end["monotonic_ns"],
                runtime_id=create["runtime_id"], source_rollout_sha256=create["rollout_sha256"],
                source_trajectory_sha256=create["trajectory_sha256"], image=create["image"])
    windows, freezes, phases = [], [], []
    classes, sampled, originals = [], [], {}
    first_guest = None
    trace = raw/"idle.jsonl"
    if not trace.exists():
        trace = raw/"boundaries.jsonl"
    if trace.exists():
        for e in read(trace):
            if e["status"]!="ok" or e["runtime"]!=meta["runtime_id"] or (
                    e["vmm_pid"],str(e["vmm_start_ticks"])) not in instance:
                raise ValueError("invalid or unrelated idle boundary")
            phases.append((e["phase"],e["step"]))
            f=e["freeze"]
            order=[f[k] for k in ("pause_request_ns","pause_ack_ns","resume_request_ns","resume_ack_ns")]
            if sorted(order)!=order:
                raise ValueError("invalid freeze timing")
            freezes.append(dict(begin_ns=order[1],end_ns=order[2], **f))
            if e.get("vm_id") != e["runtime"]:
                raise ValueError("Firecracker instance differs from replay runtime")
            census = dict(phase=e["phase"],step=e["step"],monotonic_ns=order[1],
                          census=e.get("vmm_census"),guest_ram_bytes=e["guest_ram_bytes"])
            classes.append(census)
            if "guest_telemetry" in e:
                if "window" in e:
                    if first_guest is not None:
                        classes[0]["guest"],tags=cross_tab(first_guest,e["window"]["not_resident_at_arm_gpa_ranges"])
                        sampled.append((classes[0],tags))
                        first_guest=None
                    census["guest"],tags=cross_tab(e,e["window"]["not_resident_at_end_gpa_ranges"])
                    sampled.append((census,tags))
                else:
                    first_guest=e
            if "original_snapshot_sample" in e:
                originals={r["gfn"]:r["digest"] for r in e["original_snapshot_sample"]}
            if "window" not in e:
                continue
            w=e["window"]
            page=w["page_bytes"]
            counts=Counter(w["unknown_gpas"].values())
            if counts != w["unknown_counts"]:
                raise ValueError("unknown counts disagree with detailed census")
            ambiguous=sorted(int(k) for k,v in w["unknown_gpas"].items() if v=="newly-resident-unarmed")
            windows.append(dict(phase=e["window_kind"],step=e["step"],begin_ns=w["arm_end_ns"],
                end_ns=w["scan_begin_ns"], page_bytes=page, eligible_bytes=(len(w["accessed_gpas"])+len(w["idle_gpas"]))*page,
                marked_referenced_bytes=len(w["accessed_gpas"])*page,
                demand_evidenced_new_bytes=None, ambiguous_arrival_bytes=len(ambiguous)*page,
                resident_at_end_bytes=e["guest_ram_bytes"]-sum(b-a for a,b in w["not_resident_at_end_gpa_ranges"]),
                endpoint_pss_bytes=sum(m["Pss_bytes"] for m in e["vmm_census"]["categories"].values()) if e.get("vmm_census") else None,
                unknown_counts=dict(counts), referenced_gpa_ranges=ranges(w["accessed_gpas"],page),
                ambiguous_arrival_gpa_ranges=ranges(ambiguous,page),coverage=w["coverage_scope"]))
        expected=[("session-start",0)]
        for step in dispatch:
            expected.extend([("pre-tool",step),("post-tool",step)])
        if phases!=expected+[("session-end",0)]:
            raise ValueError("tracing missed a required boundary")
    elif meta["status"]=="validated":
        raise ValueError("measurement admission requires traced boundaries")
    sample_rows=[]
    for receipt,tags in sampled:
        by_state={}
        cross=Counter()
        for row in tags:
            state=row["state"]
            if state=="resident":
                state=("snapshot_unavailable" if row["gfn"] not in originals else
                       "matches_snapshot" if row["digest"]==originals[row["gfn"]] else "differs_from_snapshot")
            key=(row["guest_flag_class"],state)
            cross[key]+=1
            by_state.setdefault(key,[]).append(row["gfn"])
        for (label,state),gfns in sorted(by_state.items()):
            sample_rows.append(dict(monotonic_ns=receipt["monotonic_ns"],guest_class=label,state=state,gfns=gfns))
        receipt["sample_cross_tab"]=[dict(guest_flag_class=k[0],state=k[1],pages=v) for k,v in sorted(cross.items())]
    if output.exists():
        raise FileExistsError(output)
    output.parent.mkdir(parents=True,exist_ok=True)
    with tempfile.TemporaryDirectory(dir=output.parent) as temporary:
        stage=Path(temporary)/"run"
        stage.mkdir()
        (stage/"run.json").write_text(json.dumps(meta,indent=2)+"\n")
        write(stage/"events.jsonl",[{k:v for k,v in e.items() if k not in ("stdout","stderr")} for e in events])
        write(stage/"samples.jsonl",samples)
        write(stage/"windows.jsonl",windows)
        write(stage/"freezes.jsonl",freezes)
        write(stage/"classes.jsonl",classes)
        if sample_rows:
            write(stage/"sampled-pages.jsonl",sample_rows)
        stage.rename(output)


if __name__=="__main__":
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument("raw",type=Path)
    p.add_argument("metadata",type=Path)
    p.add_argument("output",type=Path)
    args=p.parse_args()
    reconcile(args.raw,args.output,json.loads(args.metadata.read_text()))
