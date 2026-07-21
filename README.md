# ARIES

ARIES is a small, human-readable Go benchmark runner under construction. Its
first complete path will run the pinned Terminal-Bench 2 `fix-git` task through
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
isolation tests. The milestone is pending review and commit.

The OpenClaw harness lands in M5. Until then the CLI constructs and validates
the Terminal-Bench, Docker, and SSH bridge components, then deliberately
reports that the concrete harness is not implemented; it does not substitute
mocks or an empty implementation.

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

## Current commands

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

`make integration` runs the real M3 container lifecycle and M4 SSH bridge
sequentially and fails on daemon, isolation, or cleanup errors. It pulls the
official digest-pinned BusyBox fixture only when missing and requires no model
API. The M2 real-checkout test skips clearly when the ignored TB2 pin has not
been set up.
Fetch the only supported TB2 revision into the ARIES-local cache with:

```sh
make setup-terminalbench
```

The command is idempotent when the revision is correct and refuses to replace a
wrong existing checkout. It never creates a sibling repository. The CLI accepts
one strict JSON file:

```sh
./bin/aries configs/openclaw-tb2-fix-git-deepseek.json
```

The example contains the API-key environment variable name only. At M4 this
command constructs the pinned benchmark, Docker sandbox manager, and OpenClaw
SSH bridge, then exits with the explicit M5 harness-not-implemented error after
validation.

## Package shape through M4

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
reward. It has no Harbor dependency and creates no container. TOML parsing
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
socket, and returns the endpoint plus private host-side source paths. M5 must
copy config and credentials as root into a UID-1000-owned mode-0600 private
volume and mount it read-only; secret source files are never direct bind
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
