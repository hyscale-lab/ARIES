# Component contracts

Follow [Runner interfaces](../pkg/runner/interfaces.go) and [lifecycle](architecture.md).

| Role | Required boundary |
| --- | --- |
| Benchmark | Own tasks, sanitization, private verifier, and independent evaluation. |
| AgentHarness | Own the agent runtime, model interaction, and private evidence; retain distinct telemetry entries. |
| ToolSandbox | Keep the task environment alive through evaluation. |
| ToolBridge | Grant temporary access to one exact sandbox; confirm revocation. |

- Keep implementations independent; a paired bridge may consume a narrow sandbox
  capability. Pair Hermes/OpenClaw with their corresponding SSH bridges.
- Preserve exact argv/workdir and native protocol semantics. Never retry ambiguous
  submissions, widen accepted payloads to hide failures, or enable Hermes credential sync.
- Pass placement and resolved service URLs explicitly; deployment interprets
  attachment details. Keep these separate from tool credentials.
- Reuse shared harness ownership and credential mechanics. Retain ownership on
  failed removal; Run snapshots survive Stop, which prevents new admission.
  Keep native defaults and credential fallback policies intact; redacted errors
  must not expose secrets through their cause chain.
- Revoke bridges only after draining sessions, commands, and evidence. Keep replay
  inputs private. Use Moby for Docker; remove runtimes before their attachments.
  Command cancellation must preserve the sandbox needed for evaluation.
- Preserve benchmark-specific isolation: pinned inputs, sanitized candidate state,
  private test/reference material, and all required verifier checks. Do not change
  score meaning when refactoring evaluation.

Details: [benchmark](../docs/design/benchmark.md), [harness](../docs/design/harness.md),
[bridge](../docs/design/bridge.md), [sandbox](../docs/design/sandbox.md),
[deployment](../docs/design/deployment.md), [implementations](../docs/implementation/README.md).
