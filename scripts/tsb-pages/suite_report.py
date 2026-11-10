#!/usr/bin/env python3
"""Summarize all catalog tasks without equating missing or invalid traces to zero."""
import argparse, collections, datetime, hashlib, html, json, statistics
from pathlib import Path
import overhead
from suite import canonical_memory_regions,observation_memory


def read(path, default=None):
    return json.loads(path.read_text()) if path.exists() else default


def stats(values):
    values=[v for v in values if v is not None]
    return {"n":len(values),"min":min(values) if values else None,
            "median":statistics.median(values) if values else None,
            "max":max(values) if values else None}


def json_rows(path):
    if not path.exists():
        return []
    rows=[]
    with path.open() as stream:
        for line in stream:
            if not line.strip():continue
            try:rows.append(json.loads(line))
            except json.JSONDecodeError:continue
    return rows


def endpoint_residency(observer, readings, valid, costs):
    """Use final census counts, never the access mask's all-samples-absent state."""
    native=(observer/"access-dirty/initial-arm.json").exists() or any(s.get("vm_generation") for s in valid)
    evidence={"total_ram_bytes":None,"source":None,"errors":[]}
    try:
        total,source=observation_memory(observer,readings,"vmm_owned" if native else "legacy")
        if native:
            initial=read(observer/"access-dirty/initial-arm.json")
            generation=initial["vm_generation"]
            expected_regions=canonical_memory_regions(initial["regions"])
            if any(s.get("vm_generation")!=generation for s in valid):
                raise ValueError("comparison differs from authoritative VMM generation")
        evidence.update(total_ram_bytes=total,source=source)
    except (OSError,ValueError,KeyError,TypeError) as error:
        evidence["errors"].append(type(error).__name__+": "+str(error))
        return [],[],evidence
    records={(r["vm_generation"],r["record_id"]):r for r in costs["native_epochs"]["records"]}
    resident_mib=[];fractions=[]
    for step in valid:
        resident=None
        if native:
            record=records.get((step.get("vm_generation"),step.get("source_record_id")))
            population=(record or {}).get("resident_population") or {}
            count=population.get("resident_gpa_pages")
            if (record and record.get("completion_state")=="complete" and not record.get("invalid")
                    and record.get("phase_kind")=="tool"
                    and record.get("epoch")==step.get("source_epoch")
                    and type(count) is int and total is not None and 0<=count*4096<=total):
                try:
                    if any(canonical_memory_regions(record[key])!=expected_regions for key in ("regions","census_regions")):
                        raise ValueError("census RAM regions differ from initial arm")
                    resident=count*4096
                except (ValueError,KeyError,TypeError) as error:
                    evidence["errors"].append(str(record.get("record_id"))+": "+str(error))
        elif total is not None:
            absent=step.get("not_resident_at_end_gpa_ranges")
            if isinstance(absent,list):
                absent_bytes=sum(hi-lo for lo,hi in absent)
                if 0<=absent_bytes<=total:resident=total-absent_bytes
        if resident is not None:
            resident_mib.append(resident/1048576)
            fractions.append(step["counted_touched_bytes"]/resident if resident else None)
    return resident_mib,fractions,evidence


def task_summary(root, task):
    d=root/task["task_id"]
    status=read(d/"status.json",{})
    audit=read(d/"boundary-audit.json",{})
    comp=read(d/"comparison.json",{})
    steps=comp.get("steps",[])
    valid=[s for s in steps if s.get("measurement_valid")]
    sets=[set(s["counted_touched_gpas"]) for s in valid]
    pages=valid[0]["page_bytes"] if valid else None
    session_identities={(s.get("vm_instance"),s.get("vm_generation"),s["page_bytes"]) for s in valid}
    session_comparable=len(session_identities)==1 and next(iter(session_identities))[0] is not None
    pairs=comp.get("pairs",[])
    adjacent=[p["jaccard"] for p in pairs if p["to_index"]==p["from_index"]+1 and p.get("comparable")]
    windows=d/"observer/steps.jsonl"
    group_mode=(d/"observer/groups.jsonl").exists()
    begins=ends=requested_calls=0
    batch_mode=False
    logs=sorted((d/"runs").glob("*/"+task.get("execution_task_id",task["task_id"])+"/harness/telemetry/aries-step-pages.jsonl"))
    costs=overhead.summarize(d/"observer",logs[0] if len(logs)==1 else None)
    for r in json_rows(windows):
        begins+=r.get("event")=="step_begin"
        ends+=r.get("event")=="step_end"
    readings=d/"observer/groups.jsonl" if group_mode else windows
    reading_rows=json_rows(readings)
    if readings.exists():
        for r in reading_rows:
            if r.get("event")=="batch_begin":
                batch_mode=True
                requested_calls+=len(r.get("expected_tool_call_ids",[]))
    resident_end_mib,footprint_fraction,memory_evidence=endpoint_residency(
        d/"observer",[r for r in reading_rows if "reading" in r and r.get("measurement_valid")],valid,costs)
    return {"task_id":task["task_id"],"shard_index":task["shard_index"],
        "native_environment":task["native_environment"],"status":status,
        "boundary_audit_status":audit.get("status"),"native_step_begins":begins,
        "native_step_ends":ends,"comparison_steps":len(steps),"valid_measurement_units":len(valid),
        "measurement_granularity":comp.get("comparison_unit","llm_reasoning_step" if batch_mode else "busy_period_group" if group_mode else "native_hook_window"),
        "requested_native_calls":requested_calls if batch_mode else max(begins,ends),
        "units_with_multiple_calls":sum(len(s.get("member_tool_call_ids",[]))>1 for s in valid),
        "units_with_overlap":sum(s.get("concurrency_peak",0)>1 or bool(s.get("overlapping_tool_call_ids")) for s in valid),
        "units_with_transition_gaps":sum(bool(s.get("transition_gaps")) for s in valid),
        "invalid_measurement_units":len(steps)-len(valid),
        "guest_memory_evidence":memory_evidence,
        "guest_resident_at_scan_end_mib":stats(resident_end_mib),
        "counted_footprint_fraction_of_endpoint_resident_ram":stats(footprint_fraction),"counted_footprint_mib":stats([s["counted_touched_bytes"]/1048576 for s in valid]),
        "observed_only_footprint_mib":stats([s["accessed_bytes"]/1048576 for s in valid]),
        "unknown_pages":stats([s["unknown_pages"] for s in valid]),
        "session_sets_comparable":session_comparable,
        "session_union_mib":len(set.union(*sets))*pages/1048576 if sets and session_comparable else None,
        "session_common_mib":len(set.intersection(*sets))*pages/1048576 if sets and session_comparable else None,
        "adjacent_jaccard":stats(adjacent),"tool_interval_ms":stats([s.get("tool_interval_ms") for s in valid]),
        "observer_arm_ms":stats([s.get("arm_ms") for s in valid]),
        "observer_scan_ms":stats([s.get("scan_ms") for s in valid]),
        "tracer_overhead":costs,
        "overhead_html":str((d/"overhead.html").relative_to(root)) if (d/"overhead.html").exists() else None,
        "native_usage":audit.get("native_usage_totals"),"task_outcomes":audit.get("task_outcomes"),
        "comparison_html":str((d/"comparison.html").relative_to(root)) if (d/"comparison.html").exists() else None,
        "status_path":str((d/"status.json").relative_to(root)) if (d/"status.json").exists() else None}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument("--root",type=Path,required=True);a=p.parse_args()
    root=a.root.resolve();manifests=sorted((root/"manifests").glob("shard-*.json"))
    if not manifests:raise ValueError("no shard manifest")
    source=[read(p) for p in manifests];catalog=source[0]["tasks"]
    ids=[t["task_id"] for t in catalog]
    if len(ids)!=89 or len(set(ids))!=89:raise ValueError("catalog is not exactly89 unique tasks")
    assignment=[(t["task_id"],t["shard_index"]) for t in catalog]
    for m in source:
        if [(t["task_id"],t["shard_index"]) for t in m["tasks"]]!=assignment:raise ValueError("inconsistent shard assignment")
        if m["dataset"]["revision"]!=source[0]["dataset"]["revision"]:raise ValueError("inconsistent dataset revision")
    tasks=[task_summary(root,t) for t in catalog]
    report={"schema":"aries.tsb.suite-report.v1","generated_utc":datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "catalog_tasks":len(tasks),"available_shards":[m["shard_index"] for m in source],
        "dataset_revision":source[0]["dataset"]["revision"],
        "models_by_shard":[{"shard_index":m["shard_index"],"model":m.get("model")} for m in source],
        "status_counts":dict(collections.Counter(t["status"].get("status","not_copied_or_not_prepared") for t in tasks)),
        "tasks_with_completed_page_sets":sum(t["comparison_steps"]>0 for t in tasks),
        "tasks_with_passed_boundary_audit":sum(t["boundary_audit_status"]=="passed" for t in tasks),
        "valid_measurement_units":sum(t["valid_measurement_units"] for t in tasks),
        "invalid_measurement_units":sum(t["invalid_measurement_units"] for t in tasks),
        "counting_policy":"observed touched union all recorded unknown GPAs, by user-requested assumption",
        "limitations":["Unknown inclusion does not prove every unknown page was accessed.",
          "Guest physical pages are compared only within one VM; never across tasks or workers.",
          "The primary unit is one LLM tool-request/result phase, including all sequential, parallel and harness-expanded operations. Individual member page sets are not attributed.",
          "Historical busy-period and segmented prototype data retain their original granularity and must not be relabeled as LLM reasoning steps.",
          "Native result-persistence times can follow completion of every parallel execution; they are not individual tool completion times.",
          "Experimental segmented native-hook windows have additional interior reset gaps and are partial temporal estimates.",
          "Guest RAM and its resident subset do not equal total attributable host memory.",
          "Endpoint residency uses the final native census; pages absent at every access sample are a different quantity.",
          "Tracer costs are nested and overlapping. Native bit collection, host boundary waits and companion decoding must not be summed or subtracted from tool windows.",
          "One initial session per task does not establish run-to-run reproducibility.",
          "Observer scans include background activity and can miss accesses; these are operational footprint estimates.",
          "Missing, invalid and incomplete observations remain explicit; zero is not substituted.",
          "This report may be generated during execution; presence of all tasks in the inventory is not proof of collection completion."],
        "tasks":tasks,"manifest_sources":[{"path":str(p.relative_to(root)),"sha256":hashlib.sha256(p.read_bytes()).hexdigest()} for p in manifests]}
    (root/"suite-summary.json").write_text(json.dumps(report,indent=2)+"\n")
    e=html.escape
    def val(x):return "—" if x is None else (f"{x:.3f}" if isinstance(x,float) else str(x))
    rows=[]
    for t in tasks:
        label=e(t["task_id"])
        if t["comparison_html"]:label="<a href=\""+e(t["comparison_html"])+"\">"+label+"</a>"
        costs=t["tracer_overhead"]
        def native_ms(name):
            value=costs["native_epochs"]["by_phase"].get("tool",{}).get("metrics",{}).get(name,{}).get("median")
            return val(value/1e6 if value is not None else None)
        collect=costs["host_boundaries"]["by_kind"].get("collect",{}).get("wall_ns",{}).get("median")
        detail=[]
        for level,modes in costs["companion"]["by_detail"].items():
            timing=modes.get("attribute",{}).get("wall_ns",{}).get("median")
            if timing is not None:detail.append(level+": "+val(timing/1e6))
        cost_link="<a href=\""+e(t["overhead_html"])+"\">costs</a>" if t["overhead_html"] else e(costs["status"])
        cols=[label,str(t["shard_index"]),e(t["status"].get("status","not copied/prepared")),e(str(t["boundary_audit_status"] or "missing")),str(t["native_environment"]["memory_mb"]),str(t["requested_native_calls"]),e(t["measurement_granularity"]),str(t["valid_measurement_units"]),val(t["counted_footprint_mib"]["median"]),val(t["counted_footprint_mib"]["max"]),val(t["session_union_mib"]),val(t["session_common_mib"]),val(t["adjacent_jaccard"]["median"]),val(t["observer_scan_ms"]["median"]),native_ms("idle_scan_wall_ns"),native_ms("dirty_wall_ns"),val(collect/1e6 if collect is not None else None),e(", ".join(detail) or "—"),cost_link]
        rows.append("<tr>"+"".join("<td>"+v+"</td>" for v in cols)+"</tr>")
    headers=["Task","Shard","Execution status","Boundary audit","RAM MiB","Native calls","Measurement unit","Valid units","Median counted MiB","Max counted MiB","Session union MiB","Common MiB","Median adjacent Jaccard","Median legacy scan ms","Median native idle scan ms","Median native dirty ms","Median host collect ms","Median attribution decode ms","Tracer report"]
    page="<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content=\"width=device-width,initial-scale=1\"><title>Terminal-Bench page-footprint suite</title><style>body{font:15px system-ui;margin:28px;color:#17232b}table{border-collapse:collapse}td,th{padding:7px;border:1px solid #ccd6db}th{position:sticky;top:0;background:#eef4f5}.scroll{overflow:auto}li{margin:7px 0}</style><h1>Terminal-Bench page-footprint suite</h1><p>"+e(report["generated_utc"])+". All 89 tasks are retained in the inventory. "+str(report["tasks_with_completed_page_sets"])+" tasks have completed page sets; "+str(report["tasks_with_passed_boundary_audit"])+" have passed boundary audits.</p><p>"+str(len(source))+" retained shard manifests; native task resources and deadlines. Model settings and source identities are recorded per shard in the <a href=\"suite-summary.json\">machine-readable summary</a>.</p><ul>"+"".join("<li>"+e(v)+"</li>" for v in report["limitations"])+"</ul><div class=scroll><table><tr>"+"".join("<th>"+e(v)+"</th>" for v in headers)+"</tr>"+"".join(rows)+"</table></div></html>"
    (root/"report.html").write_text(page)
    print(json.dumps({k:report[k] for k in ("catalog_tasks","available_shards","status_counts","tasks_with_completed_page_sets","tasks_with_passed_boundary_audit","valid_measurement_units")}))

if __name__=="__main__":main()
