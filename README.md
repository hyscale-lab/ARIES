# ARIES

ARIES is a small Go benchmark runner. Its first supported experiment runs the
pinned Terminal-Bench 2 `fix-git` task with:

- one local Docker task sandbox;
- an unmodified, pinned upstream OpenClaw container;
- OpenClaw's upstream SSH sandbox backend;
- a remote OpenAI-compatible model, initially DeepSeek; and
- an independent benchmark evaluator.

ARIES implements only the Terminal-Bench fields required by this task. It does
not depend on Harbor at runtime or claim general Harbor compatibility.

## Architecture

The Runner consumes four interfaces defined in `pkg/runner`:

1. `Benchmark` discovers tasks and evaluates their final sandbox state.
2. `AgentHarness` runs the agent against a model and tool endpoint.
3. `ToolSandbox` creates the live task environment.
4. `ToolBridge` grants and later revokes harness access to that environment.

The first concrete path is:

```text
OpenClaw container
    -> SSH
host ARIES bridge listener
    -> Moby SDK ExecStream
same Docker task container
```

The SSH listener runs in the ARIES process, not in the task container. It binds
a dynamic port on the task network's host gateway, authenticates an ephemeral
Ed25519 client key, verifies an ephemeral host key, decodes the exact command
shape emitted by the pinned OpenClaw SSH backend, and streams stdin, stdout,
stderr, and the exit status through Docker exec. Syntactically valid upstream
environment names, including `ARIES_*`, are accepted; only
`OPENCLAW_SHELL=exec` has special meaning. Neither OpenClaw nor the task
container receives the Docker socket.

Each tool command runs in a private process group. Cancellation uses a typed
Moby detached exec to terminate only that group, followed by `ContainerTop` to
prove that the wrapper, group, and termination exec are absent. It never stops,
restarts, or recreates the task container.

OpenClaw computes a shared workspace at
`/aries/openclaw/openclaw-ssh-shared-8198076c/workspace`. The bridge aliases that
path inside the task container to the benchmark workdir. Agent tool calls and
the later evaluator therefore observe the same filesystem in the same running
container.

Docker behavior in the sandbox, harness, and monitor uses the official split
Moby SDK modules. There is no Docker CLI parsing or hand-written Engine HTTP
transport.

`pkg/monitor.Recorder` is an observer composed outside the Runner. It samples
CPU and memory once per second through read-only Moby client methods. It never
starts, stops, retries, or scores a component, and monitor failure is reported
separately.

## Lifecycle and evaluation boundary

For each task, the Runner:

1. asks the benchmark for the task;
2. starts its Docker sandbox;
3. starts the SSH-to-Docker-exec bridge;
4. starts OpenClaw with the bridge endpoint and model configuration;
5. sends one instruction and waits for completion or timeout;
6. stops OpenClaw;
7. stops the bridge and revokes every SSH session;
8. asks the benchmark to inject and run its verifier in the live sandbox; and
9. collects artifacts, then stops and removes the sandbox.

Cleanup runs in reverse order after errors or cancellation. Bridge Stop returns
any tool-termination, handler-drain, evidence-flush, or credential-cleanup
confirmation failure. Evaluation starts only after both harness termination and
bridge revocation are confirmed; the Runner blocks it otherwise. If the harness
fails but isolation succeeds, evaluation still runs and its result stays
separate from the harness result. Verifier files are unavailable to OpenClaw and
are uploaded only after access has been revoked.

## Package map

- `cmd/aries`: strict config loading, explicit construction, model preflight,
  monitor composition, and result persistence.
- `pkg/core`: shared task, environment, command, endpoint, and result data.
- `pkg/runner`: the four interfaces and lifecycle ordering.
- `pkg/benchmark/terminalbench`: pinned `fix-git` discovery and evaluation.
- `pkg/sandbox/docker`: task container, network, exec, transfer, logs, cleanup.
- `pkg/bridge/openclawssh`: host SSH listener, credentials, command proxy, tool
  log, and workspace alias.
- `pkg/harness/openclaw`: upstream container, generated config, one agent turn,
  and OpenClaw logs and telemetry.
- `pkg/monitor`: observer-only resource sampling.

## Pinned first path

- Go: `1.25.12` or newer.
- Terminal-Bench 2: commit
  `2fd12b88aafdd04a52c298e3940bcb189f9766d6`.
- `fix-git` image:
  `alexgshaw/fix-git:20251031@sha256:61e431c00c58df652287aadce5457634d9f9330cfdd153ebdf2802df0d540119`.
- OpenClaw: tag `v2026.5.26`, image
  `ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3`.
- Moby client/API: `github.com/moby/moby/client v0.5.0` and
  `github.com/moby/moby/api v1.55.0`.
- Initial live model: `https://api.deepseek.com`, `deepseek-v4-flash`.

A reachable local Docker daemon is required for integration and experiment
runs. The current integration path is validated against Docker Engine 29.6.2.

## Build and setup

```sh
make build
./bin/aries setup terminalbench2
```

The setup command creates the pinned checkout at
`.cache/terminal-bench-2`. It is idempotent at the correct revision and refuses
to replace a checkout at a different revision. `make setup-terminalbench` is
the equivalent source command.

Run the checks with:

```sh
make test
make test-race
make lint
make integration
```

Unit, race, and lint checks require neither Docker nor a paid API. Integration
uses real local containers, the pinned task and OpenClaw images, the real host
SSH bridge, and a deterministic fake OpenAI-compatible endpoint. Its end-to-end
assertion requires monitor samples from both the task sandbox and OpenClaw
container kinds. The Makefile builds and supplies the small `aries-ssh` client
required because the upstream OpenClaw image does not include a system SSH
client.

## Configuration and execution

The MVP accepts one strict JSON experiment file. Unknown fields and trailing
JSON values are rejected. Component types are selected with explicit switches;
there are no profiles, inheritance, plugins, or registration framework.

The checked-in example is:

```sh
./bin/aries configs/openclaw-tb2-fix-git-deepseek.json
```

For the official DeepSeek configuration, place the key in the ignored
repository-root `DEEPSEEK_API.key` file with mode `0600`. The environment
variable named by `api_key_env` is used only when that file is absent. ARIES
validates the requested model before constructing Docker resources. Key values
are not stored in experiment JSON, task or evaluator environments, Docker
metadata, logs, telemetry, or results.

## Results and artifacts

Each invocation creates a private unique `<output_dir>/<run-id>/` and writes
`run-result.json`. Harness, isolation, evaluation, observer, and cleanup
outcomes remain separate.

```text
runs/<run-id>/
├── live-validation.json
├── run-result.json
├── bridges/<attempt>/
│   └── tool-calls.jsonl
├── harnesses/<attempt>/
│   ├── agent.json
│   ├── agent.stderr
│   ├── gateway.log
│   ├── telemetry.index.json
│   └── telemetry/                 # when supplied by upstream OpenClaw
├── sandboxes/<attempt>/
│   ├── container.stdout.log
│   └── container.stderr.log
├── monitor/<task>/
│   ├── resources.jsonl
│   └── index.json
└── <task>/evaluation/
    ├── stdout.log
    ├── stderr.log
    ├── ctrf.json
    └── reward.txt
```

`bridges/<attempt>/tool-calls.jsonl` remains after bridge shutdown and its path
is included in the task result. Each record contains run/task/container
identity, an operation class, safe command metadata and hash, environment names,
atomic stream byte counts, duration, exit code, and outcome. It never records
stdin, stdout, stderr, credential values, environment values, or the raw shell
script.

## Adding the next component

Add one concrete package that implements the small interface in `pkg/runner`,
then add one explicit constructor switch in `cmd/aries`. The Runner lifecycle
does not change. Pair-specific adaptation stays in the bridge package; benchmark
and harness packages must not import one another.

## Repository boundaries

All implementation and generated state belongs inside this repository.
`invitro` was consulted only as a structural reference and `agent_bench` only
for workflow archaeology; neither repository was written or changed. Two early
read-only `git status` checks there are disclosed in [DESIGN.md](DESIGN.md).

`.cache/`, `runs/`, generated runtime files, OMX state, and
`DEEPSEEK_API.key` are ignored. See [DESIGN.md](DESIGN.md) for the current
component and security decisions and [TASKS.md](TASKS.md) for milestone status.
