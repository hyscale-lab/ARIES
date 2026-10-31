# AgentHarness

`AgentHarness` owns one task-local agent runtime and its interaction with the
model. It does not own the task sandbox, bridge listener, benchmark evaluation,
or verifier material. The [design principles](../design.md) apply to every
implementation.

## Operations and ownership

The interface is defined in [pkg/runner](../../pkg/runner/interfaces.go); request
and result data live in [pkg/core](../../pkg/core).

| Operation | Contract |
| --- | --- |
| `Prepare(context.Context, core.HarnessRequest) error` | Create the task-local runtime from model configuration, run placement, resource limits, timeout, and artifact directory, without the tool endpoint or task service endpoints, so it can run while the sandbox starts. Reject invalid configuration before allocating. Roll back partial allocations; retain enough ownership state for `Stop` after a failure. |
| `Start(context.Context, core.HarnessRequest) error` | Configure the prepared runtime with the resolved service endpoints and temporary tool endpoint, start the harness, and wait for readiness; prepare first when `Prepare` was not called. Roll back partial allocations as `Prepare` does. |
| `Warm(context.Context, string) (core.HarnessResult, error)` | Before `Run` only, execute a warm-up instruction as its own conversation, which the task's conversation does not see. Keeps no task artifacts. |
| `Run(context.Context, string) (core.HarnessResult, error)` | Execute the task instruction, accepted once, within its deadline. Return harness status and private artifact references; do not interpret this result as benchmark correctness. |
| `Stop(context.Context) error` | Idempotently stop the owned runtime and positively confirm absence. A failure prevents evaluation. |

`HarnessRequest.Connectivity` supplies typed placement and resolved task service
endpoints independently of `ToolEndpoint`, which carries tool connection identity,
credentials, and artifact locations. Harnesses forward placement to `Deployment`
and use the resolved search URL when search is enabled. The Docker provider
interprets `RuntimePlacement.AttachmentID`; the harness does not interpret
network names. `TaskEnvironment.Start` supplies the complete connectivity value,
including any declared search service URL. Neither input transfers ownership of the task environment or sandbox.
Model credentials are runtime
inputs and must not appear in profiles, Docker metadata, structured logs, or
results. Retained configuration, trajectories, audio, and other task data remain
private artifacts.

`HarnessRequest.NoSandboxTools` carries the task's `core.Task.NoSandboxTools`.
When it is set, the harness renders none of its own tools that act in the
sandbox (shell, code execution, file access), so the agent acts through its MCP
servers and the harness's web tools only. A benchmark whose tasks list their
own tools sets it to keep to that list (Toolathlon); every other task keeps all
of the harness's tools.

## Lifecycle, cancellation, and failure

[Runner](../../pkg/runner/runner.go) prepares the harness after bridge startup, or,
with `execution.parallel_start`, while the sandbox starts, and starts it after
sandbox preparation and bridge startup. It checks that the sandbox's placement is
the run's placement the harness was prepared in. Every attempted harness
preparation or start is followed by `Stop`, including
partial startup failures. After `Run` succeeds, fails, or is canceled, Runner uses
a fresh bounded context to stop the harness, then revoke the bridge. Verifier
material remains withheld unless both confirmations succeed. Evaluation then
inspects the same live sandbox and records an independent outcome.

Implementations must propagate cancellation to external work, preserve error
meaning, and clean up partially acquired resources. A timed-out request does not
prove that the agent runtime has stopped. Closing a deployment transport is also
separate from confirming runtime absence.

## Deployment and substitution

A harness receives the shared [Deployment contract](deployment.md) through an
explicit constructor. Component behavior remains in its concrete package;
implementation selection belongs in `cmd/aries`, while configuration translation,
construction, and rollback belong in `internal/app/wiring/harness`.

Both concrete managers compose a named `runtime *harness.Runtime` from
[pkg/harness](../../pkg/harness/harness.go). This common implementation owns
allocation, rollback, single-instruction admission, cleanup coordination, and
credential/artifact mechanics. `RuntimeOptions` carries the shared deployment,
image, output directory, timeouts, logger, and credential lookup. Common mode,
web, subagent, and MCP inputs use `harness.Options`; each native constructor
applies its own defaults and supported modes. The consumed `runner.AgentHarness`
interface is the one shown above.

Native managers retain configuration rendering, protocol readiness, request and
cancellation behavior, voice handling, and result interpretation. Shared search
validation lives in `pkg/harness/config.go`; connectivity data stays in `pkg/core`
because it crosses component boundaries. A new implementation reuses the common
runtime while supplying its native files, commands, and protocol operations.

For both OpenClaw and Hermes, Runner calls `Prepare` to create the container,
`Start` to stage the configuration that names the sandbox and start the
Gateway, any `Warm` calls, then `Run` to execute the instruction. A warm-up is
a new Gateway session (Hermes) or agent session key (OpenClaw). Hermes reads
the settings that name the sandbox and task services from a staged file its
launcher exports, because its container environment is fixed at creation. The manager obtains a private service endpoint through
deployment operations, and a Gateway client performs the agent protocol. Shutdown uses deployment operations to confirm absence.
Changing hosting must preserve Gateway semantics, request correlation,
cancellation, credentials, artifacts, and isolation.

Current implementations are [OpenClaw and Hermes](../implementation/harnesses.md).
New harnesses must preserve these lifecycle and ownership guarantees, with tests
for partial startup, run failure, cancellation, idempotent stop, positive absence,
credentials, and artifacts. Concrete voice modes and MCP configuration do not
add Runner roles; MCP server placement has its own execution boundary described
in the implementation page.
