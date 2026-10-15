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
sandbox gets an independent listener, execution target, and evidence recorder.
The bridge remains available while other tasks finish or later tasks arrive.

| Owner | Responsibility |
| --- | --- |
| Application run | Start shared connectivity and bridge after model preflight; stop them after task cleanup; persist infrastructure outcomes. |
| Runner / ToolSandbox | Create, prepare, evaluate, and remove live and fresh evaluation sandboxes. |
| Bridge service | Own its runtime, control connection, session admission, and, for SSH, one in-memory host key. |
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
Both transports also accept clients without authentication. One in-memory
service host key satisfies SSH's handshake and is shared across listeners.
ARIES stages no SSH client keys or known-hosts files and performs no host-key
pinning. The gRPC transport is plaintext and has no client credentials. These
choices do not remove exact sandbox binding, verifier isolation, or resource ownership.

The child's [NativeServer](../../pkg/bridge/bridge.go) receives a borrowed
executor, with no sandbox lifecycle authority. The executor runs streaming
commands and gives typed file stat, read, and write in the bound runtime
([target](../../pkg/bridge/target/target.go)). Wiring supplies
native factories and optional client staging.

## Transports

A transport is the wire between a harness and the tool bridge for tool calls.
One transport serves a whole run. `bridge.type` selects it in
[child wiring](../../internal/app/wiring/bridge/child.go):
`openclaw-ssh` and `hermes-ssh` select SSH, and `hermes-grpc` selects gRPC
([`transportOf`](../../pkg/bridge/launch.go)). The run service rejects a
registered endpoint whose transport is not the run's transport, and gives the
harness an endpoint with that transport as its protocol
([session](../../pkg/bridge/session.go)).

- **SSH.** Shared SSH code owns serving, streams, cancellation and recording;
  OpenClaw and Hermes dialects own grammar, workspace translation and refusal
  policy. A dialect is the command grammar of one harness, so it exists only
  on SSH. The thin `aries-ssh-client` forwards OpenClaw's command bytes without
  interpreting server grammar. See [SSH bridges](../implementation/ssh-bridges.md).
- **gRPC.** The `aries.sandbox.v1` service has typed calls and no dialect:
  `Exec` carries a script, stdin, and a login flag, and four file procedures
  carry a path and bytes. Only Hermes uses it, through a staged
  `aries-grpc-client` and an ARIES Hermes plugin. See
  [gRPC bridge](../implementation/grpc-bridge.md).

Both transports write the same tool-call record rules to `tool-calls.jsonl`
(see [evidence](#evidence-and-substitution)), and each writes its optional raw
log as `<transport>_raw.log`: `ssh_raw.log` or `grpc_raw.log`. The security
posture is the same for both: plaintext, no client authentication, and one
shared run network. The gRPC transport adds no isolation that SSH does not have.

The typed file operations of the gRPC transport use the Docker archive API
([Docker files](../../pkg/deployment/docker/files.go)). A write uploads to a
temporary name in the target directory, and one root exec renames it into
place. The archive API acts as the Docker daemon, that is as root, not as the
sandbox's exec user. Thus a file call can reach paths that the agent's own
shell cannot. ARIES accepts this weakness for performance, because verifier
material arrives only after revocation. New files and parents get the exec
user as owner.

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

Recording occurs once at each transport's execution boundary, through one
shared [audit writer](../../pkg/bridge/internal/bridgekit/audit.go).
Each session owns `tool-calls.jsonl` and an optional `<transport>_raw.log`, with
`sandbox_id` and a local sequence. Registration metadata relates the sandbox
to its run and task.
Release finalizes an artifact manifest; collection validates it before publishing
private task evidence. Missing finalized evidence after a child crash remains an
error. See [bridge evidence](../run-results.md#bridge-evidence).

Structured calls record command semantics, status, timing and output byte counts;
they do not contain stdout/stderr bodies or read file content. Printable UTF-8
input stays inline, binary input becomes a `binary-omitted` marker, and no
record holds base64. Raw wire input may contain sensitive task data and stays
private. Model credentials do not belong in these records.
Shared runtime measurements are recorded once under run infrastructure, while
each task retains its own harness/sandbox observations.

The gRPC transport reuses service/session ownership and borrowed execution.
Endpoint handoff and a stable sandbox identity simplify such extensions, but do
not provide E2B wire compatibility. Snapshot, restore, suspend, other providers,
mixed transports in one run, and OpenClaw over gRPC are unimplemented.
Additional providers must satisfy explicit execution, staging, addressing,
cancellation and confirmed cleanup contracts;
see [current deployment limits](deployment.md#substitution-and-current-limits).
