# ToolSandbox

`ToolSandbox` owns the task environment. It starts one isolated environment for
a task, returns the narrow live capability needed by the selected bridge and
benchmark, and stops the environment with positive confirmation that owned
resources are absent.

## Boundary and lifecycle

The sandbox begins before bridge or harness startup and stays live through
independent benchmark evaluation. It does not own harness policy, tool
credentials, or benchmark scoring. Cleanup is idempotent, reverse-order, and
bounded after cancellation; failure to confirm resource absence remains a
cleanup failure.

The current implementation uses Docker through the Moby Go SDK. ARIES owns its
containers and networks and never shells out to Docker for lifecycle
operations. A pair-specific bridge may use a narrow sandbox capability such as
streaming command execution, but the harness does not receive Docker daemon
access.

The Codex pairing uses `TaskUser` to resolve the benchmark's effective identity
and `ExecSupervisedStream` to start its trusted descendant supervisor directly.
This narrow capability preserves argv, bounds streams, checks container and
exec identity, and confirms Docker exec exit. It never runs task-owned shell
or cancellation helpers. Its caller must independently prove descendant
cleanup; closing an attach on error is not such proof, and the Runner must
block evaluation and remove the sandbox.

OpenClaw and Hermes use the concrete `StartAgentSession`, `ExecAgentStream`,
and `StopAgentSession` capabilities. Before SSH access, the sandbox starts one
protected root broker through the SDK and gives it a private nonce. Each
command runs under a child subreaper with the task UID/GID and exact argv.
Normal completion retains background descendants for later calls; cancellation
retires that command's descendants, and session Stop retires all agent work.
Worker launches use `/proc/self/exe`, so replacing the staged helper cannot
replace the trusted executable. No worker receives the nonce or passes its
private control descriptor to a task command.

Successful Stop requires all workers to be waited, a final `ECHILD`, Go-based
stage removal, a terminal nonce proof, confirmed Docker exec exit, and drained
host streams. A missing proof or ambiguous cleanup failure stays latched across
retries. Destroying a failed sandbox releases its host goroutines without
converting failure into permission to evaluate. A sandbox cannot restart its
agent session after task access. Ordinary benchmark `Exec` and `ExecStream`
retain their process-group semantics, so benchmark preparation can start its
own services outside the agent's process tree.

## Customization & Contribution Guide

A new sandbox implementation must preserve exact command argument boundaries,
context-aware external operations, bounded cancellation cleanup, and positive
absence checks. Put it in a concrete package with an explicit constructor and
command switch, then test partial startup, live evaluation, idempotent stop,
resource ownership, and bridge-facing capabilities. Update the supported
reference and operational prerequisites. Do not add registration, discovery,
factories, reflection, DI, or generic plugins.
