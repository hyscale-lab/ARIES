# ARIES

ARIES is a small, human-readable Go benchmark runner. Its first working path
runs the pinned Terminal-Bench 2 `fix-git` task through
an unmodified upstream OpenClaw container, OpenClaw's SSH sandbox backend, one
local Docker task sandbox, and an evaluator that runs only after harness access
has been revoked.

## Status

M0 established the accepted architecture, upstream compatibility pins,
repository boundaries, and implementation checklist. M1 adds the Go module,
strict experiment configuration, direct shared data, and the lifecycle Runner
with unit-tested failure, cancellation, isolation, and cleanup behavior. M2
adds the narrow pinned Terminal-Bench 2 `fix-git` loader and independent
evaluator. M3 adds the real local Docker task sandbox with scoped networking,
resources, exec, transfers, logs, positive cleanup, and a real-container test.
M4 adds the restricted OpenClaw SSH bridge, ephemeral credentials, a pinned
static client, shared-workspace proof, access revocation, and real-container
isolation tests. M5 adds the pinned upstream OpenClaw harness, private config and
secret staging, readiness and one-turn execution, redacted artifacts, and
strict deterministic fake-model coverage. M6 preserves that upstream OpenClaw
tool-chain coverage in the single Runner oracle, independently runs the private
verifier after positive isolation, and records the reviewed functional proof.
Monitoring and the optional live DeepSeek proof remain M7 work.

- [DESIGN.md](DESIGN.md) records component boundaries, lifecycle ordering,
  evaluation isolation, pinned upstream contracts, secret flow, and M0 runtime
  probe evidence.
- [TASKS.md](TASKS.md) is the milestone and release checklist from M0 through
  the deterministic end-to-end proof and monitoring work.

## M0 boundaries

All implementation and generated state belongs inside this repository.
`invitro` was consulted only as a structural reference and `agent_bench` only
for workflow archaeology; neither repository was written or changed. The
design record discloses two early read-only `git status` checks there, before
the ARIES-only Git boundary was locked.

Local workflow state, the Terminal-Bench cache, run artifacts, generated
credentials/configuration, and `DEEPSEEK_API.key` are ignored. `AGENTS.md` and
`PROMPT.md` are retained as the repository governance and originating
requirements. Secret values must never enter experiment JSON, results,
evaluator environments, logs, telemetry, or tracked files.

## Build, set up, configure, and run

Go 1.25.0 or newer is required. `make build` writes `aries` and the three
static runtime helpers `aries-exec-helper`, `aries-ssh`, and
`aries-ssh-server` beside one another under `bin/`. Build, unit, race, and lint
require neither Docker nor a model API:

```sh
make build
make test
make test-race
make lint
```

The integration gate requires a reachable local Docker daemon:

```sh
make integration
```

`make integration` runs the real M3 container lifecycle, M4 SSH bridge, and the
single M6 deterministic Runner oracle sequentially. The oracle subsumes the M5
upstream OpenClaw tool-chain coverage and adds independent evaluation. The gate
fails on daemon, isolation, evaluator, manifest, or cleanup errors, pulls pinned
images only when missing, and requires no paid model API. Set up the ignored
TB2 checkout first; the M2 real-checkout test skips clearly when it is absent,
while the M6 oracle requires it.
Fetch the only supported TB2 revision into the ARIES-local cache with:

```sh
./bin/aries setup terminalbench2
```

The command is idempotent when the revision is correct and refuses to replace a
wrong existing checkout. It never creates a sibling repository. Configure the
run by editing the single strict JSON experiment file; unknown fields are
rejected and the file contains only the API-key environment variable name.
The current CLI invocation is:

```sh
read -r DEEPSEEK_API_KEY < DEEPSEEK_API.key
export DEEPSEEK_API_KEY
./bin/aries configs/openclaw-tb2-fix-git-deepseek.json
unset DEEPSEEK_API_KEY
```

This is the implemented run path, not the deterministic M6 oracle command. The
optional paid live DeepSeek validation and the stricter host-file loader remain
M7 work.

Each invocation creates a private, unique
`<output_dir>/<run-id>/` directory and writes `run-result.json` there. The same
JSON result is written to stdout. A successful run exits zero. If the Runner
returns a functional error, ARIES still persists and prints the separated
results when it can, logs the error on stderr, and exits nonzero. Construction
or persistence failures are also reported on stderr and exit nonzero.

## Package shape through M6

- `cmd/aries` loads one experiment and owns explicit component-type switches.
- `cmd/aries-exec-helper` builds the static task-local helper used for trusted
  Docker exec I/O.
- `cmd/aries-ssh` builds the strict static SSH client invoked by OpenClaw.
- `cmd/aries-ssh-server` builds the restricted task-sandbox SSH server.
- `pkg/config` rejects unknown JSON fields and applies only the visible
  `output_dir: runs` default.
- `pkg/core` contains benchmark-independent tasks, commands, requests, and
  distinct result records.
- `pkg/runner` defines the four substitutable roles and owns lifecycle order.
- `pkg/benchmark/terminalbench` strictly maps only the pinned `fix-git` task to
  generic data and keeps verifier details private until `Evaluate`.
- `pkg/sandbox/docker` owns one labeled task container and network, direct
  Docker Engine exec, descriptor-safe transfers, logs, rollback, and positive
  removal.
- `pkg/bridge/openclawssh` owns the OpenClaw-specific SSH endpoint, credentials,
  workspace alias, restricted server, and positive access revocation.
- `pkg/harness/openclaw` owns pinned OpenClaw config, private credentials,
  readiness, one agent turn, logs, telemetry, and positive removal.

The Runner positively gates evaluation on successful harness `Stop` and bridge
`Stop`. An ordinary harness `Run` error is retained but does not suppress the
independent evaluator when both gates succeed. A failed gate permanently marks
evaluation `blocked_isolation`; cleanup retries may release resources but never
retroactively expose verifier material. Cleanup uses a fresh bounded context
derived with `context.WithoutCancel`, and joined errors preserve both the
functional failure and cleanup failures.

The Terminal-Bench adapter checks the checkout revision before discovery,
rejects unknown TOML fields and unsupported critical features, pins the task
image digest, and derives the required workdir from the environment Dockerfile.
During evaluation it injects only the private tests, runs their command with the
declared timeout and environment, then retains stdout, stderr, CTRF, and exact
reward. Reward parsing accepts only exact `0` or `1` bytes, with one optional
terminal newline. Success requires reward `1` and strict CTRF output from
pytest 8.4.1 containing exactly the two passed cases `test_about_file` and
`test_layout_file`, with no failed, skipped, pending, or other cases. It has no
Harbor dependency and creates no container. TOML parsing
uses `github.com/BurntSushi/toml` v1.4.0 (MIT), because Go's standard library
does not parse TOML.

The Docker adapter requires immutable image references and a Runner-provided
stable run/task identity. It applies exact `aries.run` and `aries.task` labels
to both resources, plus the task's workdir, environment, CPU, memory,
writable-layer storage, GPU count, and network policy. `BuildDir` is
intentionally unsupported.

Exec uses a minimal standard-library Docker Engine Unix-socket client rather
than task-writable control state. `make build` creates a static helper beside
the ARIES binary. Start stages that verified regular file and mounts it
read-only together with a short private socket directory. For each command the
Engine launches the helper directly; the host trusts the connection only when
Linux `SO_PEERCRED` matches the exact daemon-issued exec PID. The helper
executes argv directly, carries bounded stdin and separate stdout/stderr over a
framed protocol, and reports the real exit status. ARIES confirms the helper
PID absent with Docker `top`; it does not use stale `ExecInspect.Running` as a
completion signal. It then closes both socket endpoints, unlinks the per-exec
socket, and confirms path absence. A post-launch failure, socket-cleanup
failure, or cancellation triggers bounded fail-closed recovery by stopping and
restarting the same task container, preserving its writable filesystem and
positively reinspecting identity, mounts, network, and running state. The
restart also invalidates a future
bridge server, so M4 bridge cleanup must tolerate an already-dead server.

Transfers never give Docker a caller-controlled upload path after validation:
uploads copy from an already-open no-follow regular-file descriptor into a
private stage. Downloads land in a private stage and publish beneath the output
root using no-follow directory descriptors and exclusive file creation. Stop
is concurrent-safe, retryable after partial removal, idempotent after success,
and confirms no labeled task container, network, or private socket runtime
remains.

The OpenClaw SSH bridge generates a task-local Ed25519 host key and client key,
starts a memory-only-key server through an authenticated private control
socket, and returns the endpoint plus private host-side source paths. M5 copies
config and credentials as root into a UID-1000-owned mode-0600 private volume
and mounts it read-only; secret source files are never direct bind
mounts. The mode-0555 static client is a read-only bind mount and accepts one
exact non-TTY invocation while enforcing
strict host-key checking. The server admits one public-key user, session
channel, and canonical exec request per connection; it bounds connections,
handshake time, and global requests and accepts only the small pinned
environment-name allowlist. It rejects password authentication, PTYs,
forwarding, subsystems, and arbitrary environment requests.
The fixed OpenClaw workspace alias resolves to the task's real workdir, so SSH
and the later evaluator observe the same inode. Bridge `Stop` always restarts
the task sandbox to kill every SSH descendant, then proves the prior server PID
and new listener absent, rejects the retained old signer, removes credentials
and the uploaded server, and preserves the workspace for evaluation. Failed
Start recovery is separately gated by an atomically acquired workspace root
and an exact private per-attempt marker; foreign or ambiguous state is never
adopted or removed. SSH support uses
`golang.org/x/crypto` v0.54.0 (BSD-3-Clause) with `golang.org/x/sys` v0.47.0 as
its only indirect module; the versions are pinned rather than inherited or
dynamically selected.

The OpenClaw harness accepts only the digest pinned in `DESIGN.md`. It renders
one task-local provider and the locked shared SSH backend, but the JSON contains
only environment-variable references. The model credential and a separate
random gateway token enter a root-only initializer over stdin, land in a
private volume as UID-1000 mode-0600 files, and are exported only inside the
OpenClaw process. They do not enter Docker environment metadata, command
arguments, the task sandbox, or evaluator input. Successful Stop clears both
in-memory byte slices after redacted artifact collection and positive container
and volume removal; a positively completed failed-Start rollback clears them as
well.

Every Docker resource carries a random attempt label. ARIES retains container
IDs plus tentative names before issuing creates, retries inspection through a
bounded proof budget, and acts only when ID plus exact managed, milestone,
kind, task, and attempt labels prove ownership. A first not-found response is
retried and only bounded final absence clears tentative state. It never kills or removes a
foreign name collision. Failed-Start cleanup re-inspects tentative resources;
it prefers a retained immutable ID and falls back to the name only after ID
absence. If cleanup still cannot be proven, the Manager retains tentative or proven
ownership and secret bytes for a later `Stop`. The final harness preserves upstream
`tini -s --`, runs the private launcher as its command, and receives a bounded
graceful stop before KILL fallback and positive process/removal proof.

For each harness attempt, `output_dir/harnesses/<id>/` contains:

- `agent.json`: the bounded JSON result from the single agent turn;
- `agent.stderr`: bounded agent diagnostics;
- `gateway.log`: bounded gateway output with credentials and authorization
  lines removed; and
- `telemetry.index.json`: paths to any retained upstream session/trajectory
  files beneath `telemetry/`.

All files and directories are private. Stop retains a killed container when
collection fails so a later retry can recover the artifacts, and it accepts an
existing artifact only when its bytes match exactly. Harness and evaluator
outcomes remain separate; M5 does not inject verifier files or calculate a
score.

The single M6 oracle subsumes the M5 upstream OpenClaw tool-chain proof. It uses
the real pinned `fix-git` sandbox and upstream image with a separate fake
OpenAI-compatible container. The fake sees only its private evidence directory
and task-local network, never the task filesystem or Docker socket. It advances
only after exact prior tool results, discovers the lost commit dynamically,
and asks OpenClaw to repair and verify the repository through upstream SSH. Its
unique name, retained ID, attempt label, terminal stopped-state proof, and
positive removal preserve the M5 lifecycle guarantees. Run the oracle directly
after the three helpers are built:

```sh
ARIES_EXEC_HELPER=$PWD/bin/aries-exec-helper \
ARIES_SSH_CLIENT=$PWD/bin/aries-ssh \
ARIES_SSH_SERVER=$PWD/bin/aries-ssh-server \
go test -v -count=1 -run '^TestRunnerRealFixGitOracle$' \
  -tags=integration ./pkg/harness/openclaw
```

`TestRunnerRealFixGitOracle` proves verifier paths absent before and after the
harness run, then positively removes the OpenClaw harness and fake model and
revokes the bridge before `Benchmark.Evaluate` can inject verifier files. It
retains a redacted model/tool protocol, Git/filesystem delta, isolation trace,
portable Runner result, verifier output, CTRF, exact reward, cleanup evidence,
and empty final container, volume, and network inventories.

The oracle writes its ignored evidence beneath the private unique directory
`.cache/integration/m6/fix-git-<random>/`. Its `manifest.json` is created once,
changed to mode `0400`, and uses schema `aries.m6.oracle.v1`. Read `outcomes`
first for the separate harness, isolation, evaluation, observer, and cleanup
states. `outcomes.*.artifacts` use portable semantic roles with root-relative
paths, while the `run_result` role points to `m6/run-result.json`; `artifacts`
supplies the SHA-256 and byte size for every retained file. `verifier` records
exact reward bytes, the two case results, and
pinned source hashes. `observer.status` is `not_enabled` with an empty sample
list in M6, and `resource_inventory` must contain three explicit empty lists.
The manifest is evidence, not input to a later run; M7 must not modify it.

An ordinary harness failure remains distinct from evaluation: when harness
termination and bridge revocation are both confirmed, the Runner still invokes
the evaluator against the live sandbox. If either isolation gate cannot be
confirmed, evaluation is `blocked_isolation`, verifier material is never
injected, and cleanup continues.

The deterministic integration gate requires no paid API. Live DeepSeek
evidence is optional and remains an M7 release task.

To add the next benchmark, harness, sandbox, or bridge, implement one concrete
package against the small interface in `pkg/runner` and add one explicit type
case in `cmd/aries`. The Runner lifecycle does not change.
