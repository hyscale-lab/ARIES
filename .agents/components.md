# Component contracts

Follow [Runner interfaces](../pkg/runner/interfaces.go) and [lifecycle](architecture.md).

| Role | Required boundary |
| --- | --- |
| Benchmark | Own tasks, sanitization, private verifier, and independent evaluation that follows the original methodology. |
| AgentHarness | Own the agent runtime, model interaction, and private evidence; retain distinct telemetry entries. |
| ToolSandbox | Keep the task environment alive through evaluation; start fresh evaluation sandboxes on request. |
| ToolBridge | Grant temporary access to one exact sandbox; confirm revocation. |

- Keep implementations independent; a paired bridge may consume a narrow sandbox
  capability. Pair Hermes/OpenClaw with their corresponding SSH bridges.
  Keep harness-generated per-user state (such as `HOME`) out of the task workdir
  so it never becomes part of the evaluated candidate.
- Preserve exact argv/workdir and native protocol semantics. Never retry ambiguous
  submissions, widen accepted payloads to hide failures, or enable Hermes credential sync.
- Pass resolved tool/service endpoints to consumers and opaque placement handles
  to deployment. Deployment owns attachment, publication, and address resolution;
  a component must not require a private network per harness/sandbox pair.
  Fresh task/evaluation handles borrow run connectivity; releasing one must not
  remove the network or close peers' transports.
- Reuse shared harness ownership and credential mechanics. Retain ownership on
  failed removal; Run snapshots survive Stop, which prevents new admission.
  Keep native defaults and credential fallback policies intact; redacted errors
  must not expose secrets through their cause chain.
- Share one bridge service per run; give each sandbox its own listener, immutable
  execution binding and recorder, addressed by one stable sandbox ID.
  Native tool execution must not call back into Runner or own sandbox lifecycle.
  Use plaintext unauthenticated gRPC and unauthenticated SSH in the trusted
  research environment, with one in-memory service host key and no client-key
  staging. Preserve exact-target and ownership validation.
  Runner explicitly releases sessions; retain operation deadlines and bounded
  evidence collection without leases or controller-liveness supervision.
  Close session-owned access/handlers and collect finalized evidence; remove the
  shared runtime at run cleanup. Harness completion is authoritative for completed
  tool calls;
  leave sandbox processes intact for evaluation. Keep replay inputs private;
  use Moby for Docker.
  Command cancellation must preserve the sandbox needed for evaluation.
- Preserve benchmark-specific isolation: pinned inputs, sanitized candidate state,
  private test/reference material, and all required verifier checks. Do not change
  score meaning when refactoring evaluation.
- Replicate the benchmark's upstream evaluation as closely as possible rather
  than inventing equivalents. SWE-bench Pro captures the agent's patch, then runs
  the upstream entry script in a fresh sandbox from the task image.

Details: [benchmark](../docs/design/benchmark.md), [harness](../docs/design/harness.md),
[bridge](../docs/design/bridge.md), [sandbox](../docs/design/sandbox.md),
[deployment](../docs/design/deployment.md), [implementations](../docs/implementation/README.md).
