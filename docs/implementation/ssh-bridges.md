# SSH bridge implementations

The embedded bridges implement the [ToolBridge contract](../design/bridge.md).
They authenticate temporary SSH clients and forward accepted commands through
the sandbox streaming capability; Docker execution is supplied by the sandbox
deployment through the Moby SDK.

## OpenClaw SSH bridge

The current pair-specific OpenClaw SSH bridge adapts OpenClaw's pinned SSH
behavior to a narrow streaming capability of the Docker sandbox. It does not
create a persistent workspace alias in the evaluated environment. Structured
executed-command records and lossless wire-side input follow the
[bridge evidence contract](../design/bridge.md#evidence-and-substitution);
access ownership and revocation follow its
[lifecycle contract](../design/bridge.md#lifecycle-cancellation-and-failure).

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
PATH lookup of its own. Every command starts in the sandbox's own workdir, but
that alone does not decide where it runs: Hermes's script first runs
`builtin cd -- <cwd> || exit 126`, with the call's `workdir` or else its
configured directory. The bridge therefore reports the sandbox workdir on its
endpoint (`ToolEndpoint.Workdir`), and the Hermes harness gives Hermes that
directory as its terminal `cwd` (see [harness implementation](harnesses.md#hermes)), so both name the same place.

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
the native executor with the task command user and environment. Because native
commands can create independent process groups, the supervisor becomes a Linux
child subreaper, disables memory inspection, and enforces `no_new_privs` with no
capability that could bypass those restrictions. A private nonce arrives on
nonseekable stdin before RPC; it is neither logged nor passed to child argv or
environment.

After executor shutdown, the supervisor kills and reaps its adopted
descendants while preserving unrelated task processes. It emits a private
terminal proof only after `ECHILD`. Revocation requires that proof, a zero
supervisor exit, confirmed exec termination, drained audit records, and
removal of the staged directory by the supervisor's Go filesystem operations.
The sandbox's narrow `ExecSupervisedStream` capability runs the supervisor
directly after the Docker deployment revalidates the exact task container; no
task-owned shell or cleanup command runs after the proof. Disconnect, timeout,
or missing proof fails closed and prevents evaluation. SSH closure allows a
bounded cleanup interval; if it expires, sandbox removal remains the final
containment step.

Private bridge records retain the native RPC input (at most 16 MiB per session)
and output counts. The combined audit budget is 256 MiB. The nonce and cleanup
proof are stripped from evidence and remote stderr. These are transport
records; Codex's JSONL trajectory carries the model's tool-call semantics.

## Shared transport

The three adapters share concrete SSH connection and session handling,
ephemeral keys, private files, stream accounting, and bounded audit persistence
in `pkg/bridge/internal/sshbridge`; the two ARIES SSH clients share connection,
host-key verification, cancellation, and exit handling. Each adapter keeps its
own command grammar, credential-file rules, workspace mapping, and
execution/revocation policy. The package adds no Runner role or registration.

OpenClaw and Hermes execution currently confirms cleanup of the original process
group only: `setsid` and double-fork descendants can survive a successful Stop.
This is a known revocation gap, not a guarantee of the shared transport. A fix
must keep background services usable across calls, reap every agent descendant
at Stop, and preserve unrelated benchmark services. Codex's descendant proof is
independent of that gap.

## Source and limitations

All bridges are embedded in the runner process. They require streaming execution,
validated task identity, and task workdir capabilities beyond the minimal
`Sandbox` interface; arbitrary sandbox implementations are not automatically
compatible. Network ownership follows the
[task-environment contract](../design/deployment.md#taskenvironment-operations-and-ownership).

See the [OpenClaw bridge](../../pkg/bridge/openclawssh/bridge.go),
[OpenClaw grammar](../../pkg/bridge/openclawssh/grammar.go),
[Hermes bridge](../../pkg/bridge/hermesssh/bridge.go),
[Hermes grammar](../../pkg/bridge/hermesssh/grammar.go),
[Codex bridge](../../pkg/bridge/codexssh/bridge.go), and
[Codex supervisor](../../pkg/bridge/codexssh/supervisor_linux.go).
[OpenClaw contract tests](../../pkg/bridge/openclawssh/manager_contract_test.go)
and [Hermes bridge tests](../../pkg/bridge/hermesssh/bridge_test.go) cover accepted
commands, private evidence, revocation, cancellation, and denied sync.
The shared protocol and deployment roadmap is listed under
[planned targets](../supported.md#roadmap); it is not current bridge support.
