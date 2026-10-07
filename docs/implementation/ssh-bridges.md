# SSH bridge implementations

The managed bridges implement the [ToolBridge contract](../design/bridge.md).
They authenticate temporary SSH clients and forward accepted commands through
the sandbox streaming capability; Docker execution is supplied by the sandbox
deployment through the Moby SDK.

The [native-serving contract](../../pkg/bridge/bridge.go) separates protocol
serving from the managed controller. One [SSH engine](../../pkg/bridge/ssh/bridge.go)
owns listener authentication, connections, streams, audit persistence and cleanup.
The OpenClaw and Hermes dialects only interpret native requests and prepare
commands; neither owns sandbox lifecycle or a separate SSH server implementation.

OpenClaw's pinned container lacks an SSH binary, so ARIES stages the static Go
`aries-ssh-client` at `/opt/aries/bin/aries-ssh-client`. The
[client](../../pkg/bridge/ssh/client) verifies the assigned host key and forwards
the remote command and streams unchanged. It validates its supported invocation
and private configuration, but does not parse shell grammar or translate paths.
The server dialect is authoritative for command acceptance. Previously client-side
grammar refusals now reach the server and produce rejection evidence without
executing in the sandbox. Hermes continues using its image's native OpenSSH.

The bridge runs the SSH server; `aries-ssh-client` is the separate client inside
the OpenClaw harness. Its argument parser recognizes the pinned invocation
`-F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND`. This exact order
is a limitation of the supported client CLI, not an SSH protocol requirement.
The parser extracts the remote command unchanged. Configuration loading checks
the endpoint and private files; the server dialect validates the command after
receiving the SSH exec request. Unsupported client options are rejected rather
than silently ignored.

## OpenClaw SSH bridge

The current pair-specific OpenClaw SSH bridge adapts OpenClaw's pinned SSH
behavior to a narrow streaming capability of the Docker sandbox. It does not
create a persistent workspace alias in the evaluated environment. Structured
executed-command records and lossless wire-side input follow the
[bridge evidence contract](../design/bridge.md#evidence-and-substitution);
access ownership and revocation follow its
[lifecycle contract](../design/bridge.md#lifecycle-cancellation-and-failure).

In sandbox mode, OpenClaw's `exec` sets `HOME` to its sandbox workspace by
default (`buildSandboxEnv` in the pinned 2026.7.1 build); sandbox and per-call
environment can override that default. The bridge translates the
virtual workspace to the task workdir, but maps the generated `HOME` to `/tmp`
instead of the workdir. Otherwise per-user caches written under `HOME`, such as
Go's `~/.cache/go-build` or `~/.npm`, land in the repository being evaluated and
become part of the candidate's changes; SWE-bench Pro patch capture then
exceeds its 16 MiB bound. `/tmp` exists and is writable in task images, so the
bridge creates no state of its own. Each structured tool-call record names the
mapped value in `workspace_home`. A `HOME` the agent exports inside its
command script is unaffected.

## Hermes SSH bridge

The Hermes dialect shares the SSH engine with OpenClaw while retaining its
different command grammar and request policy. Hermes
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

Runner owns a managed bridge controller. Each occurrence gets a separate
`aries-bridge` container, and that child owns the native SSH server.
The sandbox exports a versioned fixed-target descriptor. Its borrowed adapter
opens its own deployment client, validates immutable runtime identity and task
ownership, and exposes streaming execution without sandbox creation or deletion.
Tool traffic never calls back into Runner. Command validation, user defaults,
and workdir defaults use the same sandbox helper as direct sandbox execution.
Network ownership follows the
[task-environment contract](../design/deployment.md#taskenvironment-operations-and-ownership).

The controller stages the authorized client public key and server private key;
the client private key stays local for staging into the harness. Authenticated
gRPC assignment control is separate from SSH tool traffic. One instance accepts
one assignment, retains its identity after failed admission or revocation, and
cannot be reused. Lease expiry starts revocation; renewals cannot revive an
expired grant. Caller timeouts do not cancel the service's ownership of admission
or cleanup. A lost response is reconciled using the original assignment ID.
If authenticated status confirms that assignment was never reserved, the controller
resubmits the identical request on the same instance. Authentication, identity,
validation, and other definitive errors do not trigger replay.
Historical admission and cleanup diagnostics remain available separately from
active cleanup errors, so a confirmed cleanup retry can finish successfully.
The bounded collection/exit interval starts when revocation begins, including
failed drains; exiting without confirmed drain never grants evaluation permission.
SSH bootstrap files are erased on revocation
attempts, and private control/bootstrap files are erased on every service exit,
including failed-drain timeouts. Evidence remains available for collection.

Stop requires native drain and evidence finalization, validated artifact collection,
and confirmed runtime removal. A crashed child without a drain acknowledgment
blocks evaluation even when its container has disappeared. Remote
artifact names are checked and mapped into local bridge evidence paths; remote
absolute paths are never exposed as local files. Failed cleanup retains ownership
for retry.
The controller retains the public host key as local `known_hosts` evidence for
both protocols; Hermes still uses its native first-use trust behavior.

The wire contract is [control.proto](../../pkg/bridge/control/v1/control.proto).
`make proto-tools` installs the pinned compiler/plugins and verifies the compiler
archive checksum. `make proto` invokes protoc to regenerate checked-in bindings;
never edit generated binding files manually.

See the [OpenClaw dialect](../../pkg/bridge/ssh/openclaw/dialect.go),
[OpenClaw grammar](../../pkg/bridge/ssh/openclaw/grammar.go),
[Hermes dialect](../../pkg/bridge/ssh/hermes/dialect.go), and
[Hermes grammar](../../pkg/bridge/ssh/hermes/grammar.go).
Shared engine and dialect tests cover accepted commands, private evidence,
revocation, cancellation, and denied sync.
The shared harness protocol and remaining deployment roadmap is listed under
[planned targets](../supported.md#roadmap); it is not current bridge support.
