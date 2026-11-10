#!/usr/bin/env node
import fs from "node:fs";
import { pathToFileURL } from "node:url";
import { SCHEMA } from "./collector.mjs";
import { deriveSteps } from "./steps.mjs";

export function reduceText(text) {
  const issues = new Set(), records = [];
  const lines = text.split("\n");
  for (let i = 0; i < lines.length; i++) {
    if (!lines[i].trim()) continue;
    try {
      const item = JSON.parse(lines[i]);
      if (item.schema !== SCHEMA) issues.add("unsupported_schema");
      else records.push(item);
    } catch { issues.add(i === lines.length - 1 ? "truncated_last_record" : "invalid_json_record"); }
  }
  const occurrences = new Set(records.map(r => r.occurrence_id));
  if (occurrences.size !== 1) issues.add("missing_or_mixed_occurrence_identity");
  const starts = records.filter(r => r.record === "collector_started");
  const closes = records.filter(r => r.record === "collector_closed");
  if (starts.length !== 1) issues.add("missing_or_duplicate_collector_start");
  if (closes.length !== 1) issues.add("missing_or_duplicate_collector_close");
  if (closes.some(r => r.collector_dropped > 0)) issues.add("collector_dropped_events");
  if (closes.some(r => r.write_error)) issues.add("collector_write_error");
  const rows = records.filter(r => r.record === "event");
  if (closes.length === 1 && closes[0].events !== rows.length) issues.add("collector_event_count_mismatch");
  if (rows.some(r => r.event.type === "diagnostic.async_queue.dropped")) issues.add("native_queue_dropped_events");
  const model = new Map(), tool = new Map(), jobs = [];
  const add = (map, event, kind) => {
    const id = kind === "model" ? event.callId : event.toolCallId;
    if (!event.runId || !id) { issues.add(kind + "_missing_native_identity"); return; }
    // Native model sequence restarts each attempt: trace identity is part of the key.
    const key = JSON.stringify([event.runId, id, event.trace?.traceId ?? null, event.trace?.spanId ?? null]);
    if (!map.has(key)) map.set(key, { native_run_id: event.runId, native_id: id, trace: event.trace ?? null, starts: [], terminals: [] });
    const group = map.get(key);
    (event.type.endsWith(".started") ? group.starts : group.terminals).push(event);
    if (!event.trace?.spanId) issues.add(kind + "_attempt_identity_unavailable");
    if (event.trusted !== true) issues.add(kind + "_untrusted_diagnostic");
  };
  for (const { event } of rows) {
    if (event.type.startsWith("model.call.")) add(model, event, "model");
    else if (event.type.startsWith("tool.execution.")) add(tool, event, "tool");
    else if (event.type === "exec.process.completed") jobs.push(event);
  }
  const windows = (map, kind) => [...map.values()].map(g => {
    const local = [];
    if (g.starts.length !== 1) local.push("missing_or_duplicate_start");
    if (g.terminals.length !== 1) local.push("missing_or_duplicate_terminal");
    const start = g.starts.length === 1 ? g.starts[0] : null;
    const end = g.terminals.length === 1 ? g.terminals[0] : null;
    const s = Number.isFinite(start?.ts) ? start.ts : null;
    const e = Number.isFinite(end?.ts) ? end.ts : null;
    if (s === null || e === null) local.push("unknown_boundary");
    if (s !== null && e !== null && e < s) local.push("wall_clock_reversal");
    for (const reason of local) issues.add(kind + "_" + reason);
    return {
      native_run_id: g.native_run_id, native_id: g.native_id, trace: g.trace,
      start_source_ms: s, end_source_ms: e,
      native_duration_ms: Number.isFinite(end?.durationMs) ? end.durationMs : null,
      native_terminal_type: end?.type ?? null,
      outcome: end?.type.endsWith(".error") ? "error" : end?.type.endsWith(".blocked") ? "blocked" : end ? "completed" : "unknown",
      interval_status: local.length ? "incomplete" : "observed",
      issues: local, start_records: g.starts.length, terminal_records: g.terminals.length,
      ...(kind === "model" ? { call_category: "unknown", response_status: "unknown", step_status: "unsupported_causal_join", resulting_tool_ids: null, no_tool: null } :
        { parent_model_call_id: null }),
    };
  });
  const modelWindows = windows(model, "model"), toolWindows = windows(tool, "tool");
  if (!modelWindows.length) issues.add("no_model_observations");
  const causal = deriveSteps(rows.map(r => r.event), [...issues]);
  const observedSteps = causal.steps.length > 0 && causal.steps.length === modelWindows.length && causal.steps.every(s => s.status !== "incomplete");
  return {
    schema: "aries.tsb-diagnostic-summary.v1",
    occurrence_id: occurrences.size === 1 ? [...occurrences][0] : null,
    coverage: {
      step_contract: observedSteps ? "primary_chain_observed; auxiliary_retry_coverage_unverified" : "unsupported_or_incomplete",
      causal_model_tool_join: causal.steps.length ? "native_ids" : "unsupported",
      background_job_identity: causal.jobs.length ? "native_process_sessions_only; detached_descendants_unobserved" : "unsupported", ssh_join: "unsupported",
      auxiliary_and_internal_retry_coverage: "unverified",
      event_delivery: issues.size ? "incomplete" : "observed_collector_lifetime_only",
      clock_alignment: "uncalibrated_wall_clock_with_receipt_anchors",
    },
    issues: [...issues].sort(), collector_anchors: [...starts, ...closes].map(r => r.anchor),
    model_windows: modelWindows, tool_windows: toolWindows,
    unjoined_process_completions: jobs, step_records: causal.steps, native_jobs: causal.jobs, auxiliary_intervals: causal.auxiliary,
    interpretation: "Only patched source metadata plus synchronous native tool-result receipt can produce primary-chain Steps. Unpatched model windows remain non-Step evidence; no temporal matching is performed.",
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  if (process.argv.length !== 3) {
    console.error("Usage: node scripts/tsb-steps/reduce.mjs DIAGNOSTICS.jsonl");
    process.exitCode = 2;
  } else {
    process.stdout.write(JSON.stringify(reduceText(fs.readFileSync(process.argv[2], "utf8")), null, 2) + "\n");
  }
}
