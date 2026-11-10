#!/usr/bin/env python3
"""Report retained tracer costs without reconstructing uninstrumented latency."""
import argparse
import collections
import hashlib
import html
import json
import math
from pathlib import Path
import statistics


SCHEMA = "aries.tracer-overhead.v1"
STATES = ("complete", "failed", "censored", "incomplete")
LIMITATIONS = [
    "Boundary/component/API/native/decode spans are nested or overlapping; do not add their totals.",
    "Boundary envelopes include recorded API-journal work, but exclude their own journal write and network/helper delay.",
    "Metadata barriers measure remaining exposed wait, not the decoder's whole background cost.",
    "Paused LLM policy time and background publication are not automatically latency overhead.",
    "Native tool windows already exclude initial arm and final collection; no adjusted workload latency is calculated.",
    "Process CPU timings have their recorded process scope; they are not total host CPU or peak-memory measurements.",
    "Detail-mode aggregates are descriptive; unmatched workloads/captures cannot establish a mode speedup.",
    "Missing durations stay unavailable. Failed and censored records are retained separately from complete measurements.",
    "Headline cost statistics include only complete, nonfailed, noncensored records; by_state retains other attempt costs.",
    "Completion state describes each recorded operation/capture, not universal measurement coverage or task success.",
]


def number(value):
    return value if type(value) in (int, float) and math.isfinite(value) and value >= 0 else None


def interval(row, begin, end):
    lo, hi = number(row.get(begin)), number(row.get(end))
    return (lo, hi) if lo is not None and hi is not None and hi >= lo else None


def duration(row, begin, end):
    span = interval(row, begin, end)
    return span[1] - span[0] if span is not None else None


def stats(values):
    known = [value for value in values if number(value) is not None]
    return {"n": len(known), "missing": len(values) - len(known),
            "total": sum(known) if known else None,
            "min": min(known) if known else None,
            "median": statistics.median(known) if known else None,
            "max": max(known) if known else None}


def union_ns(spans):
    spans = sorted(spans)
    if not spans:
        return None
    lo, hi = spans[0]
    total = 0
    for start, end in spans[1:]:
        if start > hi:
            total += hi - lo
            lo, hi = start, end
        else:
            hi = max(hi, end)
    return total + hi - lo


def completion_state(*, complete, failed=False, censored=False):
    # States are exclusive; raw failure/censor flags remain independently visible.
    return "failed" if failed else "censored" if censored else "complete" if complete else "incomplete"


class Inputs:
    """Read only small metadata artifacts; hash every file actually consumed."""
    def __init__(self, root):
        self.root = root
        self.sources = {}
        self.errors = []

    def name(self, path):
        try:
            return str(path.relative_to(self.root))
        except ValueError:
            return str(path)

    def error(self, path, message, **fields):
        self.errors.append({"source": self.name(path), "error": message, **fields})

    def read(self, path):
        if not path.is_file():
            return None
        try:
            data = path.read_bytes()
            self.sources[self.name(path)] = {"sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)}
            return json.loads(data)
        except (OSError, ValueError, UnicodeError) as error:
            self.error(path, type(error).__name__ + ": " + str(error))
            return None

    def rows(self, path):
        if not path.is_file():
            return
        digest, size = hashlib.sha256(), 0
        try:
            with path.open("rb") as stream:
                for index, raw in enumerate(stream, 1):
                    digest.update(raw)
                    size += len(raw)
                    if not raw.strip():
                        continue
                    try:
                        row = json.loads(raw)
                        if not isinstance(row, dict):
                            raise ValueError("record must be an object")
                    except (ValueError, UnicodeError) as error:
                        self.error(path, str(error), line=index)
                        continue
                    yield row
        except OSError as error:
            self.error(path, str(error))
        self.sources[self.name(path)] = {"sha256": digest.hexdigest(), "bytes": size}

    def object(self, path):
        value = self.read(path)
        if value is not None and not isinstance(value, dict):
            self.error(path, "metadata must be an object")
            return {}
        return value or {}


def api_summary(inputs, directory, embedded=None):
    ledger, legacy = directory / "api-calls.jsonl", directory / "api-calls.json"
    if ledger.is_file():
        records, source, fmt = inputs.rows(ledger), ledger, "jsonl"
    elif legacy.is_file():
        records, source, fmt = inputs.read(legacy), legacy, "legacy_json"
        if not isinstance(records, list):
            inputs.error(legacy, "API history must be an array")
            records = []
    else:
        records, source, fmt = embedded or [], directory / "companion-metrics.json", "embedded" if embedded is not None else None
    calls, previous = [], 0
    for index, row in enumerate(records):
        if not isinstance(row, dict):
            inputs.error(source, "API receipt must be an object", record=index)
            continue
        if fmt == "jsonl":
            sequence = row.get("sequence")
            if type(sequence) is not int or sequence != previous + 1:
                inputs.error(source, "API journal sequence is missing, duplicated or discontinuous", record=index)
            if type(sequence) is int:
                previous = sequence
        failed = row.get("outcome") == "failed" or (type(row.get("http_status")) is int and row["http_status"] >= 400)
        calls.append({"endpoint": row.get("endpoint"), "operation": row.get("operation"),
                      "wall_ns": duration(row, "begin_ns", "end_ns"),
                      "response_bytes": number(row.get("response_bytes")),
                      "outcome": row.get("outcome"), "http_status": row.get("http_status"),
                      "completion_state": completion_state(complete=row.get("outcome") == "returned", failed=failed)})
    groups = collections.defaultdict(list)
    for call in calls:
        groups[str(call["endpoint"]) + " " + str(call["operation"])].append(call)
    def measured(rows):
        return {"records": len(rows), "wall_ns": stats([row["wall_ns"] for row in rows]),
                "response_bytes": stats([row["response_bytes"] for row in rows])}
    def describe(rows):
        good = [row for row in rows if row["completion_state"] == "complete"]
        return {"records": len(rows), "failed": sum(row["outcome"] == "failed" or
                    (type(row["http_status"]) is int and row["http_status"] >= 400) for row in rows),
                "unknown_outcome": sum(row["outcome"] not in ("returned", "failed") for row in rows),
                "complete_records": len(good), "wall_ns": measured(good)["wall_ns"],
                "response_bytes": measured(good)["response_bytes"],
                "by_state": {state: measured([row for row in rows if row["completion_state"] == state]) for state in STATES}}
    return {"available": fmt is not None, "format": fmt, **describe(calls),
            "aggregate_scope": "successful request receipts only; other attempts are in by_state",
            "by_operation": {key: describe(rows) for key, rows in sorted(groups.items())}}


def host_boundaries(inputs, root):
    path = root / "boundaries.jsonl"
    records = []
    for index, event in enumerate(inputs.rows(path)):
        envelope = interval(event, "begin_ns", "end_ns")
        parts, covered, outside = {}, [], []
        for key in event:
            if not key.endswith("_begin_ns"):
                continue
            name = key[:-len("_begin_ns")]
            end = name + ("_end_ns" if name + "_end_ns" in event else "_ack_ns")
            span = interval(event, key, end)
            wall = span[1] - span[0] if span else None
            parts[name] = wall
            if span is not None and envelope is not None:
                if span[0] < envelope[0] or span[1] > envelope[1]:
                    outside.append(name)
                clipped = max(span[0], envelope[0]), min(span[1], envelope[1])
                if clipped[1] >= clipped[0]:
                    covered.append(clipped)
        if "api_journal" not in parts and "api_journal_ns" in event:
            parts["api_journal"] = number(event["api_journal_ns"])
        known = union_ns(covered)
        wall = envelope[1] - envelope[0] if envelope else None
        failed = bool(event.get("error") or event.get("persistence_error"))
        censored = event.get("censored") is True or "censored_epoch" in event or event.get("measurement_complete") is False
        records.append({"index": index, "kind": event.get("kind", "unknown"),
                        "vm_id": event.get("vm_id"), "vmm_pid": event.get("vmm_pid"),
                        "vmm_start_ticks": event.get("vmm_start_ticks"),
                        "begin_ns": envelope[0] if envelope else None, "end_ns": envelope[1] if envelope else None,
                        "wall_ns": wall, "components_ns": parts, "component_union_ns": known,
                        "unattributed_ns": wall - known if wall is not None and known is not None else None,
                        "components_outside_envelope": outside,
                        "failed": failed,
                        "error": event.get("error") or event.get("persistence_error"),
                        "censored": censored,
                        "completion_state": completion_state(complete=envelope is not None, failed=failed, censored=censored)})
    grouped = collections.defaultdict(list)
    for record in records:
        grouped[record["kind"]].append(record)
    def measured(rows):
        names = sorted({key for row in rows for key in row["components_ns"]})
        return {"records": len(rows), "wall_ns": stats([row["wall_ns"] for row in rows]),
                "unattributed_ns": stats([row["unattributed_ns"] for row in rows]),
                "envelope_union_ns": union_ns([(row["begin_ns"], row["end_ns"]) for row in rows if row["wall_ns"] is not None]),
                "components": {name: stats([row["components_ns"].get(name) for row in rows]) for name in names}}
    def describe(rows):
        good = [row for row in rows if row["completion_state"] == "complete"]
        return {**measured(good), "records": len(rows), "complete_records": len(good),
                "failed": sum(row["failed"] for row in rows), "censored": sum(row["censored"] for row in rows),
                "by_state": {state: measured([row for row in rows if row["completion_state"] == state]) for state in STATES}}
    return {"records": records, **{key: value for key, value in describe(records).items() if key != "records"},
            "aggregate_scope": "complete nonfailed noncensored boundary events only; other attempts are in by_state",
            "by_kind": {kind: describe(rows) for kind, rows in sorted(grouped.items())}}


NATIVE_METRICS = ("idle_arm_wall_ns", "idle_arm_cpu_ns", "idle_scan_wall_ns", "idle_scan_cpu_ns",
                  "idle_bitmap_write_ns", "idle_bitmap_read_ns", "dirty_wall_ns", "census_wall_ns")


def native_epochs(inputs, root):
    records = []
    seen_records, seen_metrics = {}, set()
    paths = sorted(root.glob("epoch-*/manifest.json"))
    if (root / "initial-baseline/manifest.json").is_file():
        paths.insert(0, root / "initial-baseline/manifest.json")
    for path in paths:
        manifest = inputs.object(path)
        if not manifest:
            continue
        response = manifest.get("response") or {}
        if not isinstance(response, dict):
            inputs.error(path, "epoch response must be an object")
            continue
        phase = inputs.object(path.with_name("phase.json"))
        record_id = manifest.get("record_id", response.get("epoch_record_id"))
        generation = manifest.get("vm_generation", response.get("vm_generation"))
        identity = generation, record_id
        invalid = False
        if record_id is not None and identity in seen_records and manifest != seen_records[identity][0]:
            inputs.error(path, "repeated immutable epoch record changed")
            invalid = True
            for prior in seen_records[identity][1]:
                prior.update(invalid=True, failed=True, completion_state="failed")
        if record_id is not None and identity not in seen_records:
            seen_records[identity] = manifest, []
        metadata = manifest.get("metadata", {})
        if isinstance(metadata, dict):
            choices = [row for row in metadata.values() if isinstance(row, dict)]
            meta = choices[0] if choices else {}
            if any(row != meta for row in choices[1:]):
                inputs.error(path, "immutable dataset metadata disagree")
                invalid = True
        else:
            meta = {}
        access, dirty, census = (meta.get(name) or {} for name in ("access", "dirty", "census"))
        epoch = response.get("epoch", meta.get("epoch"))
        generations = [row.get("vm_generation") for row in (manifest, response, meta, phase) if row.get("vm_generation") is not None]
        epochs = [row.get("epoch") for row in (response, meta, phase) if row.get("epoch") is not None]
        ids = [value for value in (record_id, response.get("epoch_record_id")) if value is not None]
        if (any(value != generation for value in generations) or any(value != epoch for value in epochs)
                or any(value != record_id for value in ids)):
            inputs.error(path, "epoch record generation/epoch/record identity disagree")
            invalid = True
        arm = response.get("arm_receipt") or access
        dirty_wall = duration(dirty, "begin_monotonic_us", "end_monotonic_us")
        timing = {"idle_arm_wall_ns": duration(arm, "arm_begin_ns", "arm_end_ns"),
                  "idle_arm_cpu_ns": number(arm.get("arm_cpu_ns")),
                  "idle_scan_wall_ns": duration(access, "scan_begin_ns", "scan_end_ns"),
                  "idle_scan_cpu_ns": number(access.get("scan_cpu_ns")),
                  "idle_bitmap_write_ns": duration(arm, "bitmap_write_begin_ns", "bitmap_write_end_ns"),
                  "idle_bitmap_read_ns": duration(access, "bitmap_read_begin_ns", "bitmap_read_end_ns"),
                  "dirty_wall_ns": dirty_wall * 1000 if dirty_wall is not None else None,
                  "census_wall_ns": duration(census, "inventory_begin_ns", "inventory_end_ns")}
        sources = {"idle_arm": interval(arm, "arm_begin_ns", "arm_end_ns"),
                   "idle_scan": interval(access, "scan_begin_ns", "scan_end_ns"),
                   "idle_bitmap_write": interval(arm, "bitmap_write_begin_ns", "bitmap_write_end_ns"),
                   "idle_bitmap_read": interval(access, "bitmap_read_begin_ns", "bitmap_read_end_ns"),
                   "dirty": interval(dirty, "begin_monotonic_us", "end_monotonic_us"),
                   "census": interval(census, "inventory_begin_ns", "inventory_end_ns")}
        reused = []
        for metric, value in timing.items():
            component = metric.removesuffix("_wall_ns").removesuffix("_cpu_ns").removesuffix("_ns")
            span = sources.get(component)
            if value is None or span is None:
                continue
            key = generation, metric, span
            if key in seen_metrics:
                reused.append(metric)
            else:
                seen_metrics.add(key)
        intervals = {}
        for metric in timing:
            component = metric.removesuffix("_wall_ns").removesuffix("_cpu_ns").removesuffix("_ns")
            span = sources.get(component)
            intervals[metric] = [value * (1000 if component == "dirty" else 1) for value in span] if span else None
        failed = invalid or bool(manifest.get("error")) or manifest.get("status") == "failed"
        censored = phase.get("censored") is True
        records.append({"source": inputs.name(path), "epoch": epoch,
                        "next_epoch": response.get("next_epoch"),
                        "phase_kind": phase.get("phase_kind", meta.get("phase", "unspecified")),
                        "group_id": phase.get("group_id"),
                        "vm_generation": generation, "record_id": record_id, "reused_metrics": reused,
                        "censored": phase.get("censored"),
                        "failed": failed, "invalid": invalid,
                        "completion_state": completion_state(complete=manifest.get("complete") is True and phase.get("censored") is False,
                                                             failed=failed, censored=censored),
                        "baseline_initialization": phase.get("baseline_initialization", path.parent.name == "initial-baseline"),
                        "excluded_from_workload": phase.get("excluded_from_workload"),
                        "complete": manifest.get("complete"),
                        "regions": meta.get("regions", response.get("regions")), "census_regions": census.get("regions"),
                        "metric_intervals": intervals,
                        "resident_population": census.get("resident_population"), **timing})
        if record_id is not None:
            seen_records[identity][1].append(records[-1])
    grouped = collections.defaultdict(list)
    for row in records:
        grouped["initial_baseline" if row["baseline_initialization"] else row["phase_kind"]].append(row)
    def measured(rows):
        result = {}
        for name in NATIVE_METRICS:
            seen, values = set(), []
            for row in rows:
                span = row["metric_intervals"][name]
                key = row["vm_generation"], tuple(span) if span else None
                if span and number(row[name]) is not None:
                    if key in seen:
                        continue
                    seen.add(key)
                values.append(row[name])
            result[name] = stats(values)
        return result
    def describe(rows):
        good = [row for row in rows if row["completion_state"] == "complete"]
        return {"records": len(rows), "complete_records": len(good),
                "censored": sum(row["censored"] is True for row in rows),
                "metrics": measured(good),
                "by_state": {state: {"records": sum(row["completion_state"] == state for row in rows),
                                      "metrics": measured([row for row in rows if row["completion_state"] == state])} for state in STATES}}
    return {"records": records, **{key: value for key, value in describe(records).items() if key != "records"},
            "aggregate_scope": "complete nonfailed noncensored epoch records; interval deduplication is local to each selected aggregate",
            "by_phase": {kind: describe(rows) for kind, rows in sorted(grouped.items())}}


def companion(inputs, root):
    records, children = [], []
    captures = root / "captures"
    for directory in sorted(path for path in captures.glob("*") if path.is_dir()):
        metrics_path = directory / "companion-metrics.json"
        metrics = inputs.object(metrics_path)
        decode_path = directory / "attribution/decode-metrics.json"
        decode = inputs.object(decode_path) if decode_path.is_file() else metrics.get("decode_metrics")
        decode = decode if isinstance(decode, dict) else {}
        begin = inputs.object(directory / "capture-begin.json")
        failure = inputs.read(directory / "failure.json")
        finalized = inputs.object(directory / "finalized-info.json")
        if not (metrics or decode or begin or failure):
            continue
        summary = inputs.object(directory / "attribution/summary.json")
        legacy = {"wall_ns": number(summary.get("capture_and_decode_ns")), "cpu_ns": None}
        api = api_summary(inputs, directory, metrics.get("api_calls"))
        children.append({"capture_id": metrics.get("capture_id", begin.get("capture_id", directory.name)), **api})
        detail = metrics.get("attribution_detail", decode.get("attribution_detail", summary.get("attribution_detail", "unspecified")))
        phases = metrics.get("phases") or {}
        mode = metrics.get("mode", "bootstrap" if directory.name == "bootstrap" else "attribute")
        phase = phases.get("bootstrap" if mode == "bootstrap" else "capture_decode", {})
        decode_wall = number(decode.get("wall_ns", legacy["wall_ns"]))
        decode_cpu = number(decode.get("cpu_ns"))
        timing_scope = "decoder_capture_metadata" if decode_wall is not None else None
        if decode_wall is None and number(phase.get("wall_ns")) is not None:
            decode_wall, decode_cpu = number(phase.get("wall_ns")), number(phase.get("cpu_ns"))
            timing_scope = "child_bootstrap_phase" if mode == "bootstrap" else "child_capture_decode_phase"
        failed = bool(failure) or finalized.get("consistency") == "invalid" or any(row.get("completed") is False for row in phases.values())
        censored = metrics.get("censored") is True or finalized.get("consistency") == "discarded"
        finished = all(row.get("completed") is True for row in phases.values()) if phases else decode_wall is not None
        selected = {key: decode.get(key) for key in ("selected_pages", "selected_gpas_sha256", "sample_pages",
                    "sample_gpas_sha256", "kernel_sha256", "layout_identity", "provider_totals",
                    "decoder_closure_pages", "decoder_closure_bytes", "category_coverage", "tasks_walked",
                    "vmas_walked", "file_identities_resolved", "scope", "stages")}
        totals = decode.get("provider_totals")
        if totals is None and metrics:
            totals = {key: metrics.get(key) for key in ("read_requests", "requested_pages", "cached_pages", "consumed_pages")}
        records.append({"source": inputs.name(metrics_path if metrics_path.is_file() else decode_path),
                        "capture_id": metrics.get("capture_id", begin.get("capture_id", directory.name)),
                        "mode": mode, "decode_timing_scope": timing_scope,
                        "attribution_detail": detail, "vm_generation": begin.get("vm_generation"),
                        "epoch": begin.get("epoch"), "consistency": finalized.get("consistency"),
                        "failed": failed, "failure": failure, "censored": censored,
                        "completion_state": completion_state(complete=finished, failed=failed, censored=censored),
                        "phases": phases, "wall_ns": decode_wall,
                        "cpu_ns": decode_cpu, "provider_totals": totals,
                        "publication_scope": metrics.get("publication_scope"),
                        "consistency_policy": metrics.get("consistency_policy"),
                        "publication_ns": number(summary.get("publication_ns")), **selected})
        records[-1]["provider_totals"] = totals
    groups = collections.defaultdict(list)
    for row in records:
        groups[(row["attribution_detail"], row["mode"])].append(row)
    grouped = {}
    for (detail, mode), rows in sorted(groups.items()):
        def measured(selected):
            phase_names = sorted({name for row in rows for name in row["phases"]})
            stage_names = sorted({name for row in rows for name in (row.get("stages") or {})})
            def stages(name):
                return {key: {"executed_records": sum((row.get(name) or {}).get(key, {}).get("executed", True) is True for row in selected if key in (row.get(name) or {})),
                              "wall_ns": stats([(row.get(name) or {}).get(key, {}).get("wall_ns") for row in selected]),
                              "cpu_ns": stats([(row.get(name) or {}).get(key, {}).get("cpu_ns") for row in selected])}
                        for key in (phase_names if name == "phases" else stage_names)}
            return {"records": len(selected),
                    "wall_ns": stats([row["wall_ns"] for row in selected]), "cpu_ns": stats([row["cpu_ns"] for row in selected]),
                    "decoder_closure_bytes": stats([row["decoder_closure_bytes"] for row in selected]),
                    "selected_pages": stats([row["selected_pages"] for row in selected]),
                    "provider_totals": {key: stats([(row["provider_totals"] or {}).get(key) for row in selected])
                                        for key in ("read_requests", "requested_pages", "cached_pages", "consumed_pages")},
                    "phases": stages("phases"), "stages": stages("stages")}
        good = [row for row in rows if row["completion_state"] == "complete"]
        grouped.setdefault(detail, {})[mode] = {**measured(good), "records": len(rows), "complete_records": len(good),
            "failed": sum(row["failed"] for row in rows), "censored": sum(row["censored"] for row in rows),
            "by_state": {state: measured([row for row in rows if row["completion_state"] == state]) for state in STATES}}
    return {"records": records, "by_detail": grouped,
            "aggregate_scope": "complete nonfailed noncensored captures only; each detail/mode retains all costs in by_state"}, children


def plugin_summary(inputs, path):
    pending, records = {}, []
    failures = collections.Counter()
    unmatched = invalid_seals = 0
    for row in inputs.rows(path) if path is not None else ():
        event = row.get("event", "")
        if event == "measurement_error":
            failures[str(row.get("reason", "unspecified"))] += 1
        if event == "terminal_seal" and row.get("valid") is not True:
            invalid_seals += 1
        requested, confirmed = event.endswith("_requested"), event.endswith("_confirmed")
        if not (requested or confirmed):
            continue
        kind = event.rsplit("_", 1)[0]
        try:
            raw_stamp = row["plugin_monotonic_ns"]
            if type(raw_stamp) is not int and not (isinstance(raw_stamp, str) and raw_stamp.isdigit()):
                raise ValueError("invalid plugin clock")
            stamp = int(raw_stamp)
            if stamp < 0:
                raise ValueError("negative plugin clock")
        except (KeyError, ValueError, TypeError):
            inputs.error(path, "plugin request has no valid local monotonic timestamp")
            continue
        key = tuple(row.get(name) for name in ("plugin_pid", "session_id", "turn_id", "batch_id", "tool_call_id")) + (kind,)
        if row.get("plugin_pid") is None:
            inputs.error(path, "plugin PID is required to pair local monotonic intervals")
            continue
        if requested:
            if key in pending:
                inputs.error(path, "ambiguous duplicate plugin request")
                pending[key] = None
            else:
                pending[key] = stamp
        elif key not in pending or pending[key] is None:
            unmatched += 1
        else:
            start = pending.pop(key)
            if stamp < start:
                inputs.error(path, "plugin confirmation precedes request")
                continue
            records.append({"kind": kind, "plugin_pid": key[0], "session_id": key[1], "turn_id": key[2],
                            "batch_id": key[3], "tool_call_id": key[4], "wall_ns": stamp - start})
    grouped = collections.defaultdict(list)
    for row in records:
        grouped[row["kind"]].append(row["wall_ns"])
    return {"available": path is not None and path.is_file(), "records": records,
            "by_kind": {key: stats(rows) for key, rows in sorted(grouped.items())},
            "unconfirmed_requests": len(pending), "unmatched_confirmations": unmatched,
            "measurement_errors": dict(failures), "invalid_terminal_seals": invalid_seals,
            "clock_scope": "paired within the same plugin process only"}


def summarize(observer: Path, plugin_log: Path | None = None) -> dict:
    observer = Path(observer).resolve()
    inputs = Inputs(observer)
    root = observer / "access-dirty" if (observer / "access-dirty").is_dir() else observer
    if not observer.is_dir():
        inputs.error(observer, "observer directory does not exist")
    host = host_boundaries(inputs, root)
    native = native_epochs(inputs, root)
    captures, children = companion(inputs, root)
    api = api_summary(inputs, root)
    plugin = plugin_summary(inputs, Path(plugin_log).resolve() if plugin_log is not None else None)
    available = {"host_boundaries": bool(host["records"]), "host_api": api["available"],
                 "native_epochs": bool(native["records"]), "companion": bool(captures["records"]),
                 "plugin": plugin["available"]}
    return {"schema": SCHEMA, "status": "partial" if inputs.errors else "available" if any(available.values()) else "no_measurements",
            "observer": str(observer), "availability": available, "host_boundaries": host,
            "native_epochs": native, "companion": captures, "api": {"host": api, "children": children},
            "plugin": plugin, "errors": inputs.errors, "sources": dict(sorted(inputs.sources.items())),
            "limitations": LIMITATIONS}


def render_html(report):
    escape = html.escape
    def value(value, scale=1):
        return "unavailable" if value is None else f"{value / scale:,.3f}"
    def table(headers, rows):
        return "<table><thead><tr>" + "".join("<th>" + escape(str(v)) + "</th>" for v in headers) + "</tr></thead><tbody>" + "".join(
            "<tr>" + "".join("<td>" + escape(str(v)) + "</td>" for v in row) + "</tr>" for row in rows) + "</tbody></table>"
    sections = []
    host = report["host_boundaries"]
    sections.append("<h2>Host boundary envelopes (complete, noncensored only)</h2>" + table(
        ["Kind", "All records", "Complete", "Failed", "Censored", "Complete wall median ms", "Complete wall max ms", "Complete unattributed median ms"],
        [[kind, row["records"], row["complete_records"], row["failed"], row["censored"], value(row["wall_ns"]["median"], 1e6),
          value(row["wall_ns"]["max"], 1e6), value(row["unattributed_ns"]["median"], 1e6)] for kind, row in host["by_kind"].items()]))
    sections.append("<h2>Host attempts by completion state</h2>" + table(
        ["Kind", "State", "Records", "Wall median ms", "Wall max ms"],
        [[kind, state, row["records"], value(row["wall_ns"]["median"], 1e6), value(row["wall_ns"]["max"], 1e6)]
         for kind, group in host["by_kind"].items() for state, row in group["by_state"].items() if row["records"]]))
    sections.append("<h2>Nested host components (complete only, not additive)</h2>" + table(["Component", "Measured", "Missing", "Median ms", "Max ms"],
        [[key, row["n"], row["missing"], value(row["median"], 1e6), value(row["max"], 1e6)] for key, row in host["components"].items()]))
    sections.append("<h2>Native bit collection and census (complete, noncensored only)</h2>" + table(["Metric", "Measured", "Missing", "Median ms", "Max ms"],
        [[key, row["n"], row["missing"], value(row["median"], 1e6), value(row["max"], 1e6)] for key, row in report["native_epochs"]["metrics"].items()]))
    sections.append("<h2>Native attempts by completion state</h2>" + table(
        ["State", "Records", "Idle scan median ms", "Dirty median ms", "Census median ms"],
        [[state, row["records"], value(row["metrics"]["idle_scan_wall_ns"]["median"], 1e6),
          value(row["metrics"]["dirty_wall_ns"]["median"], 1e6), value(row["metrics"]["census_wall_ns"]["median"], 1e6)]
         for state, row in report["native_epochs"]["by_state"].items() if row["records"]]))
    rows = []
    for detail, modes in report["companion"]["by_detail"].items():
        for mode, row in modes.items():
            rows.append([detail, mode, row["records"], row["complete_records"], row["failed"], row["censored"], value(row["wall_ns"]["median"], 1e6),
                         value(row["cpu_ns"]["median"], 1e6), value(row["selected_pages"]["median"]),
                         value(row["provider_totals"]["read_requests"]["median"]), value(row["decoder_closure_bytes"]["median"], 1024)])
    sections.append("<h2>Companion detail modes (complete only; not matched speedups)</h2>" + table(
        ["Detail", "Phase", "All records", "Complete", "Failed", "Censored", "Complete decode wall median ms", "Complete decode CPU median ms", "Selected pages median", "Read requests median", "Closure median KiB"], rows))
    sections.append("<h2>Companion attempts by completion state</h2>" + table(
        ["Detail", "Mode", "State", "Records", "Decode wall median ms", "Decode CPU median ms"],
        [[detail, mode, state, row["records"], value(row["wall_ns"]["median"], 1e6), value(row["cpu_ns"]["median"], 1e6)]
         for detail, modes in report["companion"]["by_detail"].items() for mode, group in modes.items()
         for state, row in group["by_state"].items() if row["records"]]))
    rows = []
    for detail, modes in report["companion"]["by_detail"].items():
        for mode, group in modes.items():
            for scope in ("phases", "stages"):
                for name, row in group[scope].items():
                    rows.append([detail, mode, scope, name, row["executed_records"],
                                 value(row["wall_ns"]["median"], 1e6), value(row["cpu_ns"]["median"], 1e6)])
    sections.append("<h2>Companion phase/stage breakdown (complete only, nested)</h2>" + table(
        ["Detail", "Mode", "Scope", "Stage", "Executed", "Wall median ms", "CPU median ms"], rows))
    apis = [("host", report["api"]["host"])] + [("child " + str(row["capture_id"]), row) for row in report["api"]["children"]]
    sections.append("<h2>API receipts (timings/bytes for successful calls)</h2>" + table(["Scope", "Available", "All calls", "Failed", "Known successful response bytes", "Successful request median ms"],
        [[key, row["available"], row["records"], row["failed"], value(row["response_bytes"]["total"]), value(row["wall_ns"]["median"], 1e6)] for key, row in apis]))
    sections.append("<h2>Plugin caller waits (same process clock)</h2>" + table(["Kind", "Measured", "Median ms", "Max ms"],
        [[key, row["n"], value(row["median"], 1e6), value(row["max"], 1e6)] for key, row in report["plugin"]["by_kind"].items()]))
    sections.append("<p>Unconfirmed plugin requests: " + str(report["plugin"]["unconfirmed_requests"]) +
        "; unmatched confirmations: " + str(report["plugin"]["unmatched_confirmations"]) +
        "; invalid terminal seals: " + str(report["plugin"]["invalid_terminal_seals"]) + ".</p>" +
        table(["Plugin measurement error", "Count"], sorted(report["plugin"]["measurement_errors"].items())))
    sections.append("<h2>Input errors</h2><pre>" + escape(json.dumps(report["errors"], indent=2)) + "</pre>")
    sections.append("<h2>Consumed source artifacts</h2>" + table(["Path", "Bytes", "SHA-256"],
        [[path, row["bytes"], row["sha256"]] for path, row in report["sources"].items()]))
    return "<!doctype html><html lang=en><meta charset=utf-8><meta name=viewport content='width=device-width,initial-scale=1'><title>Tracer overhead</title><style>body{font:15px system-ui;margin:24px;color:#17232b}table{border-collapse:collapse;margin-bottom:24px}th,td{border:1px solid #ccd6db;padding:7px;text-align:left}th{background:#eef4f5}li{margin:7px 0}pre{white-space:pre-wrap}</style><h1>Tracer overhead</h1><p>Status: " + escape(report["status"]) + ". No adjusted workload latency is calculated.</p><ul>" + "".join("<li>" + escape(v) + "</li>" for v in report["limitations"]) + "</ul>" + "".join(sections) + "</html>"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--observer", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True, help="JSON path; a sibling .html report is also written")
    parser.add_argument("--plugin-log", type=Path)
    args = parser.parse_args()
    if args.output.suffix.lower() != ".json":
        parser.error("--output must be a .json path")
    report = summarize(args.observer, args.plugin_log)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(report, indent=2) + "\n")
    args.output.with_suffix(".html").write_text(render_html(report))


if __name__ == "__main__":
    main()
