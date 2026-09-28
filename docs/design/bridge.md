# ToolBridge

`ToolBridge` is the additional component deep dive for temporary tool access.
It adapts one concrete harness to one live sandbox, grants only the capability
needed for that pairing, and positively revokes the grant before evaluation.

## Stable security contract

The bridge grants temporary access from one harness to the exact sandbox that
will later be evaluated. The harness is stopped first; bridge revocation then
closes listeners and sessions, drains active work and evidence, removes staged
credentials, and positively confirms access is absent. Any failure blocks
evaluation and verifier upload.

```mermaid
flowchart TB
    H[Agent Harness]
    G[Temporary ToolBridge grant]
    C[Relay Agent Harness tool execution to Sandbox]
    S[Runner positively<br/>stops harness]
    R[Revoke grant and drain<br/>active work and evidence]
    P[Positively confirm<br/>access is revoked]
    E[Evaluation may begin]

    H -->|before run| G --> C
    H -->|after run| S --> R --> P
    P -->|revocation confirmed| E
```

The current pair-specific OpenClaw SSH bridge adapts OpenClaw's pinned SSH
behavior to a narrow streaming capability of the Docker sandbox. OpenClaw never
receives the Docker socket, sandbox ownership, or verifier material. The bridge
does not create a persistent workspace alias in the evaluated environment.
Credentials, listeners, sessions, and helper processes are owned and revoked
fail-closed.

The bridge retains structured executed-command records and lossless wire-side
input as private replayable evidence. These artifacts may contain task data and
must remain private unless reviewed; model credentials and SSH private-key
bytes do not belong in the records.

OpenClaw, Hermes, and Codex share the concrete SSH transport and audit code in
`pkg/bridge/internal/sshbridge`: ephemeral keys, private files, connection and
session shutdown, stream accounting, and the bounded JSONL/raw writer. The two
ARIES SSH clients also share connection, host-key verification, cancellation,
and exit handling. Each adapter retains its own command grammar, credential
file rules, workspace mapping, and execution/revocation policy. This internal
package adds no Runner role or registration layer.

All three adapters use the static `aries-exec` helper built beside `aries`.
Its concrete Linux protection, identity, subreaper, and cleanup primitives
live in `internal/execsupervisor`. Codex retains its native executor session;
OpenClaw and Hermes establish a persistent agent broker before publishing SSH
access and send each command through the sandbox's supervised session. A
completed tool call can leave background services for later calls. Cancellation
retires that call's descendants, and bridge Stop retires the entire agent
lineage, including `setsid` and double-fork descendants. Benchmark services
started outside that lineage remain alive.

The broker carries exact command arguments and independently acknowledged
64 KiB stream chunks, with per-command input and output limits. One slow
command stream cannot block decoding other calls. On cancellation, EOF may
follow an outstanding chunk before its acknowledgement, but host cleanup still
waits for that chunk's writer. Revocation closes SSH admission, drains active
handlers, and requires the sandbox's private final cleanup proof and confirmed
Docker exit before audit and credential cleanup can complete. Unconfirmed
cleanup permanently blocks evaluation; destroying the sandbox does not clear
that failed gate.

## Hermes SSH bridge

The Hermes pairing is a second, separate adapter rather than a reuse of the
OpenClaw one, because the two harnesses put different bytes on the wire. Hermes
runs OpenSSH itself, so this bridge stages no client helper and supplies no
client command; it hands over only a generated identity. Hermes forces
`StrictHostKeyChecking=accept-new` and offers no way to preload a known-hosts
file, so it pins the generated host key on first use; the bridge retains that
key as evidence rather than implying a guarantee it cannot enforce.

Recorded against Hermes v2026.5.29.2 and re-verified unchanged against
v2026.8.3, the accepted grammar is exactly four payload shapes: the two fixed
bootstrap probes `echo 'SSH connection established'` and `echo $HOME`, and
`bash -c` / `bash -l -c` with one canonically `shlex.quote`-encoded script.
A harness may open more than one SSH session per run — v2026.8.3 opens two —
so the grammar is applied per connection and carries no run-level state. Scripts carry embedded newlines and
nested quoting, so the canonical single-token encoding OpenClaw uses does not
apply. Anything else is refused. Hermes multiplexes every command onto a single
ControlMaster connection and sends an `env` request on each channel before the
exec; the bridge refuses that request and keeps the channel open, because
closing it would drop every command.

The decoded `bash` token is resolved to the absolute `/bin/bash` before it
reaches the sandbox, which requires an absolute command path and performs no
PATH lookup of its own. The bridge is also authoritative for the working
directory: every command runs in the sandbox's own workdir regardless of what
Hermes believes its `cwd` to be.

Hermes's remaining payloads belong to its `~/.hermes` file sync — `mkdir -p`,
`tar xf -`, `tar cf -`, `rm -f` — and ARIES denies them by policy. The sync set
is built from `iter_sync_files`, which includes credential files, and the remote
is the exact container the verifier later inspects, so allowing it would both
place credentials in the evaluated sandbox and pollute it with Hermes scaffold.
Denial is safe and was verified against the real harness: Hermes catches the
failure, logs one warning, rolls its sync state back, and continues running
commands normally; because nothing was pushed, its teardown sync-back then
suppresses itself. Refusals are recorded with a distinct `denied` status so
evidence separates policy from a protocol violation.

## Codex SSH bridge

Codex `0.157.1` provides an experimental native stdio executor. The separate
`codexssh` adapter accepts one fixed SSH exec request and relays the native RPC
bytes to `codex exec-server --listen stdio` in the same task container the
benchmark evaluates. It preserves native shell and filesystem semantics;
ARIES does not translate the RPC into shell scripts. Its small static SSH
client verifies an exact ephemeral host key and accepts no forwarding, PTY,
or environment grants. Only one executor session may be claimed per run.

The bridge stages the static Codex binary and a Go descendant supervisor in an
exclusive root-owned task directory. SSH credentials and model credentials
stay outside the task container. The supervisor starts as root and launches
the native executor with the task command user and environment. Because native commands can create independent process
groups, the supervisor becomes a Linux child subreaper, disables memory
inspection, and enforces `no_new_privs` with no capability that could bypass
those restrictions. A private nonce arrives on nonseekable stdin before RPC;
it is neither logged nor passed to child argv or environment.

After executor shutdown, the supervisor kills and reaps its adopted
descendants while preserving unrelated task processes. It emits a private
terminal proof only after `ECHILD`. Revocation requires that proof, a zero
supervisor exit, confirmed Docker exec termination, drained audit records, and
removal of the staged directory by the supervisor's Go filesystem operations.
The Docker sandbox exposes a narrow supervised streaming capability that
executes the helper directly; no task-owned shell or cleanup command runs
after the proof. Disconnect, timeout, or missing proof fails
closed and prevents evaluation. SSH closure allows a bounded cleanup interval;
if it expires, sandbox removal remains the final containment step.

Private bridge records retain the native RPC input (at most 16 MiB per session)
and output counts. The combined audit budget is 256 MiB. The nonce and cleanup
proof are stripped from evidence and remote stderr. These are transport
records; Codex's JSONL trajectory carries the model's tool-call semantics.

## Lifecycle position

Bridge startup follows sandbox sanitization and precedes harness startup. On
normal completion, failure, or cancellation, the Runner positively stops the
harness, revokes the bridge, evaluates the still-running sandbox, and finally
stops the sandbox. A future harness may require a different pair-specific
adapter rather than a lowest-common-denominator remote-tool protocol.

## Customization & Contribution Guide

Build a new bridge around the selected harness's real upstream boundary and the
smallest capability exposed by its sandbox pairing. Keep authentication,
workspace mapping, command semantics, cancellation, revocation, privacy, and
evidence policy concrete. Add an explicit constructor and command switch plus
tests for partial start, active-session cancellation, positive revocation,
credential cleanup, argument preservation, private artifacts, and blocked
evaluation on stop failure. Update the supported reference. Do not add
registration, discovery, factories, reflection, DI, or generic plugins.
