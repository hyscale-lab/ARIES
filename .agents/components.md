# Component contracts

Follow [Runner interfaces](../pkg/runner/interfaces.go), [design principles](design.md),
and [lifecycle](architecture.md#task-lifecycle).

| Role | Required boundary |
| --- | --- |
| Benchmark | Own tasks, sanitization, private verifiers, and independent upstream evaluation. |
| AgentHarness | Own the agent runtime, model interaction, and private evidence; retain distinct telemetry entries. |
| ToolSandbox | Keep the task environment live through evaluation; create fresh Runner-owned evaluation sandboxes from it on request. |
| ToolBridge | Grant temporary access to one exact sandbox; confirm revocation. |

- Keep implementations independent; a paired bridge may consume a narrow sandbox
  capability. Pair Hermes/OpenClaw with their corresponding SSH bridges.
  Configure harness-generated per-user state (such as default `HOME`) outside
  the task workdir; preserve explicit native per-call overrides.
- Preserve shell bodies and native protocol semantics; adapt provider-owned path
  arguments or filesystem mappings without rewriting agent command text. Resolve an unspecified
  sandbox workdir from the running image before exposing access. Never retry ambiguous
  submissions, widen accepted payloads to hide failures, or enable Hermes credential sync.
- Task/evaluation handles borrow run connectivity. Releasing one must not remove
  the network or close peers' transports. Do not require a private network per pair.
- Reuse shared harness ownership and credential mechanics. Preserve native defaults
  and credential fallbacks. Retain ownership after failed removal. `Stop` prevents admission while preserving
  `Run` snapshots. Redacted errors must not expose secrets through their causes.
- Each sandbox session has its own listener, immutable target binding, and recorder,
  keyed by a stable sandbox ID. Native tool execution must not call Runner or own
  sandbox lifecycle. Use plaintext unauthenticated gRPC and unauthenticated SSH,
  one in-memory service host key, and no client-key staging; preserve
  target/ownership checks.
- Retain tool-operation deadlines and bounded evidence collection. Revocation
  closes access, joins handlers, and finalizes evidence; keep replay inputs private.
  Tool errors alone must not fail revocation. Harness completion is authoritative
  for completed calls; leave sandbox processes intact for evaluation. Apply the [cancellation/cleanup distinction](design.md).
- Preserve benchmark pins, candidate sanitization, verifier privacy, required checks,
  and score meaning. Follow upstream evaluation, including fresh-sandbox methods
  where required; see [benchmark contracts](../docs/design/benchmark.md).
- Keep DRB's [harness task adaptation](../docs/benchmarks/deep-research-bench.md#harness-task-prompt)
  separate from its unchanged dataset question supplied to the judge; record prompt
  changes as experimental method changes.

Details: [harness](../docs/design/harness.md), [bridge](../docs/design/bridge.md),
[sandbox](../docs/design/sandbox.md), [deployment](../docs/design/deployment.md),
[implementations](../docs/implementation/README.md).
