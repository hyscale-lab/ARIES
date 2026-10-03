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


## Source and limitations

Both bridges are embedded in the runner process. They require streaming execution,
validated task identity, and task workdir capabilities beyond the minimal
`Sandbox` interface; arbitrary sandbox implementations are not automatically
compatible. Network ownership follows the
[task-environment contract](../design/deployment.md#taskenvironment-operations-and-ownership).

See the [OpenClaw bridge](../../pkg/bridge/openclawssh/bridge.go),
[OpenClaw grammar](../../pkg/bridge/openclawssh/grammar.go),
[Hermes bridge](../../pkg/bridge/hermesssh/bridge.go), and
[Hermes grammar](../../pkg/bridge/hermesssh/grammar.go).
[OpenClaw contract tests](../../pkg/bridge/openclawssh/manager_contract_test.go)
and [Hermes bridge tests](../../pkg/bridge/hermesssh/bridge_test.go) cover accepted
commands, private evidence, revocation, cancellation, and denied sync.
The shared protocol and deployment roadmap is listed under
[planned targets](../supported.md#roadmap); it is not current bridge support.
