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

`pkg/sandbox` implements task policy over the same `deployment.Deployment` capability
used by both harnesses. Command wiring selects `sandbox.deployment.backend` and
injects `pkg/deployment/docker`, which owns the Moby SDK, containers, archive
transport, and command execution. A separate `NewEnvironment` constructor
supplies a fresh Docker task-environment owner for each occurrence.
ARIES never shells out to Docker for lifecycle operations. A pair-specific bridge may use a narrow sandbox capability such as
streaming command execution, but the harness does not receive Docker daemon
access.

## Customization & Contribution Guide

A new sandbox implementation must preserve exact command argument boundaries,
context-aware external operations, bounded cancellation cleanup, and positive
absence checks. Implement deployment-specific operations once beneath both sandbox and harness,
with an explicit constructor and command switch. Test partial startup, live
evaluation, idempotent stop,
resource ownership, and bridge-facing capabilities. Update the supported
reference and operational prerequisites. Do not add registration, discovery,
factories, reflection, DI, or generic plugins.

## Shared deployment and task policy

The sandbox chooses task labels, default workdir and numeric execution identity,
and private artifact paths. The injected task environment owns network creation,
label and ownership validation, bridge address resolution, and confirmed removal.
Network names include fresh random identity, so duplicate task IDs and retries
remain isolated. Its commands retain absolute
executable paths; explicit evaluator root commands override the agent UID. The
backend confirms security settings and ownership before exposing the live task.

The common execution implementation supports streaming input/output and confirms
command process-group termination after cancellation without stopping the task
runtime. The bridge can therefore revoke tool access before evaluation uses that
same environment. Cleanup failures continue to block the isolation gate.

Archive transport returns source metadata so the sandbox can reject oversized or
non-regular downloads before reading payloads. It confines host destinations to
the run output root, checks archive sizes, and publishes private files atomically.
Missing-source errors require evidence that the deployment still exists.

## Task attachment and bridge addresses

The task environment returns the network attachment at startup. The sandbox
passes that attachment to deployment requests and the Runner passes it through
`HarnessRequest.Network` separately from bridge credentials. Network creation,
validation, gateway discovery, and removal are Docker-specific operations;
they are absent from `deployment.Deployment`.

Composition injects `ResolveListen` into the bridge. The Docker task environment
returns its validated gateway as both the local bind host and the advertised
host. The bridge does not discover network topology through its sandbox tool
capability. Listener ports remain allocated by the OS, and each bridge grants
access only to its assigned sandbox.

Cleanup confirms harness absence and bridge revocation before evaluation,
then removes the sandbox container before its owned network. Partial startup
also cleans the occurrence's resources with fresh bounded contexts. A failure
to confirm either removal remains a cleanup error; transport closure alone
cannot prove resource absence.
