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
| `Start(context.Context, core.HarnessRequest) error` | Start the task-local runtime using model configuration, typed runtime placement and resolved service endpoints, temporary tool endpoint, resource limits, timeout, and artifact directory. Roll back partial allocations; retain enough ownership state for `Stop` after a failed start. |
| `Run(context.Context, string) (core.HarnessResult, error)` | Execute the task instruction within its deadline. Return harness status and private artifact references; do not interpret this result as benchmark correctness. |
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

## Lifecycle, cancellation, and failure

[Runner](../../pkg/runner/runner.go) starts the harness after sandbox preparation
and bridge startup. Every attempted harness start is followed by `Stop`, including
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
interface remains unchanged.

Native managers retain configuration rendering, protocol readiness, request and
cancellation behavior, voice handling, and result interpretation. Shared search
validation lives in `pkg/harness/config.go`; connectivity data stays in `pkg/core`
because it crosses component boundaries. A new implementation reuses the common
runtime while supplying its native files, commands, and protocol operations.

For both OpenClaw and Hermes, Runner calls `Start` to create the runtime, then `Run` to execute
the instruction. The manager obtains a private service endpoint through
deployment operations, and a Gateway client performs the agent protocol. Shutdown uses deployment operations to confirm absence.
Changing hosting must preserve Gateway semantics, request correlation,
cancellation, credentials, artifacts, and isolation.

Current implementations are [OpenClaw and Hermes](../implementation/harnesses.md).
New harnesses must preserve these lifecycle and ownership guarantees, with tests
for partial startup, run failure, cancellation, idempotent stop, positive absence,
credentials, and artifacts. Concrete voice modes and MCP configuration do not
add Runner roles; MCP server placement has its own execution boundary described
in the implementation page.
