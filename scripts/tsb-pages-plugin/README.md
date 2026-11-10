# Native OpenClaw LLM-step page barriers

The observation unit is one LLM reasoning step: a complete native assistant
tool-request message through all its expected native tool results ready for the
next LLM request. Sequential or parallel native calls and their expanded sandbox
operations share one VM-page observation. The hierarchy is Session → user/harness
Turn → LLM Step → native tool calls → bridge operations. Individual call IDs and
hook times remain evidence; distinct per-call page sets are not measured.

The plugin returns no message, argument or tool-result replacement. It preserves
native scheduling, tool preparation, errors and result delivery. It sends only
IDs, names, source clocks and completion metadata; assistant text, reasoning,
arguments, results and credentials are not copied into observer requests/logs.

Enable the existing ARIES OpenClaw agent-only option:

~~~json
"step_observation": {
  "plugin_dir": "/home/nehalem/aries/scripts/tsb-pages-plugin",
  "observer_url": "http://WORKER_BRIDGE_ADDRESS:9100"
}
~~~

ARIES stages exactly index.mjs, end-request.mjs, openclaw.plugin.json and
package.json as private uid/gid 1000 files. It preserves native model/tool
configuration, derives ARIES_STEP_SESSION_ID from [RunID,TaskID,AttemptID] and sets
ARIES_STEP_TURN_ID=1. The telemetry log uses the existing native archive path.

The synchronous before_message_write hook observes an executable assistant
message after native provider ID normalization, before executeToolCalls starts.
It declares batch_id/llm_step_id and the full ordered native call ID/name list
through synchronous POST /batch. The observer captures the VM and arms once
before acknowledging this declaration. batch_requested and batch_confirmed
telemetry retain the original assistant ID/timestamp when available and the
metadata-only declaration. A native before_tool_call acknowledgement annotates
admission; it never resets the bitmap.

The synchronous tool_result_persist hook sends POST /end for each expected ID.
Immediate argument-validation or missing-tool errors may legitimately have no
before_tool_call event. Intermediate acknowledgements say pending, including
when active native calls temporarily reach zero. The last expected result causes
one collection and a collected acknowledgement before result persistence and the
next reasoning request proceed. No individual completion blocks waiting for a
different future call, so native sequential execution does not deadlock.

The pinned runtime is 2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4. Its agent-loop.ts
awaits assistant message_end before executeToolCalls; Agent awaits its listeners
and AgentSession appends the message synchronously. before_message_write and
tool_result_persist are synchronous hooks and are not conversation-access-gated.
llm_input and llm_output are fire-and-forget attempt notifications here, so they
are not used as per-response barriers.

Both HTTP transports allow 120 seconds; the synchronous Node helper has a
125-second outer deadline and is given PATH only. Missing/duplicate/foreign IDs,
synthetic results, failed acknowledgements and incomplete batches remain
measurement errors. Native task behavior continues without hidden retries.
Observation validity concerns declared-batch membership and result completion,
not complete or exclusively causal page coverage. The VMM keeps running through
finite metadata scans; background descendants can outlive result delivery.

Worker validation uses node --test test.mjs inside the pinned image for
sequential, immediate-error and parallel hook flows, synchronization, ID reuse,
service registry reuse, payload exclusion and exact message preservation.
validate-native.mjs verifies native plugin registration. A real no-model fixture
must additionally establish native batch membership, bridge coverage and final
collection before the next model dispatch. Historical busy-period/segmented
prototypes remain archived as separate methods.
