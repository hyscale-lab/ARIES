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
- Pass placement and resolved service URLs explicitly; deployment interprets
  attachment details. Keep these separate from tool credentials.
- Reuse shared harness ownership and credential mechanics. Retain ownership on
  failed removal; Run snapshots survive Stop, which prevents new admission.
  Keep native defaults and credential fallback policies intact; redacted errors
  must not expose secrets through their cause chain.
- Give each managed bridge occurrence its own runtime and immutable assignment.
  Native tool execution must not call back into Runner or own sandbox lifecycle.
  Use plaintext, unauthenticated gRPC control in the trusted research environment;
  retain SSH authentication and instance, assignment, and exact-target checks.
  Runner explicitly revokes assignments; retain operation deadlines and bounded
  evidence collection without leases or controller-liveness supervision.
  Close bridge-owned access/handlers, collect finalized evidence, then confirm
  runtime removal. Harness completion is authoritative for completed tool calls;
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
