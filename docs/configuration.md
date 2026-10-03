# Experiment configuration

Start with the [quick start](quick-start.md) for a first run. This reference owns
profile field meanings, defaults, compatibility rules, and advanced examples.
The schema and validation live in [pkg/config](../pkg/config/config.go) and
[deployment normalization](../pkg/config/deployment.go); available implementations
are summarized in [supported implementations](supported.md).

## Profile structure

Start from a checked-in [profile](../profiles). JSON examples below are fragments
unless explicitly identified as complete files. Keep the remaining required
fields from your chosen profile.

| Field | Purpose |
| --- | --- |
| `name` | Experiment name used in run identity. |
| `versions_file` | Pinned benchmark repositories and harness images. |
| `overrides_file` | Optional resource/deadline overrides; `""` disables loading. |
| `benchmark` | Benchmark type, checkout root, selected task occurrences, and supported evaluator options. |
| `harness` | Agent implementation, mode, placement, and optional tool/model settings. |
| `sandbox.deployment` | Task runtime placement. |
| `bridge` | Paired tool adapter, embedded mode, and optional raw evidence retention. |
| `runtime` | Model service backend and external/managed ownership. |
| `model` | Served model ID, endpoint, and credential environment-variable name. |
| `execution` | Concurrency, looping, or arrival schedule. |
| `output_dir` | Private run output root; defaults to `runs`. |

`versions_file`, nonempty `overrides_file`, and managed SGLang's `config.file`
resolve relative to the profile. Run from the repository root for checked-in
profiles: benchmark roots, output paths, and arrival trace paths use the working
directory. Profiles reject unknown fields and trailing JSON; there is no merge
or inheritance layer. Never put credential values in profile JSON.

## Resource and timeout overrides

The five-task profile additionally references
`configs/runtime-overrides.json` relative to the profile. Its sparse
`harness_resources` and `agent_sandbox_resources` blocks apply only to their
respective containers and may specify different CPU and memory limits. An
omitted harness dimension stays unlimited; an omitted sandbox dimension keeps
the value in the task's `task.toml`. Neither block inherits from the other.
The independent `agent_timeout_seconds` field changes only the agent deadline.
The independent `verifier_timeout_floor_seconds` field raises a Terminal-Bench
task's evaluation budget to at least that many seconds and never lowers one.
It covers the entire verifier command, including dependency installation and
test execution, and leaves the agent deadline unchanged. Some task test scripts
exhaust a short declared budget while installing dependencies.

For example, an overrides file containing the following sets a 900-second
minimum verifier budget:

```json
{
  "verifier_timeout_floor_seconds": 900
}
```

Report the configured floor with benchmark results and use the same verifier
budget policy across compared runs.
Every checked-in profile explicitly contains `overrides_file`; the one-task
profile uses `""`, which disables override loading without opening a file.
Profiles and nonempty referenced override files reject unknown fields and
trailing JSON; there is no profile merge or inheritance layer. SGLang is the
exception only for its separate native launch configuration, described below.

## Deployment configuration

Checked-in profiles select harness identity and placement separately:

```json
{
  "harness": {
    "type": "hermes",
    "deployment": {
      "backend": "docker",
      "docker": { "socket": "/var/run/docker.sock" }
    }
  },
  "sandbox": {
    "deployment": {
      "backend": "docker",
      "docker": { "socket": "/var/run/docker.sock" }
    }
  },
  "bridge": { "type": "hermes-ssh", "mode": "embedded" }
}
```

This is a profile fragment; keep the benchmark, model, runtime, and other
settings from a complete checked-in profile. OpenClaw uses `harness.type:
"openclaw"` with `bridge.type: "openclaw-ssh"`. Pairing is checked independently
of placement.

The separate blocks express independent component placement, so the schema does
not require a future harness and sandbox to share a deployment implementation.
They do not establish support for mixed backends today. A heterogeneous setup
will also need compatible connectivity, identity, isolation, and cleanup semantics;
see the [deployment contract](design/deployment.md).

Both components currently must use the same supported local Docker daemon. `socket`
accepts an absolute Unix socket path or `unix:///absolute/path`; normalization
cleans the path before comparing the two settings. Remote Docker endpoints are
rejected. Image preparation and resource monitoring use the selected socket.
The supported topology remains native Linux Docker with one network per task
occurrence, including repeated task IDs.

Compatibility normalization supplies Docker with `/var/run/docker.sock` when a
deployment block is omitted, defaults an omitted Docker socket to that path,
and defaults omitted `bridge.mode` to `embedded`. Legacy `sandbox.type:
"docker"` maps to Docker deployment; conflicting explicit settings are rejected.
Unknown backends, options from a different backend, and unsupported bridge modes
fail validation. `embedded` means the listener runs inside ARIES. Bridge modes
`managed` and `external` are not implemented.

Bridge addresses come from task composition, with separate local bind and
harness destination addresses; they are not profile fields. Docker uses the
task network gateway for both. The current adapters accept IPv4 addresses,
reject DNS and wildcard destinations, and advertise the port the OS actually
assigned. One authenticated listener grant remains bound to each task.

## Preparation and task selection

Running a profile automatically loads `configs/versions.json`, creates or
verifies the pinned Terminal-Bench checkout at `.cache/terminal-bench-2-1`, reads
each selected task's explicit Docker image tag from its `task.toml`, and pulls
only the configured harness image plus those selected images through the
Docker Go SDK. Preparation
happens before the run directory is created, a managed runtime is started,
model weights load, an external endpoint is contacted, or task work is
admitted. The Terminal-Bench Git revision and exact tag-pinned OpenClaw image
remain in `configs/versions.json`, alongside the tag-pinned Hermes image; task
image digests are not duplicated there.
Preparation is safe to repeat and refuses to replace a checkout at another
revision.

For an optional prewarm, `setup` performs only profile/backend validation and
the same benchmark/image preparation. It does not contact an external model
service, start managed SGLang, load model weights, create a run directory, or
admit tasks:

```sh
make setup
make setup PROFILE=profiles/openclaw-tb2-five-deepseek.json
make setup PROFILE=profiles/openclaw-tb2-fix-git-sglang.json
```

The normal Make workflow is direct run:

```sh
make run
make run PROFILE=profiles/openclaw-tb2-five-deepseek.json
```

To run another subset from the pinned revision, copy either profile and replace
`benchmark.tasks` with the desired task directory names. ARIES preserves the
listed order and repeated entries. Set positive `execution.concurrency` to
bound parallel occurrences. Add a positive Go duration such as `"30m"` as
`execution.loop_duration` to repeat the list until admissions close; admitted
work is always drained. ARIES loads each task's explicit tagged image directly from that
task's `task.toml`; no version-catalog or Go code change is required.

## Replay task arrivals

An arrival trace schedules task occurrences at offsets from the start of task
scheduling, after benchmark/image preparation and model preflight. Scheduling
happens outside the harness, so it works with any benchmark, harness, and mode
listed in [supported implementations](supported.md). Keep the rest of an
existing profile unchanged.

For example, save this trace as `.cache/arrivals.json` (create `.cache` first):

```json
{
  "trace_id": "fix-git-replay",
  "base_rate_per_min": 1.0,
  "arrivals": [
    {"t": 0, "traj": "fix-git"},
    {"t": 60, "traj": "fix-git"},
    {"t": 180, "traj": "fix-git"}
  ]
}
```

`trace_id` is optional descriptive metadata. `base_rate_per_min` is the positive
reference rate. Each `t` is an offset in seconds, not a delay from the previous
entry; `traj` is the exact logical task ID from `benchmark.tasks`. Selected
offsets must be zero or positive and fit Go's `time.Duration` after scaling. The
trace must contain at least one arrival. Extra metadata fields are accepted in
the trace; the experiment profile still rejects unknown fields.

Copy the one-task profile, replace its `execution` block, and set
`benchmark.tasks` as follows (these are profile fragments):

```sh
cp profiles/openclaw-tb2-fix-git-deepseek.json .cache/arrival-demo.json
```

```json
{
  "execution": {
    "concurrency": 3,
    "arrivals_file": ".cache/arrivals.json",
    "arrival_rate_per_min": 2.0
  },
  "benchmark": {
    "tasks": ["fix-git", "fix-git", "fix-git"]
  }
}
```

Keep the other fields from the copied profile. From the repository root, after
configuring the model credential in [model backends](#model-backends), run:

```sh
./bin/aries .cache/arrival-demo.json
```

Relative `arrivals_file` paths resolve from the command's working directory,
not the profile directory. The copied profile's `../configs/versions.json`
continues to resolve relative to `.cache/arrival-demo.json`. Both
`arrivals_file` and a finite, positive `arrival_rate_per_min` must be set
together; omit `loop_duration` because a trace cannot be combined with looping.

The scheduler uses `offset = t * base_rate_per_min / arrival_rate_per_min`.
The example therefore schedules three independent `fix-git` occurrences at
0, 30, and 90 seconds. It replays the supplied offsets; it does not generate a
Poisson stream or guarantee a measured throughput of two tasks per minute.
For each task, its k-th occurrence in `benchmark.tasks` selects its k-th entry
in trace file order. Missing occurrences fail the run; unused trace entries
are ignored. Selected occurrences are then sorted by scaled offset, preserving
profile order for ties. This schedule order determines occurrence IDs and
result order, regardless of completion order.

`execution.concurrency` still caps active occurrences, including evaluation
and cleanup. A full pool delays admission and can make later arrivals overdue;
they run as capacity becomes available instead of being dropped. Choose enough
capacity for the intended overlap (three guarantees a free slot for each
arrival in this example). Cancellation stops further admissions and drains
admitted work through lifecycle cleanup.

Each task's `started_at` in `run-result.json` records when its Runner task
lifecycle began, after occurrence construction, observer startup, and task
loading, but before sandbox startup. It is neither the planned arrival time
nor the first model request time. Starts can be delayed by capacity or setup;
scheduling is not a hard realtime guarantee. Trace contents are loaded during
`run` after model preflight, so `setup` alone does not validate them.

## Model backends

`runtime.mode` states whether ARIES owns a model-server process: an `external`
endpoint is validated but never started, configured, or stopped, and a
`managed` process is owned for the run. `runtime.backend` names the kind of
service behind the endpoint, which selects the preflight and the provider each
harness renders; it is not a runtime ARIES prepares. The supported combinations
are:

| Backend | Mode | `runtime.config` | Process owner |
| --- | --- | --- | --- |
| `deepseek` | `external` | Must be omitted | DeepSeek |
| `sglang` | `external` | Optional `file` accepted but ignored | User |
| `sglang` | `managed` | `file`, `executable`, `startup_timeout`, `stop_timeout` | ARIES |
| `openai` | `external` | Must be omitted | User |

DeepSeek and `openai` are external only. SGLang supports both modes.

HTTP model endpoints are a trusted-local exception. The checked-in HTTP
examples use the non-secret `unused-local-token` placeholder and are suitable
only when the endpoint and network are under your control. Never send a real
API key over HTTP; use an HTTPS endpoint for a remote or credentialed service.
This is an operator requirement: current URL validation does not enforce
locality. See the [transport-policy gap](implementation/model-runtime.md#implementation-gap-http-transport-policy).

### External DeepSeek

The checked-in DeepSeek profile uses:

```json
{
  "runtime": {
    "backend": "deepseek",
    "mode": "external"
  },
  "model": {
    "base_url": "https://api.deepseek.com",
    "api_key_env": "DEEPSEEK_API_KEY",
    "id": "deepseek-flash"
  }
}
```

Do not add a `runtime.config` object for DeepSeek. ARIES performs model
preflight and configures OpenClaw, but it does not manage the remote service.

The preferred source for `./bin/aries` is the ignored repository-root file
`DEEPSEEK_API.key`:

```sh
umask 077
read -r -s -p "DeepSeek API key: " aries_key; printf '\n'
printf '%s\n' "$aries_key" > DEEPSEEK_API.key
unset aries_key
chmod 600 DEEPSEEK_API.key
```

The file must be a current-user-owned, regular, non-symlink file with owner
read access, no group or world permissions, and one nonempty line. Modes `0400`
and `0600` are both valid. ARIES never writes the value to JSON, logs, Docker
metadata, or results.

If that repository-local file is unavailable, including when the binary is
installed elsewhere, ARIES reads `DEEPSEEK_API_KEY` from the environment. If a
repository-local file exists but is invalid, ARIES fails closed rather than
falling back.

This file convenience is not limited to a DeepSeek-backed harness: it also
applies whenever a benchmark's `judge` block is genuinely official DeepSeek
(`provider: "deepseek"`, `base_url: "https://api.deepseek.com"`, and a
supported model), even if `runtime.backend` is `sglang` — e.g. SWE-Atlas QA's
`profiles/openclaw-sweatlasqa-smoke1-sglang.json`, which runs its agent
against a local SGLang server but grades with a DeepSeek judge. Any other
credential (SGLang's own `api_key_env`, for instance) is always read from the
environment regardless.

Use the model ID in the checked-in profile or one accepted by the selected
backend and returned by its catalog. Preflight requires an exact match; a model
alias is not inferred from a similar name. The [DeepSeek preflight](../internal/app/preflight.go)
defines ARIES's accepted IDs. Provider availability is checked at run time.

### External SGLang

External SGLang is started and configured by the user. The checked-in profile
references `configs/sglang/qwen3-8b-local.yaml`, which can be passed to SGLang's
native `--config` option. ARIES does not read that file in external mode; it
checks the configured endpoint and served model. Native YAML and GPU topology
validation apply only to [managed SGLang](#managed-sglang).

Copy the profile before changing its endpoint:

```sh
mkdir -p .cache
cp profiles/openclaw-tb2-fix-git-sglang.json \
  .cache/openclaw-tb2-fix-git-sglang.json
```

Replace `model.base_url` in the copy with an endpoint ending exactly in `/v1`.
The checked-in `sglang.local` hostname is a placeholder: the configured
hostname or address must resolve and be reachable from both the ARIES host and
OpenClaw containers. Use HTTP only for a trusted local endpoint with the
non-secret placeholder below; use HTTPS for a remote or credentialed service.

The checked-in profile uses the following external runtime and model settings:

```json
{
  "runtime": {
    "backend": "sglang",
    "mode": "external",
    "config": {
      "file": "../configs/sglang/qwen3-8b-local.yaml"
    }
  },
  "model": {
    "base_url": "http://sglang.local:30000/v1",
    "api_key_env": "SGLANG_API_KEY",
    "id": "Qwen/Qwen3-8B"
  }
}
```

External SGLang mode needs no `runtime.config`. An optional `config.file` is
accepted for existing profiles but is not read or validated; `executable`,
`startup_timeout`, and `stop_timeout` are rejected because ARIES does not own
that process. Start SGLang separately; for example, this exposes GPU0 to the
server:

```sh
CUDA_VISIBLE_DEVICES=0 /absolute/path/to/venv/bin/python \
  -m sglang.launch_server \
  --config configs/sglang/qwen3-8b-local.yaml
```

In the shell that runs ARIES, set the environment variable named by
`model.api_key_env`. An unauthenticated endpoint still needs a nonempty
placeholder because OpenClaw and model preflight require the configured
credential:

```sh
export SGLANG_API_KEY=unused-local-token
./bin/aries .cache/openclaw-tb2-fix-git-sglang.json
```

### External OpenAI-compatible server

`runtime.backend: "openai"` accepts any server that speaks the OpenAI chat
completions API and lists its models at `/v1/models`: vLLM, `llama.cpp`, a
gateway, or a hosted endpoint. ARIES never starts, configures, or stops the
server. Before the run it makes one bounded `/v1/models` request and confirms
that `model.id` is served. `runtime.config` must be omitted.

The checked-in profile targets a vLLM server:

```json
{
  "runtime": {
    "backend": "openai",
    "mode": "external"
  },
  "model": {
    "base_url": "http://vllm.local:8000/v1",
    "api_key_env": "VLLM_API_KEY",
    "id": "Qwen/Qwen3.6-35B-A3B-FP8"
  }
}
```

Create a local copy:

```sh
mkdir -p .cache
cp profiles/hermes-tb2-fix-git-vllm.json .cache/hermes-tb2-fix-git-vllm.json
```

In the copy, set `model.base_url` to an endpoint that ends exactly in
`/v1` and `model.id` to the name the server reports. The checked-in
`http://vllm.local:8000/v1` value is a trusted-local placeholder; the address
must resolve from the ARIES host and from the harness containers. Use HTTPS for
a remote or credentialed server. Start the server yourself, for example:

```sh
vllm serve Qwen/Qwen3.6-35B-A3B-FP8 --port 8000 \
  --served-model-name Qwen/Qwen3.6-35B-A3B-FP8
```

For this trusted-local HTTP example, use the non-secret placeholder credential:

```sh
export VLLM_API_KEY=unused-local-token
./bin/aries .cache/hermes-tb2-fix-git-vllm.json
```

Hermes has no `sglang` or plain `openai` provider, so for both backends ARIES
renders Hermes's generic `custom` provider, which routes to `model.base_url`.
OpenClaw receives the server as a `models.providers` entry named `aries`.

### Managed SGLang

To let ARIES own one SGLang process for the entire profile run, use the
following runtime and model settings in the copied profile:

```json
{
  "runtime": {
    "backend": "sglang",
    "mode": "managed",
    "config": {
      "file": "../configs/sglang/qwen3-8b-local.yaml",
      "executable": "/absolute/path/to/venv/bin/python",
      "startup_timeout": "15m",
      "stop_timeout": "1m",
      "gpu_indices": [0]
    }
  },
  "model": {
    "base_url": "http://sglang.local:30000/v1",
    "api_key_env": "SGLANG_API_KEY",
    "id": "Qwen/Qwen3-8B"
  }
}
```

`file`, `executable`, `startup_timeout`, and `stop_timeout` are required. `file` is resolved
relative to the profile, `executable` must identify the Python executable from
the SGLang environment, and both timeout values must be positive Go durations.
`model.base_url` must use the YAML port and end exactly in `/v1`;
`model.id` must equal the YAML `served-model-name`; and `model.api_key_env`
names the environment variable read by ARIES and rendered into OpenClaw.

`runtime.config.gpu_indices` is optional and valid only for managed SGLang.
For YAML `device: cuda`, ARIES derives the number of local workers from the
tensor, pipeline, and multi-node topology and selects physical devices
`[0, ..., N-1]` when the field is omitted.
Ordinary data parallelism replicates workers; DP attention, expert, MoE data,
and attention-context parallelism partition the TP workers instead. An
explicit list must contain exactly `N` unique, non-negative indices. ARIES uses
the resolved list for both the child's `CUDA_VISIBLE_DEVICES` and NVIDIA
sampling. An unsupported or inconsistent topology fails before runtime or
monitor side effects.

Do not start `sglang.launch_server` separately in this mode. ARIES passes the
referenced YAML using the exact arguments
`-m sglang.launch_server --config <file>`, waits up to `startup_timeout` for
`/health`, and then requires exact model discovery at `/v1/models`. It retains
the child output in mode-0600 `sglang/stdout.log` and `sglang/stderr.log`, stops
the process group after all admitted tasks drain, and uses `stop_timeout` as
the graceful TERM budget before forced cleanup.

The configured credential variable is available to ARIES and OpenClaw but is
removed from the managed SGLang child's environment:

```sh
export SGLANG_API_KEY=unused-local-token
./bin/aries .cache/openclaw-tb2-fix-git-sglang.json
```

ARIES does not install SGLang or models or configure the network path shared by
the host and containers.

## Harness settings

The Hermes profile is the same run with a different harness, so it needs the
[DeepSeek credential](#external-deepseek) and no extra setup:

```sh
./bin/aries profiles/hermes-tb2-fix-git-deepseek.json
```

Hermes supports text and [voice-transcribe](voice_mode.md). It runs the pinned
upstream image unmodified and is paired
with `bridge.type: "hermes-ssh"`; the two values must match, and a crossed pair
is rejected before the run starts. Hermes issues every tool call as `bash -c`,
so the task image must provide `/bin/bash`. Its `~/.hermes` file sync is refused by
the bridge to keep the evaluated sandbox free of harness scaffold and
credentials; Hermes logs one `file_sync: sync failed` warning and continues.

Artifacts land under `<run>/<task>/harness/`: the redacted `config.yaml`, the
one-shot's `hermes_stdout.log` and `hermes_stderr.log`, `container.log`, and the
exported message-level trajectory at `telemetry/sessions.jsonl`.

### Hermes context window, compaction, and request extra body

Three optional profile blocks reach the rendered Hermes `config.yaml`. Each is
Hermes-only and is rejected under another harness. A profile without them
renders the same file as before.

- `model.context_length`, `model.max_tokens`, and `model.temperature` set the
  window Hermes's compressor reasons about and the request sampling.
  Temperature (including `0.0`) requires the `sglang` or `openai` backend;
  ARIES places it in the custom provider's request `extra_body`, because the
  pinned one-shot path ignores Hermes's `model.temperature` YAML field.
  Setting both `model.temperature` and `harness.hermes.extra_body.temperature`
  is rejected. Native DeepSeek temperature is unsupported by this path.
- `harness.compaction.threshold_tokens` is an absolute compaction trigger.
  Hermes applies it after its 64K minimum and its 75% floor for windows under
  512K, so it is the one knob that gives an exact trigger on a large window.
  `harness.compaction.enabled: false` turns compaction off.
- `harness.hermes.extra_body` is a non-empty JSON object. ARIES writes it as
  the `extra_body` of one `custom_providers` entry, and Hermes merges it into
  every chat request. Hermes performs that merge only for its `custom`
  provider, so the block requires the `sglang` or `openai` backend. It sits
  under `harness.hermes` because it is a Hermes escape hatch with no meaning
  for another harness, whereas compaction is a general harness setting.

Hermes expands `${NAME}` references in its configuration from the container
environment. ARIES exports `ARIES_RUN_ID` and `ARIES_TASK_ID` into the Hermes
container, and those two are the only references `harness.hermes.extra_body`
may carry. The object is also rejected when any field at any depth is named
like a credential, such as `api_key`, `authorization`, or `token`: it is
written into the retained `config.yaml` and sent with every request, and model
keys stay out of JSON profiles. Its checked-in HTTP endpoint is intended only
for trusted local use with the non-secret placeholder shown below; use HTTPS
for a remote or credentialed server. The checked-in profile compacts at 65,536
tokens and tags every request with the task through the OpenAI `user` field:

```json
{
  "harness": {
    "type": "hermes",
    "compaction": {
      "enabled": true,
      "threshold_tokens": 65536
    },
    "hermes": {
      "extra_body": {
        "chat_template_kwargs": {
          "preserve_thinking": true
        },
        "user": "${ARIES_RUN_ID}-${ARIES_TASK_ID}"
      }
    }
  },
  "model": {
    "base_url": "http://vllm.local:8000/v1",
    "api_key_env": "VLLM_API_KEY",
    "id": "Qwen/Qwen3.6-35B-A3B-FP8",
    "context_length": 262144,
    "max_tokens": 32768,
    "temperature": 1.0
  }
}
```

```sh
export VLLM_API_KEY=unused-local-token
./bin/aries profiles/hermes-tb2-fix-git-vllm-compaction.json
```

`compression.threshold_tokens` exists since Hermes v2026.8, so the pinned image
moves to `v2026.8.31`. Profiles that omit that absolute compaction cap remain
compatible with the previously pinned `v2026.5.29.2`: that release accepts
`model.context_length`, `model.max_tokens`, and custom-provider `extra_body`
(including the request-level temperature path), but silently ignores
`compression.threshold_tokens`. The rendered `config.yaml` under
`<run>/<task>/harness/` shows the block exactly as Hermes reads it. The
`agent.max_turns` value in that
file does not bound the one-shot; use `agent_timeout_seconds` in the overrides
file to bound a run.

### Realtime OpenClaw mode

OpenClaw uses text-agent mode when `harness.mode` is omitted. Set
`harness.mode: "realtime"` to deliver the same task instruction through a
realtime voice session. The checked-in realtime profile does this already.
Keep the configured DeepSeek key file from the text setup and provide the
separate TTS credential named by the realtime profile through a secret manager
or interactive shell, then verify it is nonempty:

```sh
export OPENAI_API_KEY
test -n "${OPENAI_API_KEY:-}"
./bin/aries profiles/openclaw-tb2-fix-git-realtime-deepseek.json
```

This run can incur charges from both the configured model provider and the TTS
provider. The realtime profile keeps model and TTS credentials separate;
neither belongs in profile JSON.

## Benchmark settings

`benchmark.type`, `root`, and `tasks` select the benchmark, checkout, and task
occurrences. Pins come from `versions_file`. Keep evaluator-specific settings in
these guides:

- Terminal-Bench 2 uses pinned task metadata for images and verifier budgets;
  see [overrides](#resource-and-timeout-overrides).
- [Deep Research Bench](benchmarks/deep-research-bench.md) requires an environment
  image with its research services. An omitted `judge` reuses the harness model;
  optional `fact` configures citation checking.
- [SWE-Atlas QA](benchmarks/swe-atlas-qa.md) requires a `judge` block, which can
  explicitly disable grading. It rejects `environment` and `fact`.
- [SWE-bench Pro](benchmarks/swe-bench-pro.md) derives environments from dataset
  rows and uses its pinned evaluator. It rejects `environment`, `judge`, and `fact`.

## Other harness and evidence options

- `harness.web_search` and `harness.subagents`: see the
  [research benchmark guide](benchmarks/deep-research-bench.md#web-search-and-fetch).
- `harness.mcp_servers`: see [MCP configuration and boundaries](implementation/harnesses.md#model-context-protocol-mcp).
- `harness.voice_transcribe`: see the [voice guide](voice_mode.md).
- `bridge.retain_raw_log`: defaults to false. Enabling it retains sensitive,
  replayable SSH input; see [run artifacts](run-results.md#bridge-evidence).

