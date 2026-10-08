# SSH bridge implementations

The managed bridges implement the [ToolBridge contract](../design/bridge.md).
They accept unauthenticated SSH clients and forward accepted commands through
the sandbox streaming capability; Docker execution is supplied by the sandbox
deployment through the Moby SDK.

The [native-serving contract](../../pkg/bridge/bridge.go) separates protocol
serving from the managed controller. One [SSH engine](../../pkg/bridge/ssh/bridge.go)
owns listeners, connections, streams, audit persistence and cleanup. The run's
shared bridge service generates one in-memory host key for the SSH handshake
and uses it across all sandbox listeners; clients need no keys or passwords.
The OpenClaw and Hermes dialects only interpret native requests and prepare
commands; neither owns sandbox lifecycle or a separate SSH server implementation.

OpenClaw's pinned container lacks an SSH binary, so ARIES stages the static Go
`aries-ssh-client` at `/opt/aries/bin/aries-ssh-client`. The
[OpenClaw client](../../pkg/bridge/ssh/openclaw/client) forwards the remote command
and streams unchanged without client authentication or host-key pinning. It validates its supported invocation
and private configuration, but does not parse shell grammar or translate paths.
The client owns OpenClaw's invocation/configuration subset and does not import
the server's command grammar. Shared SSH serving and root bridge lifecycle do not
import this harness-specific client. The server dialect is authoritative for
command acceptance. Refused commands produce server-side rejection evidence
without executing in the sandbox. Hermes uses its image's native OpenSSH.

The bridge runs the SSH server; `aries-ssh-client` is the separate client inside
the OpenClaw harness. Its argument parser recognizes the pinned invocation
`-F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND`. This exact order
is a limitation of the supported client CLI, not an SSH protocol requirement.
The parser extracts the remote command unchanged. Configuration loading checks
the endpoint and OpenClaw's generated private config; the server dialect validates the command after
receiving the SSH exec request. Unsupported client options are rejected rather
than silently ignored.

## OpenClaw SSH bridge

The OpenClaw SSH dialect adapts OpenClaw's pinned SSH
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
client command. ARIES sets no `TERMINAL_SSH_KEY` and stages no client identity.
The pinned Hermes implementation adds `-i` only when a key path is configured.
Hermes retains its native
`StrictHostKeyChecking=accept-new` and offers no way to preload a known-hosts
file. That first-use behavior works with the service's stable host key, including
reconnections. ARIES supplies no known-hosts file and keeps no host-key artifact.

Recorded against Hermes v2026.5.29.2 and re-verified unchanged against
v2026.8.31, the accepted grammar is exactly four payload shapes: the two fixed
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

The application owns one `aries-bridge` service and one shared Docker network
per run. Each Runner receives a fresh bridge session. Registering a sandbox
creates its own SSH listener, borrowed executor, audit writer, and evidence
directory inside that service. Releasing it leaves other sessions and the
service available, including when there are no active sessions.

The sandbox exports its stable `sandbox_id` and exact deployment binding. Its
borrowed adapter validates immutable runtime identity and task ownership through
the service's injected deployment backend and exposes streaming execution
without sandbox creation or deletion. Child wiring owns that backend client.
Tool traffic never calls back into Runner. Command validation, user defaults,
and workdir defaults use the same sandbox helper as direct sandbox execution.
Deployment resolves the harness-facing endpoint from the shared runtime and the
listener's actual port. The native server reports its local binding without
discovering a container IP or choosing Docker routing. Network ownership follows the
[task-environment contract](../design/deployment.md#taskenvironment-operations-and-ownership).

Control uses plaintext, unauthenticated gRPC with three operations:
`RegisterSandbox`, `GetSandbox`, and `ReleaseSandbox`. Registration supplies the
target binding and task metadata once; subsequent calls use only `sandbox_id`.
The service keeps independent registration and release state for each sandbox.
Caller timeouts do not cancel service-owned admission or cleanup. A lost
registration response is reconciled by looking up the same sandbox; only a
confirmed absent registration permits resubmission of the identical request.
Definitive target or registration errors do not trigger replay.

There are no SSH client credential files, leases, or controller-liveness checks.
Runner explicitly releases its session during task cleanup. The run owner stops
the shared service after tasks finish and removes the network after its
containers are gone. An abrupt controller crash can leave owned runtimes and
networks requiring operator cleanup.

Stop closes tool admission and bridge-owned listeners/connections/handlers,
finalizes and collects that sandbox's evidence. Service shutdown separately
confirms shared runtime removal. Harness
completion is authoritative for completed tool calls; revocation neither scans
nor kills sandbox processes. The live sandbox remains available for evaluation.
If a child exits before any harness receives access, the controller can confirm
access closure from its stopped state. If access was exposed, an exit without finalized evidence remains
an error even after runtime removal. This is an evidence failure, not a requirement
to prove sandbox processes have stopped. Remote artifact names are checked and
mapped into local bridge evidence paths; remote
absolute paths are never exposed as local files. Failed cleanup retains ownership
for retry.
Each structured and raw tool record carries `sandbox_id` and that sandbox's
local sequence number, alongside run/task and exact runtime metadata. Evidence
is collected into the task's `bridge/` directory. Shared runtime ownership is
recorded under `infrastructure/bridge/`; per-task `bridge/runtime.json` records
the session's sandbox and target binding to that shared runtime.

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
