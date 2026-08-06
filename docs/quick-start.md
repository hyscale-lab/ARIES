# Quick start: OpenClaw + Terminal-Bench 2

This guide runs a checked-in Terminal-Bench 2 experiment from a clean ARIES
clone with either DeepSeek or SGLang. Run every command from the repository
root.

## 1. Prerequisites

- Linux with a running local Docker Engine and access to `/var/run/docker.sock`.
- Go 1.26.5, Git, and Make. Go's toolchain selection can download 1.26.5 when
  an older Go launcher is installed.
- Network access to GitHub, GHCR, and Docker Hub.
- For DeepSeek, access to `https://api.deepseek.com` and an API key for the
  model named in the profile.
- For SGLang, an installed Python environment containing SGLang, a compatible
  local model, and enough visible GPU memory.

Verify Docker before continuing:

```sh
cd ~/projects/aries
docker info >/dev/null
```

ARIES currently validates the host bridge path on native Linux Docker. Docker
Desktop and rootless networking are not yet supported configurations.

## 2. Choose a profile

Build ARIES once, then choose the one-task DeepSeek profile for the quickest
first run, the equivalent one-task SGLang profile for local serving, or the
heterogeneous five-task DeepSeek profile:

```sh
make build
```

- `profiles/openclaw-tb2-fix-git-deepseek.json`
- `profiles/openclaw-tb2-fix-git-sglang.json`
- `profiles/openclaw-tb2-five-deepseek.json`
- `profiles/hermes-tb2-fix-git-deepseek.json`

Running a profile automatically loads `configs/versions.json`, creates or
verifies the pinned Terminal-Bench checkout at `.cache/terminal-bench-2`, reads
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

The five-task profile additionally references
`configs/runtime-overrides.json` relative to the profile. Its sparse
`harness_resources` and `agent_sandbox_resources` blocks apply only to their
respective containers and may specify different CPU and memory limits. An
omitted harness dimension stays unlimited; an omitted sandbox dimension keeps
the value in the task's `task.toml`. Neither block inherits from the other.
The independent `agent_timeout_seconds` field changes only the agent deadline.
Every checked-in profile explicitly contains `overrides_file`; the one-task
profile uses `""`, which disables override loading without opening a file.
Profiles and nonempty referenced override files reject unknown fields and
trailing JSON; there is no profile merge or inheritance layer. SGLang is the
exception only for its separate native launch configuration, described below.

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

## 3. Configure the model backend

`runtime.backend` selects the provider, while `runtime.mode` states whether
ARIES owns a model-server process. The supported combinations are:

| Backend | Mode | `runtime.config` | Process owner |
| --- | --- | --- | --- |
| `deepseek` | `external` | Must be omitted | DeepSeek |
| `sglang` | `external` | `file` only | User |
| `sglang` | `managed` | `file`, `executable`, `startup_timeout`, `stop_timeout` | ARIES |

DeepSeek cannot use managed mode. SGLang supports both modes.

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
    "id": "deepseek-v4-flash"
  }
}
```

Do not add a `runtime.config` object for DeepSeek. ARIES performs model
preflight and configures OpenClaw, but it does not manage the remote service.

The preferred source for `./bin/aries` is the ignored repository-root file
`DEEPSEEK_API.key`:

```sh
echo 'api_key' > DEEPSEEK_API.key
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

### External SGLang

The checked-in SGLang profile keeps experiment configuration in JSON and
references the reusable native YAML at
`configs/sglang/qwen3-8b-local.yaml`. SGLang reads this file through its native
`--config` option. Before launch, ARIES performs a bounded preflight over the
fields needed to check the served model, endpoint port, and local GPU topology.
A mismatch with the profile or an invalid GPU topology is rejected before the
run starts; inference settings remain owned and interpreted by SGLang.

Copy the profile before changing its endpoint:

```sh
cp profiles/openclaw-tb2-fix-git-sglang.json \
  .cache/openclaw-tb2-fix-git-sglang.json
```

Replace `model.base_url` in the copy with an HTTP endpoint ending exactly in
`/v1`. The checked-in `sglang.local` hostname is a placeholder: the configured
hostname or address must resolve and be reachable from both the ARIES host and
OpenClaw containers.

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

External SGLang mode accepts only `runtime.config.file`; `executable`,
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

All four managed `runtime.config` fields are required. `file` is resolved
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

## 4. Run the experiment

For the one-task example:

```sh
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

For the five-task subset:

```sh
./bin/aries profiles/openclaw-tb2-five-deepseek.json
```

For SGLang, use the external or managed command from the previous section.
Keep `bin/aries-ssh` beside `bin/aries`. A live DeepSeek run can incur API
charges; the five-task profile also takes substantially longer and pulls more
images. ARIES first ensures the benchmark and images are prepared, then starts
and checks an owned managed runtime when configured, performs a bounded model
preflight, and runs each task in profile order. For each task it stops OpenClaw,
revokes SSH access, and only then evaluates the same still-running sandbox.

### Hermes instead of OpenClaw

The Hermes profile is the same run with a different harness, so it needs the
same DeepSeek credential and no extra setup:

```sh
./bin/aries profiles/hermes-tb2-fix-git-deepseek.json
```

Hermes is text only. It runs the pinned upstream image unmodified and is paired
with `bridge.type: "hermes-ssh"`; the two values must match, and a crossed pair
is rejected before the run starts. Hermes issues every tool call as `bash -c`,
so the task image must provide `/bin/bash`. Its `~/.hermes` file sync is refused by
the bridge to keep the evaluated sandbox free of harness scaffold and
credentials; Hermes logs one `file_sync: sync failed` warning and continues.

Artifacts land under `<run>/<task>/harness/`: the redacted `config.yaml`, the
one-shot's `hermes_stdout.log` and `hermes_stderr.log`, `container.log`, and the
exported message-level trajectory at `telemetry/sessions.jsonl`.

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

## 5. Check the result

For the one-task profile:

```sh
run_dir="$(ls -1dt runs/*-openclaw-tb2-fix-git-deepseek | head -1)"
cat "$run_dir/live-validation.json"
cat "$run_dir/run-result.json"
task_id="$(jq -er '.tasks[0].task_id' "$run_dir/run-result.json")"
task_dir="$run_dir/$task_id"
cat "$task_dir/evaluation/reward.txt"
cat "$task_dir/bridge/tool-calls.jsonl"
cat "$task_dir/bridge/ssh_raw.log"
cat "$task_dir/harness/openclaw.json"
cat "$run_dir/aries.log"
```

For the SGLang example, select its run and inspect the managed runtime logs
when applicable:

```sh
run_dir="$(ls -1dt runs/*-openclaw-tb2-fix-git-sglang | head -1)"
cat "$run_dir/live-validation.json"
cat "$run_dir/run-result.json"
task_id="$(jq -er '.tasks[0].task_id' "$run_dir/run-result.json")"
task_dir="$run_dir/$task_id"
cat "$run_dir/aries.log"
jq 'select(.component == "gpu")' "$task_dir/monitor/resources.jsonl"
cat "$task_dir/monitor/index.json"
test ! -d "$run_dir/sglang" || ls -l "$run_dir/sglang"
```

For a multi-task profile, `run-result.json` contains one result per task and
each task has its own readable directory:

```sh
run_dir="$(ls -1dt runs/*-openclaw-tb2-five-deepseek | head -1)"
find "$run_dir" -maxdepth 2 -type d | sort
find "$run_dir" -path '*/evaluation/reward.txt' -print -exec cat {} \;
```

A successful task has:

- successful model validation;
- separate successful harness, isolation, evaluation, observer, and cleanup
  outcomes in `run-result.json`;
- reward `1`; and
- completed tool calls in `bridge/tool-calls.jsonl`.

`bridge/ssh_raw.log` is an unconditional mode-0600 sensitive audit containing
lossless, human-readable text records between full-line
`--- ARIES SSH CALL BEGIN ---` and `--- ARIES SSH CALL END ---` delimiters.
Fixed-order `key=value` lines include the decoded wire command when available,
exact payload and stdin byte counts, and escaped exact payload/stdin. Printable
UTF-8 appears literally; backslash, newline, carriage return, tab, other
controls, and invalid UTF-8 use explicit escapes. The file is neither JSON nor
base64. It may contain exact wire-supplied values; keep the run directory
private and do not publish this artifact without review.

`bridge/tool-calls.jsonl` remains valid line-delimited JSON. Printable Unicode
and HTML characters such as `&&`, `<`, and `>` appear literally, while quotes,
backslashes, and newlines retain required JSON escaping. Printable stdin stays
inline; binary or control-bearing stdin is replaced by a concise
`binary-omitted` marker and exact byte count, with lossless bytes retained only
in `ssh_raw.log`. Structured lifecycle logs and tool-call records continue to
omit environment values and stdout/stderr bodies.

Each task directory contains the exact placeholder-only rendered
`harness/openclaw.json`, OpenClaw logs and telemetry when available, replayable
SSH tool inputs, Docker sandbox logs, one-second CPU and memory samples,
verifier stdout/stderr, and CTRF output. `aries.log` is the structured Logrus
run log.

Text mode writes `harness/agent-result.json`. Realtime mode instead writes the
private `harness/voice-instruction.txt`, `harness/voice-instruction.wav`, its
metadata, and `harness/realtime-result.json`. These mode-specific harness and
bridge artifacts may contain task or model content; review them before sharing.

## Troubleshooting

- **Docker permission or socket error:** run `docker info`; ARIES uses the local
  daemon at `/var/run/docker.sock`.
- **Missing `aries-ssh` error:** rebuild with `make build` and keep the helper
  beside the main binary.
- **Credential error:** check ownership, owner read access, absence of group or
  world permissions, and one-line formatting of `DEEPSEEK_API.key`.
- **Model error:** inspect `live-validation.json` for authentication, rate
  limit, connectivity, or missing-model categories.
- **Realtime TTS error:** confirm `OPENAI_API_KEY` is set and that the provider,
  model, and voice in `harness.realtime.tts` are available to that account.
- **Gateway or realtime session error:** inspect the task's
  `harness/gateway.log`, `harness/realtime-result.json`, and
  `harness/telemetry.index.json`, then correlate the harness status in the run's
  `run-result.json`. Keep these private artifacts out of issue reports unless
  their task and model content has been reviewed.
- **SGLang configuration error:** confirm that the YAML uses only the supported
  fields and that its served model and port match the profile.
- **SGLang readiness error:** inspect `sglang/stderr.log`, confirm that
  `model.base_url` ends in `/v1`, and test `/health` and `/v1/models` from the
  host. A server reachable only through loopback is not reachable from
  OpenClaw's container.
- **Managed SGLang exits early:** confirm that `runtime.config.executable` is
  the Python executable from the SGLang environment and that the selected GPU
  has enough free memory.
- **GPU monitor error:** confirm `nvidia-smi` is available and every configured
  `runtime.config.gpu_indices` entry exists. GPU indices must be unique and
  non-negative.
- **Unknown task or invalid task image:** choose a task directory from the
  pinned checkout and ensure its `task.toml` declares a valid explicit image
  tag. Digest-bearing or implicit-`latest` task references are rejected.
- **Terminal-Bench revision mismatch:** move the stale checkout aside, then
  rerun the profile command (or the optional setup prewarm). ARIES never
  deletes it automatically.
- **SSH timeout:** check host firewall rules and confirm containers can reach
  the Docker bridge gateway.
- **Suspected leak:** inspect `docker ps -a --filter label=aries.managed=true`
  and `docker network ls --filter label=aries.managed=true`.

For architecture and security boundaries, see [the architecture guide](design.md).
For the exact implementation matrix and configuration pointers, see
[supported implementations](supported.md).
