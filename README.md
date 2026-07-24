# ARIES

ARIES is a small Go benchmark runner. It runs any selected task from the pinned
Terminal-Bench 2 revision with an unmodified upstream OpenClaw container, one
local Docker task sandbox, OpenClaw's SSH backend, a remote OpenAI-compatible
model, and an independent evaluator.

## Quick start

From the repository root:

```sh
make build
./bin/aries setup profiles/openclaw-tb2-fix-git-deepseek.json
install -m 600 /dev/null DEEPSEEK_API.key
${EDITOR:-vi} DEEPSEEK_API.key
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

The live run uses DeepSeek and can incur API charges. See the complete
[quick-start guide](docs/quick-start.md) for the five-task profile,
prerequisites, success checks, artifacts, and troubleshooting.

## Architecture

The Runner consumes four interfaces defined in `pkg/runner`:

1. `Benchmark` discovers tasks and independently evaluates final sandbox state.
2. `AgentHarness` runs one agent instruction against a model and tool endpoint.
3. `ToolSandbox` starts and stops the live task environment.
4. `ToolBridge` grants and then revokes harness access to that environment.

The first concrete tool path is:

```text
OpenClaw container -> SSH -> host ARIES bridge -> Moby ExecStream
                                             -> task container
```

OpenClaw never receives the Docker socket. The bridge translates the pinned
OpenClaw SSH command shape into Docker exec on the exact sandbox later inspected
by the evaluator. The Runner stops OpenClaw and revokes the bridge before the
benchmark uploads verifier files. Harness failure and evaluation outcome remain
separate.

See [docs/design.md](docs/design.md) for lifecycle, isolation, bridge, Moby SDK,
monitoring, secrets, and extension decisions. See
[docs/bridge-alternatives.md](docs/bridge-alternatives.md) for why future
harnesses should not be forced through one universal transport.

## Configuration

- `profiles/openclaw-tb2-fix-git-deepseek.json` is the quickest live example.
- `profiles/openclaw-tb2-five-deepseek.json` selects a heterogeneous five-task
  subset.
- `configs/versions.json` contains the exact Terminal-Bench 2 Git revision and
  the digest-pinned OpenClaw image. Each task's explicit image tag comes from
  its `task.toml` in that pinned checkout; there is no separate task-image
  catalog.
- `configs/runtime-overrides.json` is the dedicated strict-JSON resource and
  agent-timeout override used by the five-task example. Every checked-in
  profile declares `overrides_file`; the one-task profile sets it to `""` to
  disable overrides.

To choose another subset, copy a profile and replace `benchmark.tasks` with
task directory names from the pinned checkout. Task order is preserved. Both
configuration files use strict JSON decoding; there is no inheritance,
merging, plugin registry, or factory framework. API-key values never belong in
either file.

The sparse `harness_resources` and `agent_sandbox_resources` blocks are
independent. A harness value limits only OpenClaw; an omitted harness dimension
remains unlimited. A sandbox value limits only the task container; an omitted
sandbox dimension retains the value from the task's `task.toml`. Values never
inherit between blocks. A present `agent_timeout_seconds` changes only the
agent deadline. Task containers always receive ARIES-owned
`DEBIAN_FRONTEND=noninteractive` and host-process `TZ`, falling back to `UTC`.

## Packages

- `cmd/aries`: explicit composition, setup, model preflight, and result output.
- `pkg/containerimage`: shared OCI parsing for role-specific tagged and
  digest-pinned image references.
- `pkg/core`: shared task, environment, command, endpoint, and result data.
- `pkg/runner`: the four interfaces and ordered lifecycle.
- `pkg/benchmark/terminalbench`: selected-task discovery and private verifier
  execution for the pinned Terminal-Bench 2 revision.
- `pkg/sandbox/docker`: Moby-backed container, exec, transfer, cleanup, and raw
  cumulative resource collection.
- `pkg/bridge/openclawssh`: authenticated SSH-to-Docker-exec adaptation.
- `pkg/harness/openclaw`: upstream OpenClaw container and generated config.
- `pkg/monitor`: deployment-neutral resource rates and artifact recording.

Every run writes structured Logrus output to stderr and private `aries.log`.
Task artifacts use readable paths such as
`runs/<timestamp>-openclaw-tb2-five-deepseek/fix-git/bridge/tool-calls.jsonl`.
Each task also retains `bridge/ssh_raw.log`, a mode-0600 sensitive, lossless
plain-text audit. It uses delimited, fixed-order `key=value` records: printable
UTF-8 stays readable, while control and invalid bytes use explicit escapes.
It is not JSON or base64. `tool-calls.jsonl` remains one JSON object per line
and writes printable Unicode and HTML characters literally while preserving
JSON-required escaping. Treat both files as private run evidence; they may
contain values supplied by the tool caller.

## Validation

```sh
make build
make test
make test-race
make lint
make integration
```

Unit, race, and lint checks require neither Docker nor a paid API. Integration
uses real local containers and a deterministic fake OpenAI-compatible endpoint.
It also loads the five-task subset and every task in the pinned dataset through
the generic benchmark boundary.

## Adding the next component

Add one concrete package implementing the relevant small interface, then add
one explicit constructor switch in `cmd/aries`. The Runner lifecycle does not
change. Pair-specific adaptation stays in the bridge package; benchmark and
harness packages do not import one another.

## Repository boundary

Only this repository is written or used for Git operations. `invitro` was a
read-only structural reference and `agent_bench` read-only workflow archaeology.
The recorded boundary is in [DESIGN.md](DESIGN.md); milestone history is in
[TASKS.md](TASKS.md).
