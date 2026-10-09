# Run results and troubleshooting

Use this guide after the [quick start](quick-start.md) or a run configured with
the [configuration reference](configuration.md). Artifacts are private and may
contain task or model content.

## Inspect a run

For the one-task profile:

```sh
run_dir="$(ls -1dt runs/*-openclaw-tb2-fix-git-deepseek | head -1)"
cat "$run_dir/live-validation.json"
cat "$run_dir/run-result.json"
task_id="$(jq -er '.tasks[0].task_id' "$run_dir/run-result.json")"
task_dir="$run_dir/$task_id"
cat "$task_dir/evaluation/reward.txt"
cat "$task_dir/bridge/tool-calls.jsonl"
test ! -f "$task_dir/bridge/ssh_raw.log" || cat "$task_dir/bridge/ssh_raw.log"
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
- successful harness, confirmed isolation, successful evaluation and cleanup
  outcomes in `run-result.json`; inspect the independent observer status too
  (a disabled observer reports `not_enabled`);
- reward `1`; and
- completed tool calls in `bridge/tool-calls.jsonl`.

## Hermes Gateway artifacts

Hermes text and voice-transcribe now submit tasks through the native Gateway.
Historical CLI artifacts map as follows:

| Historical CLI artifact | Gateway artifact and meaning |
| --- | --- |
| `hermes_stdout.log` | `gateway-output.txt`: native run `output`, rather than process standard output. |
| `hermes_stderr.log` | Removed; service diagnostics are retained in bounded, redacted `container.log`. |
| `session-outcome.json` with `exit_code` | Same filename, with `run_id`, `session_id`, native `end_reason`, and ARIES `status`; no CLI exit code. Timing covers submission through terminal observation, separately from voice preparation. |
| `telemetry/sessions.jsonl` | Retained native message-level session export. |
| `telemetry/otel-spans.jsonl` | Tool, model, and API timing from the pinned `hermes-otel` plugin; see [collection limits](implementation/harnesses.md#hermes). |
| `config.yaml`, voice artifacts | Retained private configuration, audio, transcript, and voice result. |

A completed native run is a harness outcome. Check isolation, evaluation, and
cleanup separately in `run-result.json`. Historical CLI runs used
`--ignore-rules`; the [compatibility note](implementation/harnesses.md#pinned-gateway-compatibility)
describes the Gateway differences relevant to comparisons.

## Bridge evidence

`bridge/ssh_raw.log` is an opt-in mode-0600 sensitive audit. It is written only
when a profile sets `bridge.retain_raw_log` to `true`; an omitted or `false`
value drops it, so the file is absent by default. When retained it contains
lossless, human-readable text records between full-line
`--- ARIES SSH CALL BEGIN ---` and `--- ARIES SSH CALL END ---` delimiters.
Fixed-order `key=value` lines include the decoded wire command when available,
exact payload and stdin byte counts, and escaped exact payload/stdin. Printable
UTF-8 appears literally; backslash, newline, carriage return, tab, other
controls, and invalid UTF-8 use explicit escapes. The file is neither JSON nor
base64. It may contain exact wire-supplied values; keep the run directory
private and do not publish this artifact without review.

With `bridge.sandbox_lifecycle: "checkpoint"`, `checkpoint/events.jsonl` records
each sandbox checkpoint and restore as one JSON line, with its sequence, start
timestamp, `duration_ns`, whether access was granted, status, and error. Tool-call
`duration_ms` includes restore time; checkpoint time appears only in this file.

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

- **Docker permission or socket error:** run `docker info` against the configured
  daemon. ARIES requires a local Unix socket; the default is `/var/run/docker.sock`.
- **Missing `aries-ssh` error:** rebuild with `make build` and keep the helper
  beside the main binary.
- **Credential error:** check ownership, owner read access, absence of group or
  world permissions, and one-line formatting of `DEEPSEEK_API.key`.
- **Model error:** inspect `live-validation.json` for authentication, rate
  limit, connectivity, or missing-model categories.
- **Realtime TTS error:** confirm `OPENAI_API_KEY` is set and that the provider,
  model, and voice in `harness.realtime.tts` are available to that account.
- **OpenClaw Gateway or realtime session error:** inspect the task's
  `harness/gateway.log`, `harness/realtime-result.json`, and
  `harness/telemetry.index.json`, then correlate the harness status in the run's
  `run-result.json`. Keep these private artifacts out of issue reports unless
  their task and model content has been reviewed.
- **Hermes Gateway error:** inspect `harness/session-outcome.json`,
  `harness/container.log`, and `harness/gateway-output.txt`, then compare the
  independent harness, isolation, evaluation, and cleanup outcomes.
- **SGLang configuration error:** confirm that the YAML uses only the supported
  fields and that its served model and port match the profile in managed mode.
  External mode does not validate the native YAML file.
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
