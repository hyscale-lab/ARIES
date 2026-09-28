# RoadmapBench

ARIES adapts the 115 tasks in the official
[RoadmapBench dataset](https://huggingface.co/datasets/UnipatAI/RoadmapBench/tree/59184e779909300a5a0150b06b945d39da81a099).
Each task asks an agent to implement a software version's roadmap in a live
repository. Evaluation uses the task's official verifier and supports partial
completion. The adapter uses the existing `Benchmark` role with any supported
harness/bridge pair.

## Setup and run

The checked-in example selects `opt-3.0.0-roadmap` with Codex. Prepare the pinned
CLI using the [Codex guide](../codex.md), then replace the endpoint and model
placeholders in `profiles/codex-roadmapbench-smoke1-openai.json` and provide
the credential named by `model.api_key_env`. The endpoint must support the
Responses API required by that harness.

```sh
make build
./bin/aries setup profiles/codex-roadmapbench-smoke1-openai.json
./bin/aries profiles/codex-roadmapbench-smoke1-openai.json
```

For all 115 tasks, replace the same endpoint and model placeholders in
`profiles/codex-roadmapbench-all115-openai.json`, then run:

```sh
./bin/aries setup profiles/codex-roadmapbench-all115-openai.json
./bin/aries profiles/codex-roadmapbench-all115-openai.json
```

The full profile selects each pinned task once in task-ID order.

`profiles/codex-roadmapbench-qwen38-27b-xhigh-first20.json` selects the first
20 tasks in that same order, from `dsl-2.1.0-roadmap` through
`glz-3.0.0-roadmap`. It uses `Qwen/Qwen3.8-27B`, `xhigh` reasoning effort,
and up to three native Codex subagents. Replace its endpoint placeholder,
provide `MODEL_API_KEY`, and run:

```sh
./bin/aries setup profiles/codex-roadmapbench-qwen38-27b-xhigh-first20.json
./bin/aries profiles/codex-roadmapbench-qwen38-27b-xhigh-first20.json
```

This profile's native
[`developer_instructions`](https://learn.chatgpt.com/docs/config-file/config-reference)
define the **ultra** execution
strategy: plan briefly, delegate an independent subtask early, parallelize
bounded work with exclusive edit ownership in the shared sandbox, and have
the parent verify and integrate results and run tests. Child spawns omit
`model`, `reasoning_effort`, and `agent_type` to inherit the parent
configuration. Ultra names the prompt strategy; the API reasoning effort
remains `xhigh`.

All three examples use sequential task execution (`execution.concurrency: 1`)
and leave `overrides_file` empty, retaining the task's published resources
and two-hour agent budget. Set `model.context_length` to the actual model
server's context window before running the first20 profile.

Setup prepares data and images without contacting the model. Runs use the
normal ARIES model, concurrency, task-order, and resource-override settings.
Replace `benchmark.tasks` with task directory names from the pinned dataset
to select other tasks. Duplicate selections produce independent occurrences.
RoadmapBench does not accept `benchmark.environment`, `judge`, or `fact`.

The dataset pin in `configs/versions.json` is
`59184e779909300a5a0150b06b945d39da81a099`. Setup uses a shallow, sparse Git
checkout under `.cache/roadmapbench`, fetching one task's instructions, TOML,
Dockerfile, and private verifier tree at a time. It omits the large vendored source
repositories and oracle solutions; task source comes from the published images.
An existing checkout must be clean and match the exact revision.

The pinned task files name untagged `znpt/roadmapbench-*` images. ARIES makes
their implicit `:latest` tag explicit. These upstream image tags are mutable:
pinning the dataset does not pin the image bytes. All currently published
images target Linux amd64. ARIES pulls images through the Moby SDK and does
not build task Dockerfiles.

Each task's configuration supplies CPU, memory, storage, and timeouts. The
pinned release uses `/app`, two CPUs, a two-hour agent deadline, and a
30- or 40-minute verifier deadline. Omitted `allow_internet` defaults to true,
matching Harbor. Explicit false remains false. The runtime override
`verifier_timeout_floor_seconds` may raise the verifier budget.

## Isolation and results

Only the task instruction and environment enter `core.Task`. Verifier files
stay in the host checkout; source symlinks and unsupported execution settings
are rejected. Before agent access, preparation removes and positively confirms
the absence of verifier and oracle staging paths, the Diesel build patch,
and the Polars oracle wheel directory. For `plr-1.35.0-roadmap` it also removes
pip's cache, which could retain the downloaded oracle wheel.

After the harness is positively stopped and the bridge revoked, evaluation
rechecks the source revision and clean tree, resets verifier staging and known
stale score/binary outputs used by eight upstream task scripts, uploads
only the private `tests/` tree, and runs `/bin/bash /tests/test.sh` in the same
live task sandbox. The agent's repository changes remain in place. Upstream
dependency and object caches are preserved. Task networking is enabled by the
published configuration, so this does not prevent retrieval
of public upstream code.

`evaluation.score` and `evaluation.reward` contain the finite value in `[0,1]`
written by the official `reward.txt`. Only exactly `1` yields succeeded
evaluation/verifier statuses. A valid partial score is a task outcome with no
evaluator error; missing or malformed rewards, failed verifier execution, and
transport failures are evaluator errors. Reward JSON schemas differ across
tasks and are retained verbatim when present. No common CTRF file is required.

Private occurrence artifacts include verifier stdout/stderr, `reward.txt`,
and available `reward.json`. Downloads and captured command output are bounded.
The official Completion Score is the mean task reward; Resolved Rate is the
fraction with reward exactly `1`. ARIES preserves the individual values and
existing success counts; it does not introduce another aggregate-results schema.
See the [official metrics](https://github.com/UniPat-AI/RoadmapBench/tree/9bbc3432d9cd36061c0286ac16a786c4e3bafe52#metrics).

Dataset-backed integration tests load all 115 tasks. A deterministic Docker
fixture separately checks verifier injection, partial/full rewards, and positive
sandbox cleanup without a paid API.
With the pinned Codex CLI installed, another regression runs the complete
Runner through native parent/child delegation, SSH, and RoadmapBench evaluation.
It checks the first20 profile's xhigh and ultra instructions, verifier privacy,
partial/full rewards, and cleanup against a deterministic Responses endpoint.
