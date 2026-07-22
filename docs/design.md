# ARIES Design

Status: **implemented and verified.** This document describes only the current
architecture.

## Goal

ARIES is a small, readable Go benchmark runner. Its first supported path runs
the pinned Terminal-Bench 2 `fix-git` task with an unmodified upstream OpenClaw
container, a remote OpenAI-compatible model, one local Docker task sandbox, and
an independent evaluator.

The implementation is intentionally narrow. It supports only the task and
upstream behavior required by this path. Kubernetes, additional benchmarks,
additional harnesses, and additional bridge combinations are future concrete
implementations, not framework code in the MVP.

## Workspace boundary

- `aries` is the only write boundary and Git-operation boundary.
- `invitro` is read-only structural and style evidence.
- `agent_bench` is read-only workflow archaeology; none of its Python
  architecture or compatibility layers is migrated.
- Before this boundary was recorded, one read-only `git status` ran in each
  reference repository. Neither changed any file. All later Git operations run
  only in `aries`.
- `.cache/`, `runs/`, `.omx/`, generated runtime files, and
  `DEEPSEEK_API.key` are ignored.

The module is `github.com/hyscale-lab/aries`. There is no DI framework,
reflection, init-time registration, plugin system, config inheritance, generic
factory layer, Harbor runtime dependency, or general utility package.

## Pinned first path

Runtime pins are loaded from the strict checked-in
[`configs/versions.json`](../configs/versions.json), not compiled into the
benchmark or harness packages.

| Component | Pin |
| --- | --- |
| Terminal-Bench 2 | `2fd12b88aafdd04a52c298e3940bcb189f9766d6` |
| `fix-git` image | `alexgshaw/fix-git:20251031@sha256:61e431c00c58df652287aadce5457634d9f9330cfdd153ebdf2802df0d540119` |
| OpenClaw | tag `v2026.5.26`, commit `10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7` |
| OpenClaw image | `ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3` |
| Moby client | `github.com/moby/moby/client v0.5.0` |
| Moby API | `github.com/moby/moby/api v1.55.0` |
| Initial model | `https://api.deepseek.com`, `deepseek-v4-flash` |

Primary compatibility evidence remains the pinned upstream source:

- Terminal-Bench task [configuration](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/task.toml),
  [instruction](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/instruction.md),
  and [verifier](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/tests/test.sh).
- OpenClaw SSH [configuration schema](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/config/types.sandbox.ts#L102-L124),
  [client invocation](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh.ts#L479-L579),
  and [workspace behavior](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh-backend.ts#L114-L177).
- Docker's [Go SDK guidance](https://docs.docker.com/reference/api/engine/sdk/)
  and the official split Moby client/API modules.

A pin change requires rechecking these contracts before implementation changes.

## Components and dependency direction

The four substitutable roles are defined where the Runner consumes them:

- `Benchmark`: task discovery and independent evaluation.
- `AgentHarness`: harness start, one instruction, and stop.
- `ToolSandbox`: creation and removal of a live task environment.
- `ToolBridge`: temporary harness access to that environment.

`Sandbox` is the exec/upload/download capability returned by `ToolSandbox`, not
a fifth main component. Lifecycle remains on `ToolSandbox`, which stops the
exact handle it created. Shared task, environment, command, endpoint, and result
data stays in `pkg/core`.

```text
cmd/aries -> config + runner + concrete constructors
runner    -> core data + four local interfaces
concrete packages -> core data and only the narrow capabilities they consume
```

The benchmark and harness do not import one another. The OpenClaw-specific
Docker adaptation stays in `pkg/bridge/openclawssh`. Explicit switches in
`cmd/aries` are the only implementation selection mechanism.

## Task lifecycle

For every task, the Runner performs this order:

1. obtain the task from the benchmark;
2. start one Docker sandbox from its generic environment;
3. start the SSH bridge for that exact sandbox;
4. start OpenClaw with the endpoint and model configuration;
5. send the instruction and wait for completion or timeout;
6. stop OpenClaw;
7. stop the bridge and revoke all harness access;
8. evaluate the still-running sandbox;
9. collect separate outcomes and logs; and
10. stop and remove the sandbox.

Cleanup runs in reverse acquisition order with a fresh bounded context after
error or cancellation. `Stop` methods are idempotent. A successful harness
`Stop` and bridge `Stop` are positive isolation gates; if either fails,
evaluation is marked `blocked_isolation` and verifier files are not uploaded.
A harness failure does not suppress evaluation when both gates succeed.

## Docker SDK boundary

The sandbox, harness, and Docker resource source use the official split Moby Go
client and API modules. Each package declares a private interface containing
only the typed methods it uses; production supplies `*client.Client`, while
unit tests supply small typed fakes. The generic monitor package does not
import Docker.

`pkg/containerimage` uses the distribution/OCI reference packages to validate
the immutable named image format shared by config, benchmark, harness, and
sandbox code. This keeps registry syntax and digest parsing out of those four
components rather than duplicating hand-written string checks.

The SDK owns API negotiation, typed requests and responses, attach streams,
archive copy, logs, stats, and resource lifecycle. ARIES does not hard-code an
Engine API version, implement Unix-socket HTTP, parse Docker CLI output, or
mount the Docker socket into runtime containers.

The sandbox owns one labeled task network and one labeled task container. It
validates the immutable image, workdir, environment, resources, ownership
labels, network attachment, and running state. It provides buffered `Exec` for
the generic Runner contract and streaming `ExecStream` for the bridge. Upload
and download use Docker archive copy. Stop captures container logs and removes
both container and network.

### Exec completion and cancellation

On the validated Docker Engine 29.6.2 daemon, a hijacked exec attachment and
`ExecInspect.Running` can remain stale after the process has disappeared. ARIES
therefore uses typed `ContainerTop` to prove process absence, briefly drains
then closes the attachment, and uses a per-exec randomized trailer for the
sandbox's exact exit code. The harness applies the same positive process check
while retaining Docker's settled inspected status.

Each sandbox tool command runs in its own private process group. If the SSH
session or command context is canceled, the sandbox starts a narrow detached
termination exec through the typed Moby API, signals only that process group,
and uses `ContainerTop` to confirm that the command wrapper, process group, and
termination exec are all absent. Cancellation never stops, restarts, or
recreates the task container. Failure to prove termination is returned to the
bridge instead of being treated as successful cancellation.

## OpenClaw SSH bridge

The required tool path is:

```text
OpenClaw container -> SSH -> host ARIES listener -> Moby ExecStream
                   -> exact task container
```

`pkg/bridge/openclawssh` binds an ephemeral TCP listener on the task network's
host gateway. It creates an ephemeral Ed25519 host key and client key, writes a
port-qualified `known_hosts` entry, and returns the dynamic endpoint plus host
source paths. The harness copies those files and the static `aries-ssh` client
into its stopped container before start; they are not bind mounts.

The client implements only the pinned OpenClaw non-TTY invocation:

```text
-F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND
```

It requires strict host-key verification and the generated identity. The host
server accepts only the task username/key and session exec channels. It rejects
TTY, subsystem, forwarding, noncanonical quoting, and malformed commands. An
OpenSSH keepalive is the only accepted global request.

Every accepted SSH exec is decoded into argument slices and passed to
`ExecStream` on the same sandbox object and container later given to the
evaluator. Binary and late stdin are streamed; stdout and stderr remain
separate; command exit codes are returned through SSH. Environment assignments
with syntactically valid shell names are accepted, including upstream
`ARIES_*` names; only `OPENCLAW_SHELL` has the required value `exec`. Closing an
SSH connection cancels its Docker exec context and triggers the targeted
termination confirmation described above.

OpenClaw's pinned shared runtime path is:

```text
/aries/openclaw/openclaw-ssh-shared-8198076c/workspace
```

Before listening, the bridge creates a guarded symlink from that path to
`Task.Environment.Workdir` for ordinary shell commands. OpenClaw's native
filesystem helper deliberately refuses to follow symlinks, so the bridge also
maps its explicit filesystem root arguments to the real workdir and maps
canonical-path replies back to OpenClaw's virtual path. The mapping is limited
to the pinned `openclaw-sandbox-fs` command shape; arbitrary shell text is not
rewritten. Partial start rolls back only an alias owned by that attempt. Once
start succeeds, the alias remains until the sandbox is removed, so native
read/write/edit tools, shell commands, and evaluation share one live filesystem.

Bridge Stop closes the listener and active connections, waits for handlers,
closes the log, and deletes staged client credential sources. It never changes
the task container lifecycle. A failed termination, handler drain, evidence
flush, or credential cleanup is returned by Stop. The Runner treats any such
failure as an unconfirmed revocation, blocks evaluation, and does not upload
verifier files. After confirmed revocation, the evaluator can inspect exactly
the state left by the agent.

### Why the bridge is more than a byte relay

The central operation is direct: one accepted SSH exec becomes one Docker
`ExecStream` on the supplied sandbox. The protocols are not equivalent,
however. The bridge must terminate ephemeral SSH authentication and host
verification, reject unsupported channels and command shapes, translate
OpenClaw's canonical shell tokens into argument slices, preserve separate
stdin/stdout/stderr and exact exit status, cancel only the disconnected tool
process group, revoke every session before evaluation, map OpenClaw's fixed
workspace, and retain a replayable tool log.

Putting `sshd` inside the task would add another process and credential
lifecycle to the evaluator's sandbox. Giving OpenClaw the Docker socket would
expose the daemon. A generic transport layer would add an interface without a
second implementation. These pair-specific responsibilities therefore remain
concrete and package-private. Further simplification must preserve the same
isolation, streaming, cancellation, and evidence guarantees.

Compatibility tradeoffs for OpenClaw, Hermes, and OpenHands are recorded in
[bridge-alternatives.md](bridge-alternatives.md).

## Tool-call evidence

Each task creates private:

```text
<task>/bridge/tool-calls.jsonl
```

The log is retained after bridge shutdown and exposed in
`TaskResult.ToolLogPaths`. Accepted and rejected requests record sequence,
time, run/task/runtime identity, operation class, path/workdir metadata,
environment names, a command hash, the exact argument vector and shell command,
exact stdin (UTF-8 or base64 for binary data), byte counts, duration, exit code,
and outcome. The separate human-readable shell-command field is omitted for
OpenClaw's large internal filesystem helpers because their exact script already
exists in the argument vector. Environment values and stdout/stderr content are
not retained.

The streaming counters use atomic updates so concurrently copied stdin, stdout,
and stderr produce race-free final counts. Tool inputs are intentionally stored
in the private mode-0600 artifact for deterministic replay; model credentials
are never sent to the sandbox or recorded. A log write or close failure is a
bridge failure rather than silent evidence loss. Exact stdin is capped at 16
MiB per call and the task tool log at 256 MiB; exceeding either bound fails the
bridge instead of silently truncating replay evidence.

OpenClaw model tool-call IDs are not sent over SSH: the pinned exec backend
drops the internal ID before it constructs the SSH command. ARIES therefore
does not invent an unreliable ID from timing, ordering, or command hashes. The
trajectory remains the authoritative source for model-level IDs.

## OpenClaw harness

`pkg/harness/openclaw` runs one container from the pinned upstream image. It
does not patch or fork OpenClaw. Before start it uses Moby archive copy to stage
the rendered OpenClaw configuration, key files, launch scripts, `aries-ssh`,
identity, known hosts, and required state directories with explicit ownership
and modes.

The generated config selects the remote OpenAI-compatible model and OpenClaw's
upstream SSH backend with shared scope, read-write remote workspace access,
strict host checking, and the bridge's dynamic address. The exact rendered
placeholder-only `openclaw.json` is retained under `<task>/harness/`, and the
same bytes are copied into the container. API-key bytes are provided through a private
runtime file and do not appear in Docker environment metadata, labels, command
arguments, configuration artifacts, or results.

The harness waits for readiness, sends one task instruction through the pinned
agent command, captures the final response, and collects gateway logs, agent
stdout/stderr, and available upstream telemetry. Stop removes the OpenClaw
container and confirms the owned resource is absent.

## Terminal-Bench evaluation

`pkg/benchmark/terminalbench` owns the pinned checkout, strict task parsing,
private verifier metadata, test injection, verifier execution, CTRF validation,
and reward parsing. The generic `Task` contains no Terminal-Bench-specific
fields.

The adapter verifies the dataset revision and verifier file identity before
use. Verifier content is not exposed through `Task` or mounted for the harness.
Only after isolation succeeds does evaluation clear fixed verifier paths,
upload the enumerated files, run the verifier with its own timeout and
environment, and collect stdout, stderr, CTRF, and reward. Reward `1` is
success; reward `0` is a valid failed task; missing or malformed reward is an
evaluator error.

## Monitoring and results

`pkg/monitor.Recorder` is composed outside the Runner. `ResourceSource` is the
small interface it consumes. `pkg/sandbox/docker` implements that interface by
discovering only running containers with the exact run/task ownership labels
and generic `aries.component=sandbox|harness` label, then reading cumulative
CPU and memory counters through the Moby SDK. Concrete `aries.kind` labels are
retained for identity validation, not component selection. The composition
root chooses the resource source in the same explicit sandbox switch that
chooses lifecycle execution. Future deployment packages can implement the same
interface without changing the recorder or JSON schema.

The recorder derives CPU percentage from successive cumulative CPU and wall
clock readings. The first reading for each runtime is a zero-percent baseline;
later readings reflect real deltas. This fixes the old all-zero behavior caused
by requesting one-shot Docker stats without a previous sample. Samples use
schema version 2 with generic runtime identity, cumulative CPU nanoseconds,
derived CPU percentage, and memory usage/limit.

A container may exit between a running list snapshot, inspection, and stats while the
Runner performs cleanup. After identity and ownership validation, monitoring
treats that transition like disappearance and retains earlier samples; malformed
identity, labels, state, stats, or Docker errors still fail the observer report.

ARIES keeps Docker Stats rather than adding cAdvisor for the local MVP. cAdvisor
would require another privileged, broadly mounted, long-lived service while
the Engine already exposes the required counters. cAdvisor remains a sensible
future choice for shared node/Kubernetes Prometheus monitoring, not for one
local task sandbox and harness.

Monitoring never controls lifecycle or scoring. Observer start, sampling, or
stop failure is reported separately and does not replace harness, evaluation,
or cleanup outcomes.

Each run has a private output directory named with its task, for example
`20260722T133727.613764127Z-fix-git`. Artifacts are grouped under
`fix-git/{harness,bridge,sandbox,monitor,evaluation}` rather than opaque attempt
directories. Results preserve separate
harness, isolation, evaluation, observer, and cleanup records. Component log
paths point to retained bridge, OpenClaw, sandbox, monitor, and verifier
artifacts. A private `aries.log` contains structured Logrus lifecycle records.
Model and gateway credentials are excluded from artifacts. Replay logs
intentionally retain task commands and stdin, which may themselves be
sensitive; run directories are therefore private and replay artifacts use mode
`0600`.

## Configuration and secrets

The runnable experiment is the strict JSON profile
[`profiles/openclaw-tb2-fix-git-deepseek.json`](../profiles/openclaw-tb2-fix-git-deepseek.json).
It references the strict JSON version file relative to its own location.
Unknown fields and trailing values are rejected in both documents. This is a
fixed reference, not profile inheritance or configuration merging. The visible
default is `output_dir = "runs"`; component type selection uses explicit
switches.

`aries setup PROFILE.json` verifies or creates the configured Terminal-Bench
checkout and pulls the configured immutable images through the Moby SDK. Normal
runs never silently change datasets or pull images.

API-key values never belong in JSON. For the official DeepSeek configuration,
the repository-root ignored `DEEPSEEK_API.key` is accepted only as a regular,
current-user-owned mode-0600 file with bounded stable contents. The configured
environment variable is the fallback when that file is absent. Official
DeepSeek runs perform a bounded authenticated model preflight before Docker
resource construction.

The executable procedure is maintained in
[`docs/quick-start.md`](quick-start.md).

## Extension rule and non-goals

A second benchmark, harness, sandbox, or bridge adds one concrete package and
one explicit constructor switch. It must not require edits throughout the
Runner. A pair-specific bridge may require a narrow concrete sandbox capability,
but that dependency remains inside the bridge package.

The MVP does not provide Kubernetes, a generic platform layer, arbitrary
Terminal-Bench compatibility, reusable SSH infrastructure, plugins, config
merging, abstract factories, or speculative implementations. New abstractions
require a current substitution need in the Runner, not a hypothetical future.

## Validation contract

Completion requires unit tests without Docker or paid credentials and real
integration tests that prove:

- typed Moby lifecycle, streaming exec, transfer, and cleanup;
- SSH authentication, host verification, stream and exit propagation,
  cancellation, revocation, and retained replayable tool logs;
- a bridge mutation is visible through the evaluator's sandbox capability;
- real pinned OpenClaw emits tool records through the host bridge;
- OpenClaw stops and bridge access is revoked before verifier injection;
- deterministic `fix-git` evaluation returns reward `1`;
- the deterministic end-to-end test records at least one monitor sample for
  both the `task-container` and `openclaw-harness` kinds; and
- no ARIES-owned container, network, credential, key, or process is leaked.

The release command set is `make build`, `make test`, `make test-race`,
`make lint`, `make integration`, plus `govulncheck ./...`.
