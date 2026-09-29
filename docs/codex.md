# Codex with a Responses API model server

ARIES supports the unmodified Codex CLI `0.157.1` as a text harness paired
with `codex-ssh`. This version's experimental native remote executor carries
shell and filesystem tools over SSH into the task container. The harness and
evaluator therefore operate on the same task files, with the usual harness
stop and bridge revocation gates before evaluation.

The [RoadmapBench adapter](benchmarks/roadmapbench.md) has a checked-in Codex
profile. A particular Qwen checkpoint is usable only when its serving
stack implements the Responses API and the native tool-call formats expected
by this Codex version; no live Qwen result is implied by the deterministic
integration test.

## Prepare the pinned CLI

Linux, Docker, and the normal ARIES build prerequisites are required. Use the
official static musl binary matching both the host and task-image architecture.
For `linux/amd64`, run from the ARIES repository:

```sh
mkdir -p .cache/codex/0.157.1
curl -fL --retry 3 \
  https://github.com/openai/codex/releases/download/rust-v0.157.1/codex-x86_64-unknown-linux-musl.tar.gz \
  -o .cache/codex/0.157.1/release.tar.gz
printf '%s  %s\n' \
  e98c1e8e028e8137fa2d2415c82ec58e7b3701a627e3554aace5b3ca31454af2 \
  .cache/codex/0.157.1/release.tar.gz | sha256sum -c -
tar -xzf .cache/codex/0.157.1/release.tar.gz -C .cache/codex/0.157.1
mv .cache/codex/0.157.1/codex-x86_64-unknown-linux-musl .cache/codex/0.157.1/codex
make build
```

The checksum above identifies the archive used for validation. ARIES rejects
scripts, dynamic executables, symlinks, and a staged CLI that reports a
different version. `make build` also builds the static `aries-codex-ssh`
client and `aries-codex-exec` supervisor; keep both beside `bin/aries`.
The version and base image are declared in `configs/versions.json`.
The default Debian slim image supports local HTTP endpoints. For HTTPS, select
a pinned harness image containing the required CA certificates; ARIES does not
install packages into that image or disable TLS verification.

## Configure and run

Edit `profiles/codex-tb2-fix-git-openai.json`:

- Set `model.base_url` to a `/v1` endpoint reachable from the harness container.
  Host-loopback `127.0.0.1` refers to the container itself.
- Set `model.id` to the exact served model ID and export the credential under
  `model.api_key_env` (`MODEL_API_KEY` in the example). For an unauthenticated
  local server, use a non-secret placeholder if the server accepts one.
- Keep `harness.type: "codex"` paired with `bridge.type: "codex-ssh"`.
  `harness.codex.executable` is relative to the profile file. Optionally set
  `model.context_length` to the model's actual window.

Use `runtime.backend: "openai"` for an external server, or `"sglang"` for
the existing SGLang runtime path. Both require streaming `/v1/responses` with
tool calls. Model discovery checks `/v1/models`; it does not test Responses
compatibility. This integration has no Chat Completions translation and
rejects the native DeepSeek backend, `max_tokens`, `temperature`, voice,
dedicated web-search tools, and compaction overrides.

`harness.codex.reasoning_effort` sets the native Responses reasoning effort
(`none`, `minimal`, `low`, `medium`, `high`, or `xhigh`); the model server must
support the selected value. `harness.codex.developer_instructions` adds
experiment instructions without changing the benchmark question.
`harness.subagents.enabled` defaults to true, and `max_concurrent` limits
concurrent child threads, excluding the parent. Native children share the
parent's SSH environment and inherit its model and reasoning settings when
the spawn call omits overrides. For models absent from Codex's built-in
catalog, explicitly selecting a child model or effort can fail validation;
omit `model`, `reasoning_effort`, and `agent_type` in those spawn calls.

`profiles/codex-drb-qwen38-27b-xhigh-all100.json` collects all 100
DeepResearchBench reports with `xhigh`, up to three native subagents, and
RACE/FACT disabled. Set its endpoint to the actual model service, keep its
context length aligned with the server, and use its one-hour task budget
from `configs/runtime-overrides.json`. Research uses the task container's
SearXNG endpoint and shell HTTP clients; no paid search or scoring key is
needed. Validate one task before starting the full collection.

```sh
./bin/aries setup profiles/codex-tb2-fix-git-openai.json
./bin/aries profiles/codex-tb2-fix-git-openai.json
```

`setup` prepares the benchmark and container images without contacting the
model. During a run, ARIES keeps the model credential in the harness only;
task commands do not receive it. The task image must support the selected
Linux architecture and provide `/bin/sh`, `mkdir`, `chmod`, and `rm` for bridge
staging and cleanup. Native command execution also needs the requested shell.
The bridge stages about 300 MiB of binaries per active task.

Native commands inherit the task image's environment, including toolchain
variables such as `CARGO_HOME` and `RUSTUP_HOME`. ARIES disables login shells
so shell startup does not replace the image's `PATH`; omit `login` or set it
to false in native tool calls. The model-key variable remains explicitly
excluded from both parent and child task commands.

## Evidence and validation

Private harness artifacts include `config.toml` and `environments.toml`,
together with these run outputs:

| Artifact | Meaning |
| --- | --- |
| `trajectory.jsonl` | Native `codex exec --json` stdout, unchanged after credential redaction. |
| `stderr.log` | Credential-redacted native stderr. |
| `agent-result.json` | Final assistant `response` and optional `error`, matching the corresponding OpenClaw output fields. |
| `session-outcome.json` | Harness `status`, `end_reason`, `exit_code`, `started_at`, `ended_at`, and `duration_ms`, matching Hermes's terminal outcome fields. |
| `telemetry.index.json` | Relative paths to the retained telemetry files. |
| `telemetry/events.jsonl` | Native JSON events wrapped with an ordered `sequence` and host-receipt `timestamp`. |
| `telemetry/native-trace.jsonl` | Native trace timestamps and selected structural metadata, excluding payload bodies, source, and arguments. |
| `telemetry/llm-calls.jsonl` | Logical inference intervals derived from the pinned CLI's native trace. |

The event recorder writes each complete stdout event as it arrives. Its
host-receipt timestamp describes observation by ARIES, not the time of the
underlying model or tool action. JSON string values and keys are decoded
before credential redaction, including escaped credentials. Malformed,
oversized, or truncated event streams produce an artifact-collection error;
they are not converted into successful completion events. The terminal
outcome separately distinguishes completion, nonzero exit, execution error,
cancellation, and deadline expiry. Harness outcome remains independent of
benchmark evaluation.

LLM intervals use the native trace's UTC event timestamps for logical
inference operations, observed after native payload serialization. These
intervals can include retries and streaming; they are not per-HTTP-attempt
measurements and do not provide time to first token. Parent and child thread
identity is retained where the native trace supplies it. Inference statuses
are `completed`, `failed`, `cancelled`, or `incomplete`. Missing terminal
observations remain incomplete without an invented `ended_at` or
`duration_ms`. Native stdout and normalized telemetry remain separate
artifacts; trace request/response payload bodies are not exported.

Native tracing still writes request/response payload files transiently inside
the harness container. Repeated long contexts can make those files large;
metadata-only export does not eliminate this temporary disk overhead. Only
`trace.jsonl` is collected and reduced to the documented telemetry fields.
Normal harness removal deletes the transient payload files with the container.
On an interrupted run, bundle discovery is bounded to two seconds; the
harness then verifies ownership and confirms the container has stopped before
copying the trace. The normal Stop gate still confirms removal before evaluation.

`bridge/tool-calls.jsonl` records each observed native `process/start` through
its exit notification, even when hundreds of commands share one SSH executor
session. Process records retain `thread_id`, `tool_call_id`, `process_id`,
`request_id`, argv, workdir, output byte counts, exit status, `started_at`,
`finished_at`, and `duration_ms`. These are host-side bridge observation
times, not task-kernel process timestamps. `timestamp` is the audit enqueue
time; `process_id` is a native logical identifier, not an operating-system
PID. A confirmed exit has status `completed`, including nonzero exits; a
rejected start has status `start_failed`. A missing exit notification leaves
status `incomplete` or `canceled`, exit code -1, and no `finished_at`;
`duration_ms` then ends at the last observation of the stream. The outer
executor session remains a separate record; filter by
`operation_class: "exec"` when counting native process executions. Native agent coordination
or filesystem tools are not automatically equivalent to a process execution.

Bridge artifacts also contain replayable native RPC inputs and must remain
private. Revocation removes the task-side binaries, the host SSH identity,
and the staged helper; the public known-hosts entry may remain as evidence.
Timing capture applies to newly started runs; prior runs cannot recover
missing per-call timestamps from their final stdout or executor-session log.

With the pinned CLI installed, `make integration` includes a real Codex
container, a deterministic local Responses endpoint, and the actual native
SSH executor. Missing CLI prerequisites are reported as a skipped test.
Use `ARIES_CODEX_BINARY` to select another local path to the same pinned
binary. Unit and race tests need neither Docker nor a model service.

The integration is tied to the
[Codex `rust-v0.157.1` source](https://github.com/openai/codex/tree/rust-v0.157.1),
including `environments.toml` and `exec-server --listen stdio`. Review that
native contract before changing the pin. See the
[bridge design](design/bridge.md#codex-ssh-bridge) for descendant cleanup and
the [official configuration reference](https://developers.openai.com/codex/config-reference/)
for custom Responses providers.
