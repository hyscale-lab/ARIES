# Native step diagnostic instrumentation

The recovered source patch is now an exact diff against the OpenClaw base in
[`versions.json`](versions.json). Applying it reproduced all eight modified worker
source/test files byte for byte. The current source recovery and setup guide is
[Memory footprint tracing](../../docs/experiments/memory-footprint-tracing.md).
Use that guide for the active VMM tracer, dependency pins and validation commands.

The feasibility observations, worker paths, proposed follow-ups and test/build
status below describe the earlier study. They do not report a new runtime build
or authorize experiment runs.

The collector/reducer fixtures pass, and the plugin registers in the **unmodified pinned image**. The versioned native source patch is a **proposed follow-up**: it has source/apply checks and authored TypeScript tests, but has not been compiled, run in an image, or exercised with an agent. The observer validity gate stopped study admission before that expense. No benchmark Step traces were collected by these scripts.

The inspected upstream tag `v2026.7.1` resolves to `2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4`. The checked image is `ghcr.io/openclaw/openclaw@sha256:6a31d44b2944e7adcd2b582bf6fb463111264ebca97a0201795b799135bd102c`, with Node v24.16.0.

## What the artifacts do

- `collector.mjs` records allowlisted internal model/tool diagnostics, source clocks/IDs and loss markers. On the original image these are partial intervals, not Steps.
- `index.mjs` registers the native service and synchronous `tool_result_persist` observation. It records native compaction hooks separately. It returns no replacement message or tool parameters.
- `steps.mjs` joins only explicit native IDs from patched model metadata to actual, nonsynthetic tool-result receipts. No-tool, failed, interrupted, truncated, missing-result and ambiguous-ID cases remain visible. Background jobs join by native process slug and originating tool ID; their lifetime is separate from the foreground Step.
- `reduce.mjs` reads retained JSONL offline, emits unpatched model windows separately and produces Step records only when source dispatch metadata exists. Missing patch fields, missing/synthetic results, duplicate IDs and data loss prevent complete Step certification.
- `native-openclaw.patch` adds metadata-only source instrumentation in six upstream implementation files and five authored Vitest cases in two existing test files. It changes no shell text, tool arguments, cancellation or process control.

The patch captures embedded primary-chain calls and labels raw model calls separately. It does **not** certify compaction/auxiliary model requests or provider SDK internal HTTP retries. Compaction hook intervals are retained as explicit auxiliary activity. Stream-call IDs must not be described as complete physical-request IDs. No SSH causal association or detached guest-descendant lifetime is inferred.

A native process-session `created` event means registration, before process spawn; `exited` observes the native process/transport lifetime. It is not a guest-kernel per-process scheduler trace. Shell-detached descendants remain unobserved.

## Native registration

The pin exports `onInternalDiagnosticEvent` and `waitForDiagnosticEventsDrained` from `openclaw/plugin-sdk/diagnostic-runtime`. The plugin uses these metadata APIs, never the restricted `ctx.internalDiagnostics` private-content capability. `onDiagnosticEvent` alone omits trusted model/tool events. Native plugin loading supplies SDK aliases; plain Node import from an arbitrary directory is insufficient.

Stage the directory owned by the harness's `node` user or root, then explicitly load it:

```json
{
  "diagnostics": {"enabled": true},
  "plugins": {
    "load": {"paths": ["/run/aries/tsb-steps"]},
    "entries": {"aries-tsb-diagnostics": {"enabled": true}}
  }
}
```

Preserve existing plugin entries and any allowlist; add this ID if an allowlist exists. Set `ARIES_TSB_OCCURRENCE_ID` to the ARIES occurrence/attempt ID, distinct across repeated tasks. `ARIES_TSB_DIAGNOSTICS_PATH` optionally selects the private JSONL output; default is `aries-tsb-diagnostics.jsonl` under the native state directory.

ARIES does not yet stage this plugin or retrieve that file. Native service failures can be nonfatal to the agent; absent evidence is a measurement failure. No conversation-access permission is needed for the three hooks used. A trial `agent_end` hook was denied by native policy and was removed; normal diagnostic run terminal records remain available.

The file is mode 0600, refuses overwrite, and is bounded at 64 MiB including reserved closing metadata. Drops and write failures are explicit. Callbacks write synchronously; overhead must be measured. Native diagnostics can be delayed/dropped by their async queue. Start/close anchors do not calibrate the host observer clock. Abrupt termination can lose events/footer. A clean footer proves only this collector's observed lifetime, not all native call categories.

Prompts, commands, tool parameter/result bodies, private diagnostic payloads, raw errors, credentials and memory contents are excluded. Native source IDs and source wall/monotonic clocks are retained. Filtered global diagnostic sequence gaps alone are not classified as loss.

## Executed checks

On amd205, the final 16 Node behavior fixtures passed in the pinned image (105.4 ms test duration). They cover refusal to fabricate Steps from unpatched diagnostics, patched no-tool/multi-tool Steps, failed/interrupted/truncated responses, synthetic results, overlapping/reused tool IDs, background origin/lifetime, native/collector loss, truncated files, duplicate terminals, clock reversal and content exclusion.

```sh
sudo docker run --rm --network none --cpus 1 --memory 512m --read-only \
  --tmpfs /tmp:size=16m \
  -v /home/nehalem/aries/scripts/tsb-steps:/work:ro -w /work \
  --entrypoint node ghcr.io/openclaw/openclaw:2026.7.1 --test test.mjs
sudo docker run --rm --network none --cpus 1 --memory 1g --read-only \
  --tmpfs /tmp:size=64m \
  -v /home/nehalem/aries/scripts/tsb-steps:/plugin:ro \
  --entrypoint node ghcr.io/openclaw/openclaw:2026.7.1 /plugin/validate-native.mjs
```

The native CLI reported the final plugin loaded/imported, its service and all three typed hooks registered, and no diagnostics. This validates registration, not service startup/event delivery under a live agent.

The source patch passed `git diff --check` and reverse apply-check against the modified exact-pin checkout. Its five TypeScript test cases remain **unexecuted**, and the patched runtime remains **unbuilt**.

Offline reduction:

```sh
node scripts/tsb-steps/reduce.mjs runs/RUN/native-diagnostics.jsonl > runs/RUN/native-diagnostic-summary.json
```

## Smallest next native test/build

Do these only after the study's observer gate permits continued work. On a fresh checkout of the exact native commit, apply the tracked patch. The current worker cache already has it applied; do not apply twice.

```sh
git rev-parse HEAD
git apply --unidiff-zero --check /home/nehalem/aries/scripts/tsb-steps/native-openclaw.patch
git apply --unidiff-zero /home/nehalem/aries/scripts/tsb-steps/native-openclaw.patch
corepack pnpm install --frozen-lockfile
corepack pnpm exec vitest run --config test/vitest/vitest.agents-embedded-agent.config.ts \
  src/agents/embedded-agent-runner/run/attempt.model-diagnostic-events.test.ts --maxWorkers=1
corepack pnpm exec vitest run --config test/vitest/vitest.agents-core.config.ts \
  src/agents/bash-process-registry.test.ts --maxWorkers=1
```

Use the pin's Node 24 toolchain and package-manager pin (pnpm 11.2.2). These test-only commands do not build the UI. Dependency installation and the tests are still future work, not verified commands from this run.

For the reproducible runtime build, prefer the inspected upstream Dockerfile with its digest-pinned base images, frozen lockfile and native postbuild steps:

```sh
sudo docker build --build-arg OPENCLAW_DOCKER_BUILD_SKIP_DTS=1 \
  --build-arg OPENCLAW_DOCKER_BUILD_NODE_OPTIONS=--max-old-space-size=8192 \
  -t aries/openclaw:2026.7.1-tsb-metadata .
```

This builds the original pin plus the metadata patch; record source/patch/image digests. It includes asset/UI stages and needs substantially more resources than fixture validation. No hashed dist-file regex replacement is proposed. After building, repeat native registration and deterministic actual-harness fixtures before admitting any paid stage.

## Native audit and smallest follow-up patch

Load-bearing source locations, relative to the inspected OpenClaw checkout:

- `src/agents/embedded-agent-runner/run/attempt.ts:3280`: existing stream wrapper and attempt-local `diagnosticModelCallSeq`; IDs can restart on another attempt within the same run. `runTrace` is fresh per attempt at lines 1179–1184. Tool context (1348) and model context (3294) receive the same run trace, so their spans are siblings.
- `run/attempt.model-diagnostic-events.ts:846`: actual stream dispatch; 565–628 emit terminal diagnostics. Model plugin hook events omit source timestamps and run through a bounded fire-and-forget queue (16 active, 256 queued, 2000 ms warning, queue overflow drops).
- `src/agents/embedded-agent-runner/run/attempt.ts:4788,5682`: `llm_input`/`llm_output` surround a whole embedded attempt. They are not per-Step hooks.
- `packages/agent-core/src/agent-loop.ts:338–376`: completed response, awaited native tool batch, then `turn_end`; error/aborted response ends at 348–351. `streamAssistantResponse` calls the stream function at 477 after context/key preparation; `turn_start` is therefore too early for the required dispatch boundary.
- `src/agents/agent-tools.before-tool-call.ts:1732–1745,1887–1950`: tool diagnostics carry native toolCallId but no parent model call; terminal diagnostic is before the loop has received all foreground results.
- `src/agents/bash-tools.exec.ts:1565`: native exec receives but names its ID `_toolCallId`; it is not forwarded to `runExecProcess` at 1971.
- `src/agents/bash-tools.exec-runtime.ts:615–655`: pollable process/session slug and start; `bash-process-registry.ts:189,207`: actual exit/background transition. `exec.process.completed` emitted at runtime lines 175–197 contains no slug or originating tool/model ID, so it cannot establish job lifetime/parentage.
- `src/agents/embedded-agent-runner/resource-loader.ts:15`: ambient session extensions are disabled; merely dropping a turn-end extension into the native home will not instrument this path.
- `src/infra/diagnostic-events.ts:897–925,1200–1264`: 10000-entry asynchronous queue; source timestamps/sequence assigned before delivery; only tool terminal events get priority. Drop reports are retained.
- `packages/ai/src/providers/openai-completions.ts:175–188`: provider SDK retry options are inside one wrapped stream invocation. A stream-call ID is not certification of every physical HTTP request/retry.

The proposed patch uses the final native response's tool IDs and source clocks plus the existing synchronous persistence hook; it does not treat tool-execution diagnostics as final foreground receipt. The hook is called during `AgentSession.handleAgentEventUnlocked(message_end)` → `SessionManager.appendMessage` at `src/agents/sessions/agent-session.ts:630–652`, via `session-tool-result-guard-wrapper.ts:113–136`. It carries `isSynthetic`; synthetic records never complete an awaited foreground result.

Remaining work after a validated build: actual dispatch/response/tool fixture reconciliation, aborted in-flight process handling, compaction and physical provider-retry coverage, calibrated cross-process clock alignment, native queue/collector loss under load, plugin retrieval at shutdown, and explicit SSH metadata only if needed. Existing SSH accepts only exec; adding a metadata channel request is a separate protocol change. Until then retain unjoined SSH evidence without temporal matching.

The current artifact cannot admit benchmark H1/H2 claims or satisfy the requested fix-git Step validation.
