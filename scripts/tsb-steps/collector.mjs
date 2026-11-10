// Metadata-only collector for OpenClaw v2026.7.1 diagnostics.
import fs from "node:fs";
import path from "node:path";

export const SCHEMA = "aries.tsb-native-diagnostics.v1";
const TYPES = new Set([
  "run.started", "run.completed", "harness.run.completed", "harness.run.error",
  "model.call.started", "model.call.completed", "model.call.error",
  "tool.execution.started", "tool.execution.completed", "tool.execution.error",
  "tool.execution.blocked", "exec.process.completed", "diagnostic.async_queue.dropped",
  "exec.process.created", "exec.process.backgrounded", "exec.process.exited",
  "native.tool_result.received", "native.compaction.started", "native.compaction.completed", "native.agent.ended",
]);
const TEXT_FIELDS = [
  "runId", "callId", "sessionKey", "sessionId", "agentId", "provider", "model",
  "api", "transport", "toolName", "toolCallId", "toolSource", "toolOwner",
  "outcome", "target", "mode", "failureKind", "terminalReason", "errorCategory",
  "dispatchMonoNs", "responseMonoNs", "responseStopReason", "callCategory",
  "processSessionId", "originToolCallId", "sourceMonotonicNs",
];
const NUMBER_FIELDS = [
  "seq", "ts", "sourceTimestampMs", "durationMs", "requestPayloadBytes",
  "responseStreamBytes", "timeToFirstByteMs", "exitCode", "droppedCount",
  "maxQueueLength", "drainBatchSize", "droppedEvents", "droppedTrustedEvents",
  "droppedUntrustedEvents", "droppedPriorityEvents", "queueLength",
  "dispatchWallMs", "responseWallMs", "processStartedAtMs",
];

export function selectMetadata(event, metadata = {}) {
  if (!event || !TYPES.has(event.type)) return null;
  // This exported SDK listener sees internal metadata, never the private payload.
  const clean = { type: event.type, trusted: metadata.trusted === true };
  for (const key of TEXT_FIELDS) {
    if (typeof event[key] === "string" && event[key].length <= 512) clean[key] = event[key];
  }
  for (const key of NUMBER_FIELDS) {
    if (Number.isFinite(event[key])) clean[key] = event[key];
  }
  for (const key of ["timedOut", "backgrounded", "isSynthetic", "isError", "success"]) {
    if (typeof event[key] === "boolean") clean[key] = event[key];
  }
  if (Array.isArray(event.returnedToolCallIds) && event.returnedToolCallIds.length <= 1024 &&
      event.returnedToolCallIds.every(id => typeof id === "string" && id.length <= 512)) {
    clean.returnedToolCallIds = [...event.returnedToolCallIds];
  }
  if (event.trace && typeof event.trace === "object") {
    const trace = {};
    for (const key of ["traceId", "spanId", "parentSpanId", "traceFlags"]) {
      if (typeof event.trace[key] === "string" && event.trace[key].length <= 128) {
        trace[key] = event.trace[key];
      }
    }
    clean.trace = trace;
  }
  // No prompts, shell bodies, tool parameters/results, error messages, or memory content.
  return clean;
}

export function createCollector({ outputPath, occurrenceId, maxBytes = 64 * 1024 * 1024 }) {
  if (!occurrenceId || typeof occurrenceId !== "string") throw new Error("occurrenceId is required");
  if (!Number.isSafeInteger(maxBytes) || maxBytes < 8192) throw new Error("invalid maxBytes");
  fs.mkdirSync(path.dirname(outputPath), { recursive: true, mode: 0o700 });
  const fd = fs.openSync(outputPath, "wx", 0o600);
  let bytes = 0, events = 0, dropped = 0, closed = false, writeError = false;
  const anchor = { wall_ms: Date.now(), monotonic_ns: process.hrtime.bigint().toString() };
  const write = (record, reserve = 4096) => {
    const line = JSON.stringify({ schema: SCHEMA, occurrence_id: occurrenceId, ...record }) + "\n";
    const size = Buffer.byteLength(line);
    if (bytes + size > maxBytes - reserve) return false;
    fs.writeFileSync(fd, line);
    bytes += size;
    return true;
  };
  write({
    record: "collector_started", clock: "harness_process_wall_ms",
    receipt_clock: "harness_process_monotonic_ns", anchor,
    native_pin: "v2026.7.1", native_source_commit: "2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4",
    max_bytes: maxBytes,
    boundary: "native_stream_function_dispatch_to_stream_terminal",
    limitations: [
      "Causal Step/job fields require the versioned native metadata patch",
      "No SSH causal association",
      "No certification of auxiliary/provider-internal retry coverage",
      "Source timestamps are wall clock; receipt timestamps are not source boundaries",
      "Native async queue may drop events; process termination may truncate this file",
    ],
  });
  return {
    accept(event, metadata) {
      if (closed) return;
      const selected = selectMetadata(event, metadata);
      if (!selected) return;
      try {
        if (write({ record: "event", receipt_monotonic_ns: process.hrtime.bigint().toString(), event: selected })) events++;
        else dropped++;
      } catch {
        writeError = true;
        dropped++;
      }
    },
    close() {
      if (closed) return;
      closed = true;
      try {
        write({
          record: "collector_closed", events, collector_dropped: dropped,
          write_error: writeError, anchor: {
            wall_ms: Date.now(), monotonic_ns: process.hrtime.bigint().toString(),
          },
          completeness: "collector_lifetime_only; semantic_coverage_requires_reduction",
        }, 0);
        fs.fsyncSync(fd);
      } finally { fs.closeSync(fd); }
    },
  };
}
