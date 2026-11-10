import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { createCollector, selectMetadata, SCHEMA } from "./collector.mjs";
import { reduceText } from "./reduce.mjs";

const trace = spanId => ({ traceId: "1".repeat(32), spanId, parentSpanId: "0".repeat(16) });
const model = (type, ts, spanId = "a".repeat(16), extra = {}) => ({
  type: "model.call." + type, runId: "run", callId: "run:model:1",
  ts, seq: ts, trace: trace(spanId), trusted: true, ...extra,
});
const tool = (type, ts, id = "tool-a") => ({
  type: "tool.execution." + type, runId: "run", toolCallId: id, ts,
  trace: trace(id.padEnd(16, "b")), trusted: true,
});
function exportText(events, { closed = true, dropped = 0 } = {}) {
  const wrap = r => ({ schema: SCHEMA, occurrence_id: "attempt-a", ...r });
  return [
    wrap({ record: "collector_started", anchor: { wall_ms: 1, monotonic_ns: "1" } }),
    ...events.map(event => wrap({ record: "event", event })),
    ...(closed ? [wrap({ record: "collector_closed", events: events.length, collector_dropped: dropped })] : []),
  ].map(r => JSON.stringify(r)).join("\n") + "\n";
}

test("model-only fixture cannot certify a no-tool Step", () => {
  const result = reduceText(exportText([model("started", 10), model("completed", 20)]));
  assert.equal(result.model_windows[0].interval_status, "observed");
  assert.equal(result.model_windows[0].no_tool, null);
  assert.equal(result.model_windows[0].response_status, "unknown");
  assert.equal(result.coverage.step_contract, "unsupported_or_incomplete");
  assert.deepEqual(result.step_records, []);
});
test("single and parallel foreground tools stay causally unjoined", () => {
  const result = reduceText(exportText([
    model("started", 10), model("completed", 20),
    tool("started", 21), tool("started", 22, "tool-b"),
    tool("completed", 30, "tool-b"), tool("completed", 40),
  ]));
  assert.equal(result.model_windows[0].end_source_ms, 20);
  assert.equal(result.tool_windows.length, 2);
  assert.ok(result.tool_windows.every(t => t.parent_model_call_id === null));
  assert.equal(result.model_windows[0].resulting_tool_ids, null);
});
test("failed request is retained independently of later attempt using same call ID", () => {
  const result = reduceText(exportText([
    model("started", 10), model("error", 12),
    model("started", 15, "c".repeat(16)), model("completed", 20, "c".repeat(16)),
  ]));
  assert.equal(result.model_windows.length, 2);
  assert.deepEqual(result.model_windows.map(m => m.outcome), ["error", "completed"]);
  assert.ok(result.model_windows.every(m => m.call_category === "unknown"));
});
test("interruption, missing start, and truncated export remain incomplete", () => {
  const partial = reduceText(exportText([model("started", 10)], { closed: false }) + '{"schema":');
  assert.ok(partial.issues.includes("truncated_last_record"));
  assert.ok(partial.issues.includes("missing_or_duplicate_collector_close"));
  assert.equal(partial.model_windows[0].end_source_ms, null);
  const orphan = reduceText(exportText([model("error", 20)]));
  assert.equal(orphan.model_windows[0].start_source_ms, null);
});
test("background completion cannot be joined to nearby tool or job", () => {
  const event = { type: "exec.process.completed", ts: 50, sessionKey: "native-session", durationMs: 29 };
  const result = reduceText(exportText([
    model("started", 10), model("completed", 20),
    tool("started", 21), tool("completed", 23), event,
  ]));
  assert.equal(result.unjoined_process_completions.length, 1);
  assert.equal(result.model_windows[0].end_source_ms, 20);
  assert.equal(result.coverage.background_job_identity, "unsupported");
});
test("native queue loss, collector loss, duplicate terminals, and clock reversal are visible", () => {
  const result = reduceText(exportText([
    model("started", 20), model("completed", 10),
    { type: "diagnostic.async_queue.dropped", ts: 15, droppedEvents: 3 },
  ], { dropped: 2 }));
  assert.ok(result.issues.includes("model_wall_clock_reversal"));
  assert.ok(result.issues.includes("native_queue_dropped_events"));
  assert.ok(result.issues.includes("collector_dropped_events"));
  const duplicate = reduceText(exportText([model("started", 1), model("completed", 2), model("completed", 3)]));
  assert.equal(duplicate.model_windows[0].interval_status, "incomplete");
});
test("metadata whitelist rejects content while preserving causal identifiers and source clocks", () => {
  const event = selectMetadata({
    ...model("started", 20), prompt: "private", command: "secret",
    params: { password: "secret" }, outputMessages: ["private"],
    error: "secret", trace: { ...trace("a".repeat(16)), content: "secret" },
  }, { trusted: true });
  const json = JSON.stringify(event);
  assert.ok(!json.includes("secret") && !json.includes("private"));
  assert.equal(event.callId, "run:model:1");
  assert.equal(event.ts, 20);
});
test("bounded collector retains a close marker with dropped count", () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "aries-tsb-fixture-"));
  try {
    const filename = path.join(directory, "events.jsonl");
    const writer = createCollector({ outputPath: filename, occurrenceId: "attempt-a", maxBytes: 8192 });
    for (let i = 0; i < 100; i++) writer.accept(model("started", i), { trusted: true });
    writer.close();
    writer.close();
    assert.ok(fs.statSync(filename).size <= 8192);
    const parsed = reduceText(fs.readFileSync(filename, "utf8"));
    assert.ok(parsed.issues.includes("collector_dropped_events"));
    assert.ok(!parsed.issues.includes("missing_or_duplicate_collector_close"));
    assert.throws(() => createCollector({ outputPath: filename, occurrenceId: "attempt-a" }));
  } finally { fs.rmSync(directory, { recursive: true, force: true }); }
});
test("missing trace identity and event-count mismatch are not silently certified", () => {
  const m = model("started", 10); delete m.trace;
  const result = reduceText(exportText([m, { ...m, type: "model.call.completed", ts: 12 }]));
  assert.ok(result.issues.includes("model_attempt_identity_unavailable"));
  const mismatched = reduceText(exportText([model("started", 1)]).replace('"events":1', '"events":2'));
  assert.ok(mismatched.issues.includes("collector_event_count_mismatch"));
});

function patchedPair({ ids = [], stop = "stop", span = "a".repeat(16), call = "run:model:1" } = {}) {
  return [
    model("started", 10, span, { callId: call, sessionKey: "session", dispatchWallMs: 9, dispatchMonoNs: "900", callCategory: "primary_agent_chain" }),
    model("completed", 20, span, { callId: call, sessionKey: "session", responseWallMs: 19, responseMonoNs: "1900", returnedToolCallIds: ids, responseStopReason: stop }),
  ];
}
function receipt(id, at, extra = {}) {
  return { type: "native.tool_result.received", sessionKey: "session", toolCallId: id, sourceTimestampMs: at, sourceMonotonicNs: String(at * 100), isSynthetic: false, ...extra };
}
test("patched no-tool source closes at original response time, not callback/diagnostic time", () => {
  const result = reduceText(exportText(patchedPair()));
  assert.equal(result.step_records.length, 1);
  assert.equal(result.step_records[0].started_at_ms, 9);
  assert.equal(result.step_records[0].ended_at_ms, 19);
  assert.equal(result.step_records[0].no_tool, true);
});
test("patched multi-tool Step closes at last required synchronous native receipt", () => {
  const result = reduceText(exportText([
    ...patchedPair({ ids: ["a", "b"], stop: "toolUse" }),
    receipt("unrelated", 999), receipt("b", 31), receipt("a", 50),
  ]));
  assert.equal(result.step_records[0].ended_at_ms, 50);
  assert.equal(result.step_records[0].received_tools.length, 2);
  assert.equal(result.step_records[0].status, "completed");
});
test("synthetic and missing results never certify awaited completion", () => {
  const synthetic = reduceText(exportText([
    ...patchedPair({ ids: ["a"], stop: "toolUse" }), receipt("a", 30, { isSynthetic: true }),
  ]));
  assert.equal(synthetic.step_records[0].ended_at_ms, null);
  assert.ok(synthetic.step_records[0].issues.includes("synthetic_tool_result"));
  const missing = reduceText(exportText(patchedPair({ ids: ["a"], stop: "toolUse" })));
  assert.ok(missing.step_records[0].issues.includes("missing_or_duplicate_tool_result"));
});
test("reused tool IDs across attempts are ambiguous, not nearest-time matched", () => {
  const result = reduceText(exportText([
    ...patchedPair({ ids: ["a"], stop: "toolUse" }),
    ...patchedPair({ ids: ["a"], stop: "toolUse", span: "c".repeat(16), call: "run:model:2" }),
    receipt("a", 30),
  ]));
  assert.equal(result.step_records.length, 2);
  assert.ok(result.step_records.every(s => s.issues.includes("ambiguous_tool_id")));
});
test("native background job keeps causal origin and lifetime after Step completion", () => {
  const life = (type, at) => ({
    type: "exec.process." + type, processSessionId: "job-a", originToolCallId: "a",
    sessionKey: "session", sourceTimestampMs: at,
  });
  const result = reduceText(exportText([
    ...patchedPair({ ids: ["a"], stop: "toolUse" }), receipt("a", 25),
    life("created", 21), life("backgrounded", 24), { ...life("exited", 100), outcome: "completed" },
  ]));
  assert.equal(result.step_records[0].ended_at_ms, 25);
  assert.equal(result.native_jobs[0].exited_at_ms, 100);
  assert.equal(result.native_jobs[0].parent_step_id, result.step_records[0].step_id);
});
test("patched error, aborted, and truncated responses stay in Step sequence", () => {
  for (const [stop, status] of [["error", "failed"], ["aborted", "interrupted"], ["length", "truncated"]]) {
    const result = reduceText(exportText(patchedPair({ stop })));
    assert.equal(result.step_records[0].status, status);
    assert.equal(result.step_records[0].ended_at_ms, 19);
  }
});
test("native thrown model failure closes failed dispatch without fabricating a response", () => {
  const [start] = patchedPair();
  const error = model("error", 22, "a".repeat(16), { sessionKey: "session" });
  const result = reduceText(exportText([start, error]));
  assert.equal(result.step_records[0].status, "failed");
  assert.equal(result.step_records[0].response_at_ms, null);
  assert.equal(result.step_records[0].ended_at_ms, 22);
});
