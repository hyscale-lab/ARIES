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
  OpenClaw and Hermes dialects own grammar, workspace adaptation and refusal
  policy. A dialect is the command grammar of one harness, so it exists only
  on SSH. The thin `aries-ssh-client` forwards OpenClaw's command bytes without
  interpreting server grammar. OpenClaw's concrete server prepares its workspace
  alias before starting the shared SSH manager; the alias remains sandbox-owned
  through evaluation and disappears with sandbox removal. Native cwd-validation
  arguments are mapped to the task root, while shell bodies pass through
  unchanged. See [SSH bridges](../implementation/ssh-bridges.md).
- **gRPC.** The `aries.sandbox.v1` service has typed calls and no dialect:
  `Exec` carries a script, stdin, and a login flag, and four file procedures
  carry a path and bytes. Hermes is its harness: the ARIES Hermes plugin runs a
  staged `aries-grpc-client`, which carries the calls. See
  [gRPC bridge](../implementation/grpc-bridge.md) and
  [Hermes over gRPC](../implementation/harnesses.md#hermes-over-grpc).

Both transports write the same tool-call record rules to `tool-calls.jsonl`
(see [evidence](#evidence-and-substitution)), and each writes its optional raw
log as `<transport>_raw.log`: `ssh_raw.log` or `grpc_raw.log`. The security
posture is the same for both: plaintext, no client authentication, and one
shared run network. The gRPC transport adds no isolation that SSH does not have.

The typed file operations of the gRPC transport use the Docker archive API
([Docker files](../../pkg/deployment/docker/files.go)), or short shell scripts
over `kubectl exec` on Kubernetes
([Kubernetes files](../../pkg/deployment/kubernetes/files.go)). A write acts as
the user of the agent's shell: the sandbox's exec user, or the image `USER`
when no exec user is set. That user creates the missing parents, passes the
access check, and renames a temporary file into place. New files and parents
get that user as owner. Thus a typed write reaches only what the agent's own
shell can write, as on SSH.

Typed reads (stat and read) act as the Docker daemon, that is as root, or on
Kubernetes as the container user, which is root on every current benchmark
image. A file read can thus see files that the agent's own shell cannot read.
This weakness is lower: a read changes nothing in the sandbox, and verifier
material arrives only after revocation.

## Lifecycle, cancellation, and failure

Task execution remains: prepare sandbox → register bridge session → run harness →
confirm harness stop → release session and collect evidence → evaluate → remove
sandboxes and release logical attachments. Fresh evaluation sandboxes borrow the
same run network and have their own unique service names.

Harness completion is authoritative for completed tool calls. Revocation closes
bridge-owned access and handlers; it does not snapshot, classify, or sweep sandbox
processes. Command cancellation closes the backend execution transport and may
leave the command running, as with native Docker or Kubernetes exec.
Tool-call errors remain in command evidence and do not by themselves fail
revocation, including errors returned during cancellation. Handler-join timeouts
and evidence-finalization failures still prevent confirmed release.
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
