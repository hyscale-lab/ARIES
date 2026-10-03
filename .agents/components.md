# Component contracts

| Role | Owns | Implementation |
| --- | --- | --- |
| Benchmark | Task data, sanitization, private verifier, independent evaluation | `pkg/benchmark/*` |
| AgentHarness | Agent process/container, model configuration, private telemetry | `pkg/harness/{hermes,openclaw}` |
| ToolSandbox | Live task environment through evaluation, task execution policy | `pkg/sandbox` |
| ToolBridge | Temporary authenticated access to one exact sandbox; revocation | `pkg/bridge/{hermesssh,openclawssh}` |

Contracts: [runner/interfaces.go](../pkg/runner/interfaces.go) defines
`Benchmark.Tasks/PrepareSandbox/Evaluate`, `AgentHarness.Start/Run/Stop`,
`ToolSandbox.Start/Stop`, and `ToolBridge.Start/Stop`. `Sandbox` is a live
capability, not another role. Its required `NetworkName()` returns the task
attachment; current harnesses require a nonempty shared network name. Keep implementations independent; a paired bridge
may use a narrow sandbox capability.

## Benchmark changes

- Terminal-Bench derives image/workdir from pinned task data; verifier inputs
  remain private until isolation is confirmed.
- SWE-bench Pro pins dataset and evaluator separately. Preserve sanitized Git
  snapshots, candidate capture, private verifier staging, numeric execution
  identity, and all required FAIL_TO_PASS/PASS_TO_PASS checks.
- Deep Research Bench and SWE-Atlas QA keep rubrics/reference material host-side;
  download the agent output only after isolation. DRB's optional FACT pass does
  not change its RACE-derived score.

## Harness and bridge changes

Pair Hermes with `hermes-ssh`, OpenClaw with `openclaw-ssh`. Each bridge implements
its harness's actual protocol. Preserve exact argv and workdir semantics; never
concatenate user commands. Do not widen accepted payloads merely to silence a failure. Hermes credential-file sync is
denied. OpenClaw's ambiguous agent requests must not be retried.

Harnesses own their deployment transports, not task sandboxes or evaluation.
Bridge revocation drains sessions, commands, and evidence and removes temporary
credentials. Replayable input stays private. Attachment comes from
`HarnessRequest.Network`, separately from `ToolEndpoint` credentials.

## Sandbox and deployment changes

Docker uses the Moby SDK; never shell out to Docker. Task environments own network identity,
ownership checks, and bridge address resolution. Remove containers before their
networks. Cancellation must terminate command processes without prematurely
stopping the sandbox needed for evaluation.

Human references: [benchmark](../docs/design/benchmark.md),
[harness](../docs/design/harness.md), [bridge](../docs/design/bridge.md),
[sandbox](../docs/design/sandbox.md), [deployment](../docs/design/deployment.md).
