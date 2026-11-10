// Causal reduction uses IDs from source observations, never time-nearness joins.
const finite = value => Number.isFinite(value) ? value : null;
const identity = event => JSON.stringify([event.runId, event.callId, event.trace?.traceId ?? null, event.trace?.spanId ?? null]);
const toolIdentity = (sessionKey, id) => JSON.stringify([sessionKey ?? null, id]);

export function deriveSteps(events, deliveryIssues = []) {
  const models = new Map(), results = new Map(), owners = new Map(), processes = new Map();
  const auxiliary = events.filter(e => e.type.startsWith("native.compaction.") || e.type === "native.agent.ended");
  for (const event of events) {
    if (event.type.startsWith("model.call.") && event.runId && event.callId) {
      const key = identity(event);
      if (!models.has(key)) models.set(key, { starts: [], ends: [] });
      const group = models.get(key);
      (event.type.endsWith(".started") ? group.starts : group.ends).push(event);
    } else if (event.type === "native.tool_result.received" && event.toolCallId) {
      const key = toolIdentity(event.sessionKey, event.toolCallId);
      if (!results.has(key)) results.set(key, []);
      results.get(key).push(event);
    } else if (event.type.startsWith("exec.process.") && event.processSessionId) {
      if (!processes.has(event.processSessionId)) processes.set(event.processSessionId, []);
      processes.get(event.processSessionId).push(event);
    }
  }
  for (const [key, group] of models) {
    if (group.ends.length !== 1) continue;
    const end = group.ends[0];
    if (end.responseStopReason !== "toolUse" || !Array.isArray(end.returnedToolCallIds)) continue;
    for (const id of end.returnedToolCallIds) {
      const toolKey = toolIdentity(end.sessionKey, id);
      if (!owners.has(toolKey)) owners.set(toolKey, []);
      owners.get(toolKey).push(key);
    }
  }
  const steps = [];
  for (const [key, group] of models) {
    const start = group.starts.length === 1 ? group.starts[0] : null;
    const end = group.ends.length === 1 ? group.ends[0] : null;
    // An unpatched source cannot supply an actual causal Step; retain its model window separately.
    if (!Number.isFinite(start?.dispatchWallMs)) continue;
    const reasons = [];
    if (!start.trace?.spanId) reasons.push("missing_attempt_identity");
    if (!end) reasons.push("missing_or_duplicate_model_terminal");
    if (deliveryIssues.length) reasons.push("diagnostic_delivery_incomplete");
    const stopReason = end?.responseStopReason ?? null;
    if (end?.type === "model.call.completed" && stopReason === null) reasons.push("missing_response_stop_reason");
    const responseTime = finite(end?.responseWallMs);
    const expected = stopReason === "toolUse" && Array.isArray(end?.returnedToolCallIds) ? end.returnedToolCallIds : [];
    let endedAt = responseTime, status = "completed";
    const received = [];
    if (end?.type === "model.call.error") {
      endedAt = finite(end.ts);
      status = end.failureKind === "aborted" ? "interrupted" : "failed";
    } else if (!end || responseTime === null || !Array.isArray(end.returnedToolCallIds)) {
      reasons.push("missing_response_metadata");
    } else if (stopReason === "error" || stopReason === "aborted") {
      status = stopReason === "aborted" ? "interrupted" : "failed";
    } else if (stopReason === "length") {
      status = "truncated";
    }
    for (const id of expected) {
      const toolKey = toolIdentity(end.sessionKey, id), candidates = results.get(toolKey) ?? [];
      if ((owners.get(toolKey) ?? []).length !== 1) { reasons.push("ambiguous_tool_id"); continue; }
      if (candidates.length !== 1) { reasons.push("missing_or_duplicate_tool_result"); continue; }
      const result = candidates[0];
      if (result.isSynthetic) { reasons.push("synthetic_tool_result"); continue; }
      const at = finite(result.sourceTimestampMs);
      if (at === null) { reasons.push("missing_tool_result_clock"); continue; }
      received.push({ native_tool_id: id, received_at_ms: at, received_mono_ns: result.sourceMonotonicNs ?? null, is_error: result.isError === true });
      endedAt = endedAt === null ? at : Math.max(endedAt, at);
      if (result.isError) status = "tool_failed";
    }
    if (endedAt !== null && endedAt < start.dispatchWallMs) reasons.push("wall_clock_reversal");
    if (stopReason === "toolUse" && !Array.isArray(end?.returnedToolCallIds)) reasons.push("missing_tool_batch");
    const reasonList = [...new Set(reasons)];
    steps.push({
      step_id: key, native_run_id: start.runId, native_model_call_id: start.callId,
      session_key: start.sessionKey ?? null, trace: start.trace ?? null,
      call_category: start.callCategory ?? "unknown",
      started_at_ms: start.dispatchWallMs, started_mono_ns: start.dispatchMonoNs ?? null,
      response_at_ms: responseTime, response_mono_ns: end?.responseMonoNs ?? null,
      ended_at_ms: reasonList.length ? null : endedAt,
      observed_terminal_at_ms: endedAt,
      status: reasonList.length ? "incomplete" : status, issues: reasonList,
      no_tool: end && Array.isArray(end.returnedToolCallIds) ? expected.length === 0 : null,
      expected_tool_ids: expected, received_tools: received,
      boundary: expected.length ? "model_dispatch_to_last_native_result_received" : "model_dispatch_to_response",
    });
  }
  const jobs = [...processes].map(([id, observations]) => {
    const created = observations.filter(e => e.type === "exec.process.created");
    const backgrounded = observations.filter(e => e.type === "exec.process.backgrounded");
    const exited = observations.filter(e => e.type === "exec.process.exited");
    const origin = created[0] ?? observations[0];
    const matchingOwners = owners.get(toolIdentity(origin.sessionKey, origin.originToolCallId)) ?? [];
    return {
      native_job_id: id, native_origin_tool_id: origin.originToolCallId ?? null,
      parent_step_id: matchingOwners.length === 1 ? matchingOwners[0] : null,
      origin_status: matchingOwners.length === 1 ? "causal_id" : "unknown_or_ambiguous",
      created_at_ms: created.length === 1 ? finite(created[0].sourceTimestampMs) : null,
      backgrounded_at_ms: backgrounded.length === 1 ? finite(backgrounded[0].sourceTimestampMs) : null,
      exited_at_ms: exited.length === 1 ? finite(exited[0].sourceTimestampMs) : null,
      outcome: exited.length === 1 ? exited[0].outcome ?? null : null,
      lifetime_status: exited.length === 1 && created.length === 1 ? "observed_native_process_session" : "censored",
      scope: "native_exec_process_session; detached_guest_descendants_unobserved",
    };
  });
  return { steps, jobs, auxiliary };
}
