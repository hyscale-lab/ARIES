# ARIES

ARIES is a small Go benchmark runner. Its first complete path runs the pinned
Terminal-Bench 2 `fix-git` task with an unmodified upstream OpenClaw container,
OpenClaw's SSH sandbox backend, one local Docker task sandbox, a remote
OpenAI-compatible model, and an evaluator that runs only after harness access
has been revoked.

ARIES implements only the Terminal-Bench fields needed by this pinned task. It
does not depend on Harbor at runtime and does not claim general Harbor
compatibility.

## Architecture

The Runner depends on exactly four substitutable roles, all defined where they
are consumed in `pkg/runner`:

1. `Benchmark` discovers tasks and independently evaluates the resulting
   sandbox state.
2. `AgentHarness` runs one instruction through the model and tool endpoint.
3. `ToolSandbox` creates the task container and returns its live capability.
4. `ToolBridge` adapts that capability to OpenClaw's upstream SSH backend.

The composition root in `cmd/aries` constructs the concrete Terminal-Bench 2,
OpenClaw, Docker, and OpenClaw-SSH packages with explicit type switches. Shared
data stays in `pkg/core`; benchmark and harness implementations do not import
one another.

`pkg/monitor.Recorder` is a concrete observer outside the Runner, not a fifth
component interface. The composition root starts it before `Runner.Run` and
stops it after Runner cleanup with a fresh bounded context. It makes only
label-scoped Docker Engine `GET` requests, discovers the task sandbox and
OpenClaw container by exact `aries.run` ownership, and samples CPU and memory
once per second. It never starts, stops, retries, or scores a component. A
monitor failure is reported separately and never prevents the Runner or
evaluator from completing.

The task lifecycle remains:

1. discover the task;
2. start its Docker sandbox;
3. start the SSH bridge;
4. start OpenClaw with the bridge endpoint and model configuration;
5. send one task instruction;
6. stop OpenClaw;
7. revoke the bridge;
8. inject and run the benchmark verifier in the still-live sandbox; and
9. stop and remove the sandbox.

Cleanup runs in reverse order after failure or cancellation. An ordinary
harness failure is retained but still permits evaluation when harness shutdown
and bridge revocation are both positively confirmed. A failed isolation gate
blocks evaluation and keeps verifier files out of the sandbox.

## Repository boundaries

All implementation and generated state belongs inside this repository.
`invitro` was consulted only as a structural reference and `agent_bench` only
for workflow archaeology; neither repository was written or changed. Two early
read-only `git status` checks there are disclosed in [DESIGN.md](DESIGN.md).

The Terminal-Bench checkout, run artifacts, generated credentials and config,
OMX state, and `DEEPSEEK_API.key` are ignored. API-key values must never enter
experiment JSON, Docker metadata, command arguments, task or evaluator
environments, logs, telemetry, results, or tracked files.

## Build and setup

Go 1.25.12 or newer and a reachable local Docker daemon are required for the
real integration path.

```sh
make build
./bin/aries setup terminalbench2
```

The setup command clones only the pinned Terminal-Bench 2 revision into
`.cache/terminal-bench-2`. It is idempotent at the correct revision and refuses
to replace an existing checkout at a different revision. The equivalent source
setup command is `make setup-terminalbench`.

Run all release checks with:

```sh
make build
make test
make test-race
make lint
make integration
```

Unit, race, and lint checks require neither Docker nor a paid API. Integration
uses real local containers and a deterministic fake OpenAI-compatible endpoint.
It includes the full monitored `fix-git` release oracle and requires no paid
model credential. From a clean checkout, ordinary `make integration` snapshots
and preserves zero or more ignored local M6 manifests; historical workstation
artifacts are not a prerequisite.

When the retained canonical M6 audit artifact is available, run the stricter
final-release audit with:

```sh
ARIES_REQUIRE_CANONICAL_M6=1 make integration
```

That opt-in requires the expected canonical M6 manifest and hash instead of
accepting an empty local M6 set. The final audit in this workspace used the
opt-in and preserved canonical SHA-256
`84c17fe2ff6717907528221d461b5992f227dd6970f76e21cf908fe42cdc2143`.

To run only that deterministic full path after `make build`:

```sh
ARIES_EXEC_HELPER="$PWD/bin/aries-exec-helper" \
ARIES_SSH_CLIENT="$PWD/bin/aries-ssh" \
ARIES_SSH_SERVER="$PWD/bin/aries-ssh-server" \
go test -v -count=1 -run '^TestRunnerRealFixGitMonitoredRelease$' \
  -tags=integration ./pkg/harness/openclaw
```

## Production DeepSeek run

The checked-in experiment uses the current official endpoint and model:

- base URL: `https://api.deepseek.com`
- model: `deepseek-v4-flash`

Place the single-line key in the fixed ignored repository-root file and make it
private, then run the already-built repository binary:

```sh
chmod 600 DEEPSEEK_API.key
./bin/aries configs/openclaw-tb2-fix-git-deepseek.json
```

The binary trusts this repository root only when it is running as the regular,
non-symlinked `bin/aries` beside the repository's validated `go.mod`. If the
fixed key file exists, it wins. ARIES opens it without following symlinks,
requires a regular mode-0600 file owned by the current user, verifies stable
identity, size, and modification time while reading, bounds it to 16 KiB, and
accepts one key with at most one terminal LF. The environment variable named by
the experiment is consulted only when that fixed file is absent. Key bytes are
cloned only for request-local use and cleared afterward.

Before Docker or any other runtime resource is constructed, an official
DeepSeek configuration makes an authenticated, redirect-disabled `GET
/models` request with a 64 KiB response cap and bounded timeouts. It requires
the exact requested model. At most two preflight attempts occur; only transport
errors and HTTP 500 or 503 are retried, after two seconds. The sanitized outcome
is written to `live-validation.json`. Official DeepSeek V4 requests add
request-scoped `--thinking off`; deterministic and other endpoints are
unchanged.

One optional live validation confirmed the model in one preflight attempt and
completed with score and reward `1`, 103 one-second samples
(`task-container`: 68; `openclaw-harness`: 35), confirmed isolation, successful
cleanup, and no remaining ARIES Docker resource. The deterministic monitored
oracle remains the repeatable release proof.

## Results and artifacts

Every experiment invocation creates a private unique
`<output_dir>/<run-id>/`. A run that reaches the Runner writes
`run-result.json` and prints the same JSON to
stdout. Each task result keeps independent `harness`, `isolation`, `evaluation`,
`observer`, and `cleanup` records, followed by the aggregate summary. Functional
and cleanup errors remain visible even when partial results can be persisted.

The useful tree is:

```text
runs/<run-id>/
├── live-validation.json
├── run-result.json
├── monitor/<task>/
│   ├── resources.jsonl
│   └── index.json
├── harnesses/<attempt>/
│   ├── agent.json
│   ├── agent.stderr
│   ├── gateway.log
│   ├── telemetry.index.json
│   └── telemetry/                 # upstream sessions/trajectory when present
├── sandboxes/<attempt>/
│   ├── container.stdout.log
│   └── container.stderr.log
└── <task>/evaluation/
    ├── stdout.log
    ├── stderr.log
    ├── ctrf.json
    └── reward.txt
```

OpenClaw log collection already retains its final response, gateway output,
upstream telemetry index, and available trajectory files. The Docker sandbox
retains container logs. Terminal-Bench evaluation owns its verifier command,
timeout, environment, logs, CTRF, and reward. Monitor `resources.jsonl` stores
bounded typed samples; `index.json` records interval, duration, status, sample
count, and per-container coverage.

The ignored deterministic release evidence lives under
`.cache/integration/m7/fix-git-<random>/manifest.json` with schema
`aries.m7.monitored.v1`. The final fresh proof recorded reward `1`, exact passed
cases `test_about_file` and `test_layout_file`, 73 one-second samples covering
both the sandbox and OpenClaw with shared sampled seconds, 27 hashed artifacts,
and empty container, volume, and network inventories. The workspace's opt-in
final audit also proved its retained canonical M6 manifest unchanged. These are
local validation artifacts, not checked-in product data.

## Package map and extension path

- `cmd/aries`: strict configuration, explicit construction, key loading,
  provider preflight, monitor composition, and result persistence.
- `pkg/runner`: the four interfaces and lifecycle ordering.
- `pkg/core`: generic task, environment, endpoint, command, and result data.
- `pkg/benchmark/terminalbench`: pinned `fix-git` discovery and evaluation.
- `pkg/sandbox/docker`: task container, exec, transfers, logs, and cleanup.
- `pkg/bridge/openclawssh`: ephemeral verified SSH access to that sandbox.
- `pkg/harness/openclaw`: upstream config, one turn, logs, and telemetry.
- `pkg/monitor`: observer-only Docker resource sampling.

To add the next benchmark, harness, sandbox, or bridge, implement one concrete
package against the small interface in `pkg/runner` and add one explicit switch
case in `cmd/aries`. The Runner workflow does not change.

See [DESIGN.md](DESIGN.md) for locked security and lifecycle decisions and
[TASKS.md](TASKS.md) for the completed MVP milestone record.
