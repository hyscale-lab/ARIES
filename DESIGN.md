# ARIES Design

Status: **MVP implemented and verified through M7.** The RALPLAN-DR Architect
and sequential Critic approved the design without blockers. M0 then confirmed
the pinned runtime assumptions below; later changes must return to planning if
one of these locked contracts changes.

## Goal and workspace boundaries

ARIES is a small Go benchmark runner. Its first path runs the real pinned
Terminal-Bench 2 `fix-git` task with an unmodified upstream OpenClaw container,
a remote OpenAI-compatible model, one local Docker sandbox, and an independent
verifier.

- `invitro` is read-only structural and style evidence for a direct `cmd` plus
  `pkg` Go layout.
- `agent_bench` is read-only workflow archaeology. No Python architecture or
  source is migrated.
- No file was written or changed outside `aries`. Before this boundary was
  locked, two early read-only `git status` checks ran once in `invitro` and
  once in `agent_bench`; they changed nothing. From M0 onward, all Git
  operations run in `aries`.
- `.omx/`, `.cache/`, `runs/`, generated credentials and config under
  `.aries/`, and `DEEPSEEK_API.key` are ignored and untracked.
- `AGENTS.md` and `PROMPT.md` are deliberately reviewed for secrets and tracked
  in M0 as the workspace contract and originating requirements, not silently
  ignored or left untracked.

The module is `github.com/hyscale-lab/aries`. The MVP has no Kubernetes,
plugins, registration, reflection, DI framework, generic factories, config
inheritance, future stubs, or Harbor runtime dependency.

## Pinned first path

| Component | Pin |
| --- | --- |
| Terminal-Bench 2 | commit `2fd12b88aafdd04a52c298e3940bcb189f9766d6` |
| `fix-git` task image | `alexgshaw/fix-git:20251031@sha256:61e431c00c58df652287aadce5457634d9f9330cfdd153ebdf2802df0d540119` |
| OpenClaw source | tag `v2026.5.26`, commit `10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7` |
| OpenClaw image | `ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3` |
| Initial live model | `https://api.deepseek.com`, `deepseek-v4-flash` |

The deterministic completion proof uses the pinned real task, image, private
verifier, and expected reward `1`. The optional DeepSeek run never substitutes
for that proof.

## Durable upstream evidence and compatibility lock

These commit-permalinked primary sources are the M0 evidence record:

- TB2 task and verifier:
  [task.toml](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/task.toml),
  [instruction](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/instruction.md), and
  [tests](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/tests/test.sh).
- OpenClaw SSH schema:
  [types.sandbox.ts lines 102-124](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/config/types.sandbox.ts#L102-L124).
- OpenClaw SSH config and argv:
  [ssh.ts lines 479-579](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh.ts#L479-L579).
- OpenClaw exec wrapping and workspace semantics:
  [ssh.ts lines 265-293](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh.ts#L265-L293) and
  [ssh-backend.ts lines 114-177 and 284-305](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh-backend.ts#L114-L177).
- Published image tools and readiness:
  [Dockerfile packages lines 159-171](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/Dockerfile#L159-L171) and
  [runtime and readiness lines 303-325](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/Dockerfile#L303-L325).
- One-turn run API:
  [register.agent.ts lines 62-139](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/cli/program/register.agent.ts#L62-L139).

M0 executed the pinned image and confirmed its digest, non-root user,
`/readyz`, `node openclaw.mjs agent --message ... --json`, and absence of a
system `ssh`. Any later mismatch blocks implementation and returns to
RALPLAN-DR.

### M0 executable compatibility evidence

The 2026-07-21 M0 probes used the exact digest-qualified image references above
and left no probe container behind.

- Docker reported the OpenClaw OCI index digest exactly as pinned on
  `linux/amd64`, with user `node`, entrypoint `tini -s --`, and default gateway
  command. Inside the image OpenClaw reported version `2026.5.26`; `id` reported
  UID/GID 1000; `ssh` and `scp` were absent; `tar` and Python 3 were present.
- The shipped `agent --help` accepted `--agent`, `--session-key`, `--message`,
  `--json`, and `--timeout`. A temporary gateway started with the upstream
  diagnostic `--allow-unconfigured` flag and a probe-only token; an in-container
  request to `http://127.0.0.1:18789/readyz` returned HTTP 200 with
  `"ready": true`. Production runs render an explicit config and do not rely on
  that diagnostic flag.
- Inspection of the shipped distribution confirmed the exact non-TTY argv
  suffix `-F CONFIG -T -o RequestTTY=no HOST REMOTE_COMMAND`, the strict-host
  config directives, canonical single-quote escaping, direct construction of
  `/bin/sh -c` or `env ... /bin/sh -c` requests, and the runtime path formula.
  Applying that shipped formula to scope key `shared` produced
  `openclaw-ssh-shared-8198076c`; its workspace is
  `<workspaceRoot>/<runtimeID>/workspace`.
- Docker reported the `fix-git` image digest exactly as pinned on
  `linux/amd64`. Both its image metadata and a runtime probe reported
  `/app/personal-site`; `git rev-parse --show-toplevel` returned that same path.

The commit-pinned primary-source permalinks remain the human-readable evidence
index. The executable probes deliberately lock only the published artifacts
and shipped behavior needed by ARIES; they do not claim compatibility with
other OpenClaw or Terminal-Bench revisions.

The locked OpenClaw sandbox fragment is:

```json5
agents: {
  defaults: {
    sandbox: {
      mode: "all",
      scope: "shared",
      backend: "ssh",
      workspaceAccess: "none",
      ssh: {
        target: "aries@task-sandbox:2222",
        command: "/opt/aries/bin/aries-ssh",
        workspaceRoot: "/aries/openclaw",
        strictHostKeyChecking: true,
        updateHostKeys: false,
        identityFile: "/run/aries/ssh/id_ed25519",
        knownHostsFile: "/run/aries/ssh/known_hosts"
      }
    }
  }
}
```

No inline identity or known-host data is rendered. The bridge pre-creates the
pinned upstream shared runtime path and aliases its computed `workspace`
directory to `Task.Environment.Workdir`. OpenClaw therefore edits the same live
filesystem later inspected by the evaluator without clearing or copying it.
The harness uses a fixed task session key, and M0 locks the pinned runtime-ID
formula from `ssh-backend.ts`. A path mismatch blocks implementation.

The helper client accepts exactly this non-TTY argv shape:

```text
/opt/aries/bin/aries-ssh -F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND
```

It rejects reordered, repeated, unknown, TTY, forwarding, subsystem, proxy,
agent, X11, and extra destination arguments. It parses only the directives
emitted by the pinned OpenClaw source: `Host`, `HostName`, `Port`, `BatchMode`,
`ConnectTimeout`, `ServerAliveInterval`, `ServerAliveCountMax`,
`StrictHostKeyChecking`, `UpdateHostKeys`, `User`, `UserKnownHostsFile`,
`IdentityFile`, and `IdentitiesOnly`, with the locked safe values above.

The server accepts only an SSH `exec` request from the per-task public key. The
request payload must decode under the pinned canonical single-quote grammar to:

```text
request := q("/bin/sh") q("-c") q(script) q(arg)*
request := q("env") q(NAME=VALUE)* q("/bin/sh") q("-c") q(script) q(arg)*
q(value) := single-quoted value using only the canonical embedded-quote escape
```

The server decodes tokens and invokes the decoded argv directly with a bounded
context; it never passes the raw SSH request to a permissive outer shell. The
inner `/bin/sh -c script` is the intentional OpenClaw tool execution inside the
task sandbox. NULs, malformed quoting, and environment names outside `PATH`,
`HOME`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TERM`, `TMPDIR`, and `TZ` are rejected,
except for the reserved exact assignment `OPENCLAW_SHELL=exec`. When present,
that assignment must be final and unique. Noncanonical
encodings, other command heads, and all non-exec SSH requests are rejected.
Password and keyboard-interactive authentication, PTY, forwarding,
agent and X11 forwarding, subsystems, and environment requests are disabled.
The listener is reachable only on the task-scoped Docker network.

## Architecture

The Runner substitutes exactly four main interfaces, defined in `pkg/runner`:

```go
type Benchmark interface {
    Tasks(context.Context) ([]core.Task, error)
    Evaluate(context.Context, core.Task, Sandbox) (core.Evaluation, error)
}

type AgentHarness interface {
    Start(context.Context, core.HarnessRequest) error
    Run(context.Context, string) (core.HarnessResult, error)
    Stop(context.Context) error
}

type ToolSandbox interface {
    Start(context.Context, core.SandboxRequest) (Sandbox, error)
}

type ToolBridge interface {
    Start(context.Context, Sandbox) (core.ToolEndpoint, error)
    Stop(context.Context) error
}
```

`SandboxRequest` adds the Runner's stable run ID and task ID to the benchmark's
environment without putting runner identity into `Environment`. `RunResult`
records the same run ID. `Sandbox` is the returned live capability, not a fifth component role. Helpers
stay concrete and package-private. Each component rolls back resources acquired
by a failed `Start` before returning. Cleanup uses a bounded context derived
with `context.WithoutCancel`, not the cancelled run context, and joins primary
and cleanup errors with the primary failure still identifiable.

Responsibilities remain direct: `core` owns data; `runner` owns the four
interfaces and lifecycle; Terminal-Bench owns discovery and private evaluation;
Docker owns Docker; the bridge owns SSH; OpenClaw owns harness config and run;
`cmd/aries` owns explicit construction. Benchmark and harness do not import one
another.

Monitoring is a concrete observer, not a fifth Runner role. M6 ran with no
observer and recorded `not_enabled`. In M7 the composition root starts
`monitor.Recorder` before `Runner.Run` and stops it after Runner cleanup with a
fresh bounded context derived from `context.WithoutCancel`. The recorder uses
only Docker Engine `GET` requests: a container list filtered by exact
`aries.managed=true` and `aries.run`, followed by one-shot stats for positively
revalidated `task-container` and `openclaw-harness` records. Production sampling
is once per second. Each task gets bounded private `resources.jsonl` samples and
an `index.json` summary. The monitor never controls lifecycle or scoring;
partial Start is rolled back, teardown disappearance is normal, and any Start,
sampling, or Stop failure remains a separate observer result while the Runner
and evaluator continue.

### M1 implementation refinements

M1 keeps `Sandbox` as the capability returned by `ToolSandbox`, so the Runner
still substitutes exactly four roles. `ToolEndpoint` carries protocol,
address, and credential-file paths but never credential bytes. The experiment
loader has one visible default, `output_dir = "runs"`, rejects unknown fields
at every struct level, and rejects trailing JSON values. Known component types
remain explicit switches in `cmd/aries`; the CLI fails clearly until their
concrete milestone packages exist.

The generic interfaces deliberately use a nil `Stop` error as the component's
positive termination or revocation confirmation. A concrete M3-M5 `Stop` may
return nil only after the stronger Docker and SSH checks locked above. If a
gate fails, the Runner records `blocked_isolation` and never calls `Evaluate`,
even when a later cleanup retry succeeds. `Start` failures are
self-rolled back by the component and receive no Runner `Stop` call.

Isolation calls and post-evaluation cleanup each receive a fresh bounded
context derived with `context.WithoutCancel`; evaluator runtime therefore does
not consume the sandbox cleanup budget. Stops within one phase share its
deadline, bounding the phase as a whole. `errors.Join` retains harness,
isolation, evaluation, and cleanup causes while the result keeps those outcomes
separate. The M1 observer result is explicitly `not_enabled`; monitoring still
does not participate in Runner lifecycle control. M1 proves that the Runner
retains no lifecycle state across repeated `Run` calls. It does not claim that
generic interfaces make `Stop` implementations race-safe or idempotent; those
are concrete, integration-tested obligations for the Docker sandbox, SSH
bridge, and OpenClaw harness in M3-M5.

### M2 Terminal-Bench adapter

`pkg/benchmark/terminalbench` supports only `fix-git` at the pinned TB2 commit.
The setup command creates a shallow detached checkout under the ignored
`.cache/terminal-bench-2` directory, verifies `HEAD` exactly, is idempotent for
the correct revision, and refuses to alter an existing wrong revision. The
runtime adapter repeats that revision check before discovery.

The adapter strictly decodes the real task TOML, rejects unknown keys and the
execution-critical capabilities not implemented by the MVP, verifies the final
Dockerfile workdir, and translates the declared image tag to the locked digest.
Only the stable ID, instruction, and generic environment leave the package.
Verifier paths, files, timeout, environment, SHA-256 digests, and stable file
metadata stay in a private table keyed by task ID; solution content is never
read or exposed. Evaluation reverifies the clean dataset revision and rejects a
verifier file that became a symlink, nonregular file, replacement, or mutation.

Evaluation uses only the live `runner.Sandbox` capability. It first removes the
fixed sandbox-owned `/tests` and `/logs/verifier` paths, recreates clean
directories, then uploads only the two privately enumerated regular files to
exact destinations. It runs the pinned `/tests/test.sh` command with its own
timeout and environment and downloads fresh CTRF and authoritative reward
alongside captured stdout and stderr. Host artifact paths are cleared before
collection, so stale or symlink-preseeded sandbox state cannot supply a result.
Reward `1` is success, reward `0` is a valid failed evaluation, and missing or
malformed reward is an evaluator error. Process exit alone is never treated as
the score.
The M2 tests use fakes, so unit checks require neither Docker nor a model API;
the integration-tagged checkout test reads the real ignored pin when present.

Go's standard library has no TOML decoder. M2 therefore added
`github.com/BurntSushi/toml` v1.4.0 (MIT) for strict typed decoding;
there is no Harbor runtime dependency.

### M3 local Docker sandbox

`pkg/sandbox/docker` is the sole owner of Docker behavior. Lifecycle and copy
operations use the Docker CLI through `os/exec` argument slices; exec uses a
small standard-library client for only the required Docker Engine Unix-socket
endpoints. There is no host shell, Docker SDK dependency, registration, or
task-writable control file. `Manager.Start` requires an immutable SHA-256 image
reference and a validated `SandboxRequest`, creates collision-resistant
task-data-free names, and applies exact `aries.run` and `aries.task` labels to
both the network and container. It positively reinspects those labels,
attachment, workdir, and running state before returning. A network is internal
when the task disallows external networking. The container receives the
declared sorted environment and resource flags and runs direct
`/bin/sleep infinity` under Docker's small init process. `BuildDir` is rejected;
unsupported storage drivers or GPU runtimes fail rather than being dropped.

`make build` also produces a small static `aries-exec-helper` beside the ARIES
binary. Start opens that executable without following symlinks, copies it into
private host staging while checking stable file identity, and mounts the staged
file read-only at `/opt/aries/bin/aries-exec-helper`. A short private runtime
directory under the host temporary directory is mounted read-only at
`/run/aries`; it contains only per-command Unix sockets and is removed and
positively confirmed absent during rollback or Stop. Start reinspects both
mount destinations and rejects a writable or missing mount.

Exec calls are serialized per sandbox. For each command ARIES listens on a
private socket, asks the Engine to start the mounted helper directly as a
detached non-TTY exec, and uses `ExecInspect` only to obtain the daemon-issued
exec ID and PID. The host accepts a peer only when Linux `SO_PEERCRED` reports
that exact PID; wrong peers are closed without receiving input. The helper
connects before spawning, executes the supplied argv directly, captures stdin
and separate output streams, waits, and returns a small framed result. The
socket descriptor is close-on-exec. Input and combined output are each capped
at 16 MiB; malformed or oversized traffic fails closed. A nonzero command exit
is a result. After receiving it, ARIES uses Docker `top` to confirm the helper
PID is absent rather than treating the observed stale `ExecInspect.Running`
field as a completion oracle. Task commands run as root for the pinned
Terminal-Bench path; user selection is not an MVP environment field.
Every return path explicitly closes any helper connection and listener, then
attempts to unlink the per-exec socket and confirms its absence with `Lstat`.
Cleanup failures are joined with the functional error and remain post-launch
failures, so the sandbox performs the same fail-closed restart.

Every failure after exec launch, including cancellation, is a terminal tool
failure and runs the same fail-closed recovery: a bounded
`context.WithoutCancel` cleanup stops and restarts the same task container,
then positively reinspects running state, identity, workdir, mounts, and
network. This preserves the writable filesystem but deliberately kills and
invalidates any M4 bridge server; the bridge's later `Stop` must tolerate that
server already being dead. A failed restart is joined with the original tool
failure.

Upload opens a regular source with `O_NOFOLLOW`, copies from that already-open
descriptor into a private stage, and checks stable identity, size, mode,
timestamps, and bytes copied before Docker sees the stage path. Download copies
into a private stage, validates a regular no-follow descriptor, and publishes a
new file beneath the output root by walking directory descriptors with
`openat`, `O_NOFOLLOW`, and exclusive creation. Hostile path replacement or
concurrent source mutation fails closed. Stop serializes concurrent callers,
captures separate stdout and stderr logs under `output_dir/sandboxes`, stops
and force-removes the container, removes its network and private socket
runtime, and confirms all three absent. A failed partial Start uses bounded
non-cancelled rollback. A failed Stop keeps ownership for a later retry;
completed Stop is idempotent.

The integration test uses the official pinned BusyBox 1.37.0 musl OCI index
`sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94`.
It exercises the real daemon, exact identity labels, resource inspection,
workdir, environment, stdin, separate streams and nonzero status, byte-exact
transfers, cancellation-driven same-container restart with filesystem
preservation and process removal, post-restart exec, private logs, concurrent
Stop, and empty labeled container/network and private-runtime inventories.
Local Unix-socket tests cover the framed protocol, direct helper execution,
exact peer-PID authentication, hostile wrong-PID rejection, Engine failures,
output bounds, cancellation, and positive helper-PID absence. The real
integration test fails on daemon or cleanup errors; it does not silently skip
the claim-bearing M3 path.

### M4 OpenClaw SSH bridge

`pkg/bridge/openclawssh` is the concrete OpenClaw-plus-local-Docker adapter. It
type-asserts only the Docker metadata and lifecycle evidence it needs; no new
Runner interface or benchmark field was added. `cmd/aries-ssh` implements the
exact pinned non-TTY argv, and `cmd/aries-ssh-server` implements workspace
preparation plus the restricted SSH server. Both helpers are static binaries
built explicitly by the Makefile. The composition root constructs the bridge
and harness through their existing explicit type switches, then executes the
full M6 Runner lifecycle, including independent benchmark evaluation after
positive harness and bridge isolation.

The client accepts exactly `-F CONFIG -T -o RequestTTY=no
openclaw-sandbox REMOTE_COMMAND`. Its config must be the current user's
mode-0600 `config` beneath a direct mode-0700
`/tmp/openclaw/openclaw-sandbox-ssh-*` directory, or OpenClaw's UID fallback
root `/tmp/openclaw-UID/openclaw-sandbox-ssh-*`, and must contain the pinned
directives in their exact upstream order and safe values. Identity and
known-hosts files are current-user mode 0600. The client
uses one Ed25519 identity, one exact `[task-sandbox]:2222` Ed25519 host key,
strict verification, a five-second connect deadline, and the pinned keepalive
interval/count. It carries stdin, separate stdout/stderr, and the remote exit
status without interpreting the command.

The staged client is mode 0555 so pinned OpenClaw's default `node` user
(UID/GID 1000) can execute its read-only bind mount. Credential source files
remain host-private mode 0600 and are not bind-mounted into the harness. The
M5 harness initializes a private Docker volume as root, copies config,
identity, known-host, and task-local secret data into it, sets its directory to
UID 1000 mode 0700 and files to UID 1000 mode 0600, then mounts the volume
read-only. The M4 real integration test exercises the credential half of this
contract with the pinned unmodified OpenClaw image and no user override.

The server receives no key path or credential bytes in argv, environment, or
task-writable files. Start uploads the static server, prepares the locked
workspace, opens a mode-0600 Unix control socket in the Docker adapter's
private read-only runtime mount, and passes only a random 32-byte token through
trusted exec stdin. The detached server must return that token and a Linux
`SO_PEERCRED` PID before the host sends its ephemeral host private key and the
single authorized client public key over the already-authenticated socket.
Docker must positively report that exact PID still present immediately before
the key frame is written; a lost or nonzero spawn response is conservatively
treated as a possible live server and requires isolation restart.
Keys remain in memory; the client identity and known-host entry live only in a
private host artifact directory. The TCP listener binds port 2222 only on the
task-scoped Docker network.

Only public-key authentication for user `aries`, one `session` channel, and one
`exec` request per connection are accepted. The server caps concurrent
connections at eight, applies a five-second pre-authentication handshake
deadline, and bounds rejected global requests at 96, which exceeds the pinned
20-minute command timeout's 15-second keepalive requirement. Saturated and
over-limit connections are closed. Password, keyboard-interactive, PTY, env, shell,
subsystem, signal, agent/X11, direct/forwarded TCP, global forwarding, and all
other requests are rejected. The exec decoder accepts only OpenClaw's unique
canonical single-quote encoding of optional allowlisted `env NAME=VALUE`
assignments followed by `/bin/sh -c script args...`; the exact reserved
`OPENCLAW_SHELL=exec` assignment is allowed only once and last. Malformed,
duplicate, other reserved, secret-bearing, NUL-containing, or noncanonical
input fails before a process starts. The decoded shell argv is started
directly. The server owns
the child stdin pipe so a finished command cannot deadlock on an SSH input
copier, kills the process group on timeout, and preserves byte streams and exit
status.

Workspace preparation is generic to `Sandbox.Workdir()`. It requires a real
directory and an absent, disjoint workspace root. When the filesystem permits,
it moves that directory to
`/aries/openclaw/openclaw-ssh-shared-8198076c/workspace` and creates an
absolute symlink at the original path. Docker OverlayFS reports `EXDEV` for the
pinned task workdir because it originates in a lower layer; in that exact case
the bridge leaves the real workdir in place and creates the reverse absolute
workspace symlink to it. Start and normal Stop accept only these two proved
shapes and require both paths to resolve to the same inode. Evaluation
therefore sees the same writable directory rather than a copy. Failed partial
Start restores the move-based shape or removes the reverse alias. Normal Stop
retains the proved alias and re-verifies identity through the read-only M3 exec
helper. The same trusted helper removes and confirms absence of the uploaded
server without trusting a task-modifiable `rm`, `test`, or uploaded executable.
Existing ancestors must be real directories rather than symlinks, and resolved
workdir/root paths are proved disjoint before creation and after preparation.
Prepare creates and validates the workspace-root parent chain separately, then
acquires the root leaf with one `mkdir`; `EEXIST` is always foreign and is never
adopted. Each attempt has a random 32-byte host token supplied to prepare only
on stdin, never in argv, environment, endpoint data, logs, or results. Before
workspace preparation, it writes that exact token to a fixed no-follow,
exclusive, mode-0600 marker inside the newly acquired root. Recovery receives
the token on stdin and validates the marker plus the complete allowed root,
runtime, workspace, and workdir shape before its first mutation. A missing or
mismatched marker, pre-existing root, symlink, or foreign entry fails closed.

For the pinned `/aries/openclaw` root, prepare may create the otherwise absent
`/aries` parent. That parent is container-scoped, contains no credential or
task data, and is destroyed by `Sandbox.Stop`. Failed-Start recovery
intentionally does not remove it: the per-attempt proof owns only the atomic
`openclaw` leaf, while an empty `/aries` could have pre-existed and must not be
claimed without separate evidence.

This proof covers uncertain transport during bridge preparation, before the
agent harness starts. The token is deliberately absent from process arguments
and environment, but prepare necessarily writes it into the task sandbox; the
pre-harness sandbox image and Docker daemon are therefore trusted at this
boundary. After a successful Start the harness may be privileged inside its
own sandbox, so normal bridge Stop never invokes destructive workspace
recovery: it only verifies and preserves the alias for evaluation. The task
container's later removal disposes of the marker and workspace root.

Stop closes the authenticated control connection and always performs the M3
fail-closed restart, even after a graceful server exit, so every SSH-launched
background descendant is killed before evaluation. Under one fresh bounded
`context.WithoutCancel` cleanup context it reinspects the restarted sandbox,
obtains its current IP, proves the exact prior server PID absent, confirms the
new listener absent, and probes with the retained in-memory signer/host key to
prove rejection. It then verifies workspace identity, removes the server,
deletes the control socket and credential directory, and positively checks
every absence. Stop is concurrent-safe, retryable after failed evidence, and
idempotent after success; uncertain-spawn rollback uses the same restart rule.

Go's standard library has no SSH implementation. The repository minimum is Go
1.25.12. `golang.org/x/crypto` remains pinned at v0.54.0 (BSD-3-Clause), with
only its required `golang.org/x/sys` v0.47.0 indirect module in `go.mod`. This
avoids
the SSH flaws fixed in v0.52.0, including
[GO-2026-5013](https://pkg.go.dev/vuln/GO-2026-5013),
[GO-2026-5016](https://pkg.go.dev/vuln/GO-2026-5016), and
[GO-2026-5017](https://pkg.go.dev/vuln/GO-2026-5017), rather than pinning an
older Go-1.22-compatible but knowingly vulnerable release.

A pinned one-shot `govulncheck` v1.6.0 source scan reports zero reachable or
imported-package vulnerabilities. It reports one module-only advisory,
[GO-2026-5932](https://pkg.go.dev/vuln/GO-2026-5932), for the unmaintained
`golang.org/x/crypto/openpgp` package. ARIES imports only `x/crypto/ssh`, not
`openpgp`; the scanner confirms there is no package or symbol path to that
advisory. The scan runs under the module-selected Go 1.25.12 toolchain without
changing the module graph.

Unit tests use an in-memory full-duplex connection because the managed unit
sandbox forbids socket syscalls; production still uses Unix/TCP sockets. The
real integration test uses the digest-pinned BusyBox task fixture and exact
pinned OpenClaw image, starts the M3 sandbox, and runs detached client
containers as OpenClaw's default UID 1000. It proves the initialized private
volume modes, exact static argv, stdin/stdout/stderr/nonzero status, tar, strict
host key, wrong key/password denial, cross-network denial, shared inode/bytes,
actual old-signer rejection after source/volume deletion, gated background
descendant termination, and empty container/network/volume inventories. Detached commands write a private
status artifact because this local Docker daemon can retain stale running
state after its PID exits; no stale daemon state is accepted as completion.

### M5 pinned OpenClaw harness

`pkg/harness/openclaw` owns the pinned upstream container, its task-local
configuration, one agent turn, and harness artifacts. `New` accepts only the
digest-qualified M0 image. Each Start has a random attempt nonce on both
volumes, the root initializer, and the harness. ARIES retains Docker container
IDs and a tentative name record before issuing every create, then reinspects
names through a bounded proof budget after every successful or ambiguous
result. It acquires cleanup ownership only when the immutable ID and exact
nonce, task, kind, milestone, and managed labels match. Volume names receive
the same proof. A transient not-found result does not clear tentative state;
only absence at the end of the bounded proof window may do so. If inspection
remains unavailable, failed-Start rollback and a
later `Stop` retain and reinspect its immutable ID when available, falling back
to its name only when that ID is absent; only exact attempt
labels promote it to destructive cleanup ownership. A foreign name collision
is never started, stopped, killed, or removed. Start
uses a short-lived root initializer to copy the rendered config, SSH material,
model credential, separate random gateway token, launcher, and run wrapper,
then changes ownership to UID/GID 1000 with mode-0700 directories and
mode-0600 private files. The final upstream container runs as its default
`node` user, has exactly the task network, read-only config and SSH-client
mounts, writable OpenClaw state, no task bind mount, and no Docker socket.
ARIES preserves the image's upstream `tini -s --` entrypoint and supplies the
private launcher plus pinned gateway argv as its command. Inspection rejects
any different image, user, command, label, network, mount, or model/gateway
secret in Docker `Config.Env`.

The rendered JSON names one `openai-completions` provider, selects the requested
model explicitly, and configures the locked shared SSH sandbox. It contains
only `${API_KEY_ENV}` and `${OPENCLAW_GATEWAY_TOKEN}` references. A private
launcher reads the two mode-0600 files, exports them only inside the OpenClaw
process, clears its shell variables, and execs the supplied argv. The gateway
token is random per harness and distinct from the model credential. Start
launches the exact upstream gateway command, probes `127.0.0.1:18789/readyz`
from inside the container, requires HTTP 200, `ready:true`, UID 1000, and the
exact OS-visible `openclaw` process title before returning.

Run accepts one non-empty instruction exactly once. It executes the exact
`node openclaw.mjs agent --session-key agent:main:aries-TASK --message
INSTRUCTION --json --timeout SECONDS` argv through the same private launcher.
A request for the exact official DeepSeek URL with model `deepseek-v4-flash`
or `deepseek-v4-pro` additionally appends request-scoped `--thinking off`;
deterministic and other endpoints retain the locked argv. Harness resources
also carry the Runner's exact `aries.run` label, so the observer can identify
them without coupling to OpenClaw internals.
A small wrapper atomically publishes separate stdout, stderr, and numeric exit
status only after the child finishes. ARIES requires all three files, exit zero,
top-level OpenClaw status `ok`, and at least one final payload; an embedded
fallback marker is a harness failure. Cancellation kills the harness before
returning, and Runner still invokes normal Stop for positive isolation.

Stop is concurrent-safe, idempotent after success, and retryable after partial
failure. It first verifies the retained ID and nonce, gives upstream `tini` a
bounded graceful stop, and uses KILL only when graceful termination or process
proof fails. It then positively proves no process remains, captures bounded
and secret-redacted gateway logs and available upstream session telemetry, and
revalidates ownership immediately before removing the container and volumes.
Every absence is positively inspected. It does not remove a stopped container
until artifact collection succeeds, so a retry can recover logs. Existing
artifacts are accepted only when their mode-0600 contents match byte-for-byte;
a conflicting file fails closed. Failed Start rollback retries within its
bounded cleanup budget. If positive cleanup remains unavailable, `Manager`
retains the entire session ownership record and secrets so a later `Stop` can
finish; it never discards ownership while resources may remain.
`HarnessResult.LogPaths` names stable `agent.json`, `agent.stderr`,
`gateway.log`, and `telemetry.index.json` artifacts. The index lists any
preserved files beneath the private `telemetry/` directory.

The M5 harness design is now exercised by the single M6 Runner oracle. That
oracle preserves the real pinned TB2 task, M3 sandbox, M4 bridge, unmodified
OpenClaw image, and strict deterministic model tool chain. The fake-model
resource still has a unique name, ID, and attempt nonce, one private evidence
bind, and only the task network; it has no task filesystem or Docker socket.
Its bounded ownership proof, exact request/result state machine, dynamic lost
commit, stopped-state reinspection, and positive removal remain M5 harness
coverage. The M6 section owns the retained evidence and adds the independent
evaluator boundary; there is no separate current M5 evidence run.

### M6 independent evaluator and functional oracle

The CLI now constructs the same four concrete Runner roles and executes the
full lifecycle. Each invocation creates one mode-0700
`<output_dir>/<run-id>/` directory, using a timestamp plus random suffix, and
exclusively publishes `run-result.json` there. It prints the same JSON to
stdout. A Runner error does not erase partial results: ARIES persists and
prints them when possible, reports the joined error on stderr, and exits
nonzero. A clean run exits zero. Provider preflight always publishes its
sanitized `live-validation.json`; construction failures return without
manufacturing a Runner result.

Terminal-Bench evaluation has its own command, timeout, environment, stdout,
stderr, CTRF, and reward artifacts. It accepts only exact reward bytes `0` or
`1`, optionally followed by one newline. A successful `fix-git` evaluation
requires reward `1` plus strict pytest 8.4.1 CTRF with exactly two passed
records: `test_about_file` and `test_layout_file`. Unknown fields, missing or
duplicate cases, any other case or status, inconsistent summaries, malformed
timings, or malformed reward are evaluator failures.

The retained M6 evidence was produced by the single deterministic functional
oracle that M7 later evolved into `TestRunnerRealFixGitMonitoredRelease`. It
composes the real pinned benchmark, Docker sandbox, SSH bridge, unmodified
OpenClaw harness, and a strict fake OpenAI-compatible model. Before evaluation
it positively proves verifier paths and canaries absent at `pre_harness`,
`pre_run`, `post_run`, and `immediately_pre_evaluate`; proves the fake has no
task filesystem or Docker socket; stops and removes the harness and fake; and
revokes the bridge. Only then may the benchmark inject the verifier into the
still-live sandbox. The test proves the agent-produced Git state directly,
runs the evaluator, removes the sandbox, and requires empty ARIES container,
volume, and network inventories. It requires Docker and pinned local images but
no paid model API.

Each successful oracle creates one ignored private directory at
`.cache/integration/m6/fix-git-<random>/`. It writes `manifest.json`
exclusively, changes it to mode 0400, reads it back, and validates schema
`aries.m6.oracle.v1`. The manifest records exact pins and separated outcomes;
portable semantic artifact roles and root-relative paths for harness,
evaluation, and `run_result`; SHA-256 plus byte size for every retained
artifact; exact verifier reward bytes, cases, and source hashes; observer
status `not_enabled` with an explicit empty sample list; and explicit empty
container, volume, and network inventories. The manifest links but does not
hash itself. M7 may read this oracle but must not mutate it.

### M7 observer and monitored release proof

`TestRunnerRealFixGitMonitoredRelease` evolves the single full deterministic
integration path rather than adding another paid or redundant oracle. It starts
the concrete recorder before the real Runner and stops it only after Runner
cleanup. OpenClaw still owns its agent result, stderr, gateway log, upstream
telemetry index, and available session and trajectory files; Docker still owns
the sandbox stdout and stderr; Terminal-Bench still owns verifier artifacts.
The monitor adds only read-only resource observations and does not duplicate or
follow component logs.

Each successful run writes a separate ignored private directory at
`.cache/integration/m7/fix-git-<random>/`. Its exclusively created mode-0400
`manifest.json` uses schema `aries.m7.monitored.v1`, portable root-relative
semantic roles, and SHA-256 plus byte size for every retained artifact. It
records the separate harness, isolation, evaluation, observer, and cleanup
outcomes; exact pins, verifier sources, reward bytes, and cases; monitor index
and samples; live-validation disposition; M6 before/after preservation
metadata; and explicit final Docker inventories.

The final fresh local proof produced reward `1`, passed exactly
`test_about_file` and `test_layout_file`, retained 27 hashed artifacts, and
recorded 73 samples at the exact one-second interval. Coverage includes both
the task sandbox and OpenClaw container with shared sampled seconds. Final
container, volume, and network inventories were empty. Ordinary integration
snapshots and preserves zero or more ignored local M6 manifests, so a clean
checkout has no historical-artifact prerequisite. Setting
`ARIES_REQUIRE_CANONICAL_M6=1` turns on the final-release audit that requires the
expected retained canonical M6 hash. This workspace used that opt-in and proved
the canonical manifest byte-identical. Ignored `.cache` evidence is a local
release proof, not shipped runtime state.

## Exact lifecycle and evaluation isolation

The composition root may start the observer before the Runner and stop it after
Runner cleanup, but observation does not alter the following per-task sequence:

1. obtain the task;
2. start the sandbox;
3. start the bridge;
4. give endpoint and model configuration to the harness;
5. start the harness;
6. run one instruction;
7. wait for completion, failure, cancellation, or timeout;
8. stop the harness and positively confirm container exit and `State.Running`
   false;
9. stop the bridge and positively confirm listener and server-process absence,
   old-key reconnect rejection, and credential deletion;
10. only after both confirmations, inject and run the private evaluator in the
    still-live sandbox;
11. collect separate harness, isolation, evaluation, observer, and cleanup
    outcomes and artifacts;
12. stop and remove the sandbox.

An ordinary harness failure still permits evaluation when both isolation gates
pass and the sandbox remains valid. If either gate fails, no verifier material
is injected, evaluation is `blocked_isolation`, cleanup continues, and primary
plus cleanup errors remain visible. Every Stop is idempotent. Partial Start
rollback is owned by the component that partially acquired the resource.

## DeepSeek secret flow

The experiment stores only `api_key_env: "DEEPSEEK_API_KEY"`. For the exact
official DeepSeek URL and supported V4 model IDs, `cmd/aries` first anchors the
key location to the trusted repository layout: the executable must be the
regular non-symlinked `bin/aries`, the parent must contain the expected bounded
`go.mod`, and the repository root itself must not be a symlink. The fixed
ignored root file `DEEPSEEK_API.key` wins when present. The loader uses
`O_NOFOLLOW`, requires a current-user-owned regular mode-0600 file, verifies
stable device, inode, size, and modification time before and after its bounded
read, accepts at most 16 KiB with one optional terminal LF, and rejects empty,
NUL, CR, or remaining LF content. Only when the fixed file is absent does the
named environment variable provide the credential.

Lookups return fresh byte clones. The harness copies the credential through a
private stdin tar stream into a task-local UID-1000 mode-0600 volume file. The
rendered config retains only the variable reference; the private launcher
exports the value inside OpenClaw. Source and request buffers are explicitly
cleared. The value never enters Docker `Config.Env`, command arguments, task
or evaluator environments, logs, results, telemetry, digests, or tracked
content. Terminal rollback and successful Stop clear retained host bytes and
positively remove the harness container and private volumes.

Before any Docker or Runner component construction, official DeepSeek runs make
an authenticated, redirect-disabled `GET https://api.deepseek.com/models` with
a ten-second attempt timeout and 64 KiB response cap. The response must contain
the exact requested `deepseek-v4-flash` or `deepseek-v4-pro` ID. ARIES makes at
most two attempts and retries only transport errors or HTTP 500/503, after a
two-second delay. `live-validation.json` records only a typed sanitized status,
category, provider, endpoint, model, and attempt count. A failed preflight
starts no Docker, harness, Runner, or paid model turn.

One optional live validation recorded `model_confirmed` in one attempt, reward
`1`, 103 one-second samples, confirmed isolation and cleanup, and empty final
ARIES resource inventories; it supplements but does not replace the
deterministic monitored release oracle.

## Evidence and repository completion contract

M6 writes an immutable **functional-oracle manifest** for its deterministic
reward-`1` run, with observer status `not_enabled` and no samples. M7 reads but
does not mutate it. M7 writes a separate immutable **monitored release
manifest** for a fresh deterministic reward-`1` run, with one-second samples
and a separate observer outcome. Both are ignored local evidence.

The completed M7 release includes build, unit, race, lint, integration,
security-scan, manifest, documentation, and cleanup evidence. README documents
setup, clean-checkout integration, the opt-in canonical-M6 audit, production
execution, artifact interpretation, architecture, and the explicit extension
path. Post-release verification is read-only.

## ADR-001 summary

Select the bridge-owned static Go client and server while retaining OpenClaw's
upstream SSH backend. This preserves the unmodified image and the four-role
Runner. M0 accepts this decision after the approved Architect/Critic review and
the executable probes above. The detailed alternatives, invalidation triggers,
and security pre-mortem remain in the local OMX planning record. The decision
is invalidated if the pinned argv, config, workspace, or exec grammar cannot be
supported without a permissive fallback; that event returns to planning rather
than weakening the boundary.
