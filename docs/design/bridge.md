# ToolBridge

`ToolBridge` grants temporary harness access to the exact live sandbox that the
benchmark later evaluates. It adapts native tool protocols without taking
ownership of the sandbox. The implementation builds on baseline
`ec941cbece53df3022946a8502dab3f938f4e5ca`; this page describes the current shared
service, not the topology at that earlier revision.

## Operations and ownership

Runner still composes four roles. Its [ToolBridge interface](../../pkg/runner/interfaces.go)
is a sandbox session hosted by a run-owned [Service](../../pkg/bridge/service.go).
There is one bridge container and one shared Docker network per run. Every live
sandbox gets an independent SSH listener, execution target, and evidence recorder.
The bridge remains available while other tasks finish or later tasks arrive.

| Owner | Responsibility |
| --- | --- |
| Application run | Start shared connectivity and bridge after model preflight; stop them after task cleanup; persist infrastructure outcomes. |
| Runner / ToolSandbox | Create, prepare, evaluate, and remove live and fresh evaluation sandboxes. |
| Bridge service | Own its runtime, control connection, one in-memory SSH host key, and session admission. |
| ToolBridge session | Register one exact sandbox, expose its listener, release access, and collect finalized evidence. |

`Start(ctx, sandbox)` returns a resolved `core.ToolEndpoint`. `Stop(ctx)` closes
that session's admission, connections and handlers, finalizes evidence, and
confirms revocation. It leaves other sessions, the shared runtime, and the sandbox
alive. A failed release or evidence collection prevents that task's evaluation;
cleanup remains retryable. Every attempted start is followed by Stop.

The service and sandbox deployment are separate dependencies. Wiring supplies
an explicit [launch specification](../../pkg/bridge/launch.go), provider access,
and runtime metadata. Common bridge code does not choose a provider, discover
network interfaces, or construct Docker addresses. See
[endpoint handoff](deployment.md#endpoint-handoff).

## Control and native protocols

The internal [gRPC API](../../pkg/bridge/control/v1/control.proto) has
`RegisterSandbox`, `GetSandbox`, and `ReleaseSandbox`. Each operates on one stable
`sandbox_id`, reused from the sandbox's unique runtime name. Registration supplies
the immutable provider runtime ID, workdir, execution user, and task metadata.
The run and backend are configured once for the service. Runtime IDs are internal
execution and cleanup references, not additional public lifecycle identifiers.

Independent entries use independent locks. Repeating identical registration is
idempotent; conflicting bindings fail. Released IDs cannot be reopened by a late
registration retry. If an admission response is uncertain, lookup and release
reconcile the same ID; tool commands are never replayed automatically.
The provider validates the actual runtime identity and ownership before execution.

Control is plaintext, unauthenticated gRPC in the trusted research environment.
SSH also accepts clients without authentication. One in-memory service host key
satisfies SSH's handshake and is shared across listeners. ARIES stages no SSH
client keys or known-hosts files and performs no host-key pinning. These choices
do not remove exact sandbox binding, verifier isolation, or resource ownership.

The child's [NativeServer](../../pkg/bridge/bridge.go) receives a borrowed streaming
executor, with no sandbox lifecycle authority. Shared SSH code owns serving,
streams, cancellation and recording; OpenClaw and Hermes dialects own grammar,
workspace translation and refusal policy. The thin `aries-ssh-client` forwards
OpenClaw's command bytes without interpreting server grammar. Wiring supplies
native factories and optional client staging. See [SSH bridges](../implementation/ssh-bridges.md).

## Lifecycle, cancellation, and failure

Task execution remains: prepare sandbox → register bridge session → run harness →
confirm harness stop → release session and collect evidence → evaluate → remove
sandboxes and release logical attachments. Fresh evaluation sandboxes borrow the
same run network and have their own unique service names.

Harness completion is authoritative for completed tool calls. Revocation closes
bridge-owned access and handlers; it does not snapshot, classify, or sweep sandbox
processes. Existing command cancellation retains its targeted execution semantics.
The live sandbox, including benchmark services and background work, remains
available to evaluation. There are no leases, heartbeats, renewal RPCs, or
controller-liveness watchdogs; abrupt Runner crashes may need operator cleanup.

After task scheduling and cleanup finish, the application stops service admission,
retries outstanding session cleanup, removes the bridge runtime, finalizes its
resource observation, removes the run network, and closes transports. Cleanup
uses fresh bounded contexts. Startup and teardown failures are recorded in
`RunResult.Infrastructure` before `run-result.json` is persisted, separately from
task counts. Later removal does not erase earlier evidence or cleanup failures.

## Evidence and substitution

Recording occurs once at the shared SSH execution boundary. Each session owns
`tool-calls.jsonl` and optional `ssh_raw.log`, with `sandbox_id` and a local
sequence. Registration metadata relates the sandbox to its run and task.
Release finalizes an artifact manifest; collection validates it before publishing
private task evidence. Missing finalized evidence after a child crash remains an
error. See [bridge evidence](../run-results.md#bridge-evidence).

Structured calls record command semantics, status, timing and output byte counts;
they do not contain stdout/stderr bodies. Raw wire input may contain sensitive
task data and stays private. Model credentials do not belong in these records.
Shared runtime measurements are recorded once under run infrastructure, while
each task retains its own harness/sandbox observations.

Another bridge protocol can reuse service/session ownership and borrowed execution.
Endpoint handoff and a stable sandbox identity simplify that extension, but do
not provide E2B wire compatibility. Snapshot, restore, suspend, other providers,
and mixed protocols are unimplemented. Additional providers must satisfy explicit
execution, staging, addressing, cancellation and confirmed cleanup contracts;
see [current deployment limits](deployment.md#substitution-and-current-limits).
