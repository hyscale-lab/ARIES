import path from "node:path";
import { onInternalDiagnosticEvent, waitForDiagnosticEventsDrained } from "openclaw/plugin-sdk/diagnostic-runtime";
import { createCollector } from "./collector.mjs";

// Native plugin service; no model proxy, tool mutation, or context-content capture.
export default {
  id: "aries-tsb-diagnostics",
  name: "ARIES TSB metadata diagnostics",
  register(api) {
    let collector, unsubscribe;
    const hookEvent = (type, fields) => collector?.accept({
      type, sourceTimestampMs: Date.now(), sourceMonotonicNs: process.hrtime.bigint().toString(), ...fields,
    }, { trusted: false });
    api.on("tool_result_persist", (event, ctx) => {
      hookEvent("native.tool_result.received", {
        toolCallId: event.toolCallId, sessionKey: ctx.sessionKey,
        isSynthetic: event.isSynthetic === true, isError: event.message?.isError === true,
      });
      // This hook is synchronous; return no replacement and preserve native persistence.
    });
    api.on("before_compaction", (_event, ctx) => {
      hookEvent("native.compaction.started", { runId: ctx.runId, sessionKey: ctx.sessionKey, sessionId: ctx.sessionId });
    });
    api.on("after_compaction", (_event, ctx) => {
      hookEvent("native.compaction.completed", { runId: ctx.runId, sessionKey: ctx.sessionKey, sessionId: ctx.sessionId });
    });
    api.registerService({
      id: "aries-tsb-diagnostics",
      start(ctx) {
        const occurrenceId = process.env.ARIES_TSB_OCCURRENCE_ID;
        if (!occurrenceId) throw new Error("ARIES_TSB_OCCURRENCE_ID is required");
        const outputPath = process.env.ARIES_TSB_DIAGNOSTICS_PATH ||
          path.join(ctx.stateDir, "aries-tsb-diagnostics.jsonl");
        collector = createCollector({ outputPath, occurrenceId });
        unsubscribe = onInternalDiagnosticEvent((event, metadata) => collector.accept(event, metadata));
      },
      async stop() {
        await waitForDiagnosticEventsDrained();
        unsubscribe?.();
        collector?.close();
      },
    });
  },
};
