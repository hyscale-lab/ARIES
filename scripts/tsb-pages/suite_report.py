#!/usr/bin/env python3
"""Summarize all catalog tasks without equating missing or invalid traces to zero."""
import argparse, collections, datetime, hashlib, html, json, statistics
from pathlib import Path


def read(path, default=None):
    return json.loads(path.read_text()) if path.exists() else default


def stats(values):
    values=[v for v in values if v is not None]
    return {"n":len(values),"min":min(values) if values else None,
            "median":statistics.median(values) if values else None,
            "max":max(values) if values else None}


def task_summary(root, task):
    d=root/task["task_id"]
    status=read(d/"status.json",{})
    audit=read(d/"boundary-audit.json",{})
    comp=read(d/"comparison.json",{})
    steps=comp.get("steps",[])
    valid=[s for s in steps if s.get("measurement_valid")]
    sets=[set(s["counted_touched_gpas"]) for s in valid]
    pages=valid[0]["page_bytes"] if valid else None
    pairs=comp.get("pairs",[])
    adjacent=[p["jaccard"] for p in pairs if p["to_index"]==p["from_index"]+1 and p.get("comparable")]
    windows=d/"observer/steps.jsonl"
    group_mode=(d/"observer/groups.jsonl").exists()
    begins=ends=requested_calls=0
    batch_mode=False
    resident_end_mib=[]; footprint_fraction=[]
    slots=read(d/"observer/selected-slots.json",[])
    total_ram_bytes=sum(slot["memory_size"] for slot in slots) if slots else None
    if windows.exists():
        for line in windows.open():
            if not line.strip():continue
            try:r=json.loads(line)
            except json.JSONDecodeError:continue
            begins+=r.get("event")=="step_begin"
            ends+=r.get("event")=="step_end"
    readings=d/"observer/groups.jsonl" if group_mode else windows
    if readings.exists():
        for line in readings.open():
            if not line.strip():continue
            try:r=json.loads(line)
            except json.JSONDecodeError:continue
            if r.get("event")=="batch_begin":
                batch_mode=True
                requested_calls+=len(r.get("expected_tool_call_ids",[]))
            if "reading" in r and r.get("measurement_valid") and total_ram_bytes:
                reading=r["reading"]
                absent=reading.get("not_resident_at_end_gpa_ranges")
                if isinstance(absent,list):
                    resident=total_ram_bytes-sum(hi-lo for lo,hi in absent)
                    resident_end_mib.append(resident/1048576)
                    counted=len(set(reading["accessed_gpas"])|{int(g) for g in reading["unknown_gpas"]})*reading["page_bytes"]
                    footprint_fraction.append(counted/resident if resident else None)
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
        "guest_resident_at_scan_end_mib":stats(resident_end_mib),
        "counted_footprint_fraction_of_endpoint_resident_ram":stats(footprint_fraction),"counted_footprint_mib":stats([s["counted_touched_bytes"]/1048576 for s in valid]),
        "observed_only_footprint_mib":stats([s["accessed_bytes"]/1048576 for s in valid]),
        "unknown_pages":stats([s["unknown_pages"] for s in valid]),
        "session_union_mib":len(set.union(*sets))*pages/1048576 if sets else None,
        "session_common_mib":len(set.intersection(*sets))*pages/1048576 if sets else None,
        "adjacent_jaccard":stats(adjacent),"tool_interval_ms":stats([s.get("tool_interval_ms") for s in valid]),
        "observer_arm_ms":stats([s.get("arm_ms") for s in valid]),
        "observer_scan_ms":stats([s.get("scan_ms") for s in valid]),
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
        cols=[label,str(t["shard_index"]),e(t["status"].get("status","not copied/prepared")),e(str(t["boundary_audit_status"] or "missing")),str(t["native_environment"]["memory_mb"]),str(t["requested_native_calls"]),e(t["measurement_granularity"]),str(t["valid_measurement_units"]),val(t["counted_footprint_mib"]["median"]),val(t["counted_footprint_mib"]["max"]),val(t["session_union_mib"]),val(t["session_common_mib"]),val(t["adjacent_jaccard"]["median"]),val(t["observer_scan_ms"]["median"])]
        rows.append("<tr>"+"".join("<td>"+v+"</td>" for v in cols)+"</tr>")
    headers=["Task","Shard","Execution status","Boundary audit","RAM MiB","Native calls","Measurement unit","Valid units","Median counted MiB","Max counted MiB","Session union MiB","Common MiB","Median adjacent Jaccard","Median scan ms"]
    page="<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content=\"width=device-width,initial-scale=1\"><title>Terminal-Bench page-footprint suite</title><style>body{font:15px system-ui;margin:28px;color:#17232b}table{border-collapse:collapse}td,th{padding:7px;border:1px solid #ccd6db}th{position:sticky;top:0;background:#eef4f5}.scroll{overflow:auto}li{margin:7px 0}</style><h1>Terminal-Bench page-footprint suite</h1><p>"+e(report["generated_utc"])+". All 89 tasks are retained in the inventory. "+str(report["tasks_with_completed_page_sets"])+" tasks have completed page sets; "+str(report["tasks_with_passed_boundary_audit"])+" have passed boundary audits.</p><p>Six independent workers, one task at a time per worker; native task resources and deadlines. DeepSeek Flash, high thinking. <a href=\"suite-summary.json\">Machine-readable summary</a>.</p><ul>"+"".join("<li>"+e(v)+"</li>" for v in report["limitations"])+"</ul><div class=scroll><table><tr>"+"".join("<th>"+e(v)+"</th>" for v in headers)+"</tr>"+"".join(rows)+"</table></div></html>"
    (root/"report.html").write_text(page)
    print(json.dumps({k:report[k] for k in ("catalog_tasks","available_shards","status_counts","tasks_with_completed_page_sets","tasks_with_passed_boundary_audit","valid_measurement_units")}))

if __name__=="__main__":main()
