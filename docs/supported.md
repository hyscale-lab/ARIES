# Supported implementations

ARIES uses explicit profile values and command switches. This page lists the
implementations present in the repository; it is not a promise of automatic
plugin discovery.

| Category | Implementation and status | ARIES ownership | Configuration and examples |
| --- | --- | --- | --- |
| Agent harness | **OpenClaw** — text is the default mode; realtime voice is also supported | Starts and stops the pinned harness container; retains private text or realtime artifacts | `harness.type: "openclaw"`; omit `harness.mode` or use `"agent"` for text; use `"realtime"` with `profiles/openclaw-tb2-fix-git-realtime-deepseek.json` |
| Agent harness | **Hermes** — text only; the pinned upstream image is used unmodified | Starts and stops the pinned harness container; runs one Hermes one-shot and retains its output, container log, and exported session trajectory | `harness.type: "hermes"`; requires `bridge.type: "hermes-ssh"`; `profiles/hermes-tb2-fix-git-deepseek.json` |
| Benchmark | **Terminal-Bench 2** — current supported benchmark | Verifies the pinned checkout, loads tasks, sanitizes verifier paths, and evaluates | `benchmark.type: "terminalbench2"`; checkout and revision in `configs/versions.json` |
| Tool sandbox | **Docker** — current supported sandbox | Owns task containers and networks through the Moby Go SDK | `sandbox.type: "docker"`; task images come from pinned Terminal-Bench task data |
| Tool bridge | **OpenClaw SSH** — pair-specific bridge for OpenClaw | Owns temporary SSH access, evidence, credentials, listener, sessions, and revocation | `bridge.type: "openclaw-ssh"`; requires `bin/aries-ssh` beside `bin/aries` |
| Tool bridge | **Hermes SSH** — pair-specific bridge for Hermes | Same ownership; accepts Hermes's own SSH grammar and denies its `~/.hermes` file sync | `bridge.type: "hermes-ssh"`; requires `harness.type: "hermes"`; needs no helper binary because Hermes runs OpenSSH itself |
| Model service | **DeepSeek** — supported external OpenAI-compatible endpoint | Validates model access; does not own the service | `runtime.backend: "deepseek"`, `runtime.mode: "external"`; `profiles/openclaw-tb2-fix-git-deepseek.json` |
| Model service | **SGLang** — supported external or ARIES-managed runtime | Validates both modes; in managed mode owns one host process for the profile run | `runtime.backend: "sglang"`; `profiles/openclaw-tb2-fix-git-sglang.json`; `configs/sglang/qwen3-8b-local.yaml` |

## Configuration boundaries

The four Runner implementations are chosen by `benchmark.type`, `harness.type`,
`sandbox.type`, and `bridge.type`. At present, the values in the table are the
only wired choices. Concrete construction is explicit rather than registered or
discovered.

Each bridge implements exactly one harness's SSH grammar, so the harness and
bridge values must be paired: `openclaw` with `openclaw-ssh`, and `hermes` with
`hermes-ssh`. A crossed pair is rejected before the run starts. Hermes
additionally requires `/bin/bash` in the task image, because every tool call it
issues is `bash -c` on the remote.

Model services sit outside the four-role Runner. DeepSeek must be external and uses the
configured remote base URL. SGLang accepts an external endpoint backed by its
native YAML, or a managed mode with an explicit Python executable, startup and
stop timeouts, and optional validated GPU indices. DeepSeek is not a managed
runtime, and SGLang is not a fifth Runner role.

Realtime mode requires `harness.realtime` TTS and session settings and the
separate `OPENAI_API_KEY` named by `harness.realtime.tts.api_key_env`. The TTS
credential is separate from the configured model credential. See the
[quick start](quick-start.md#realtime-openclaw-mode) for the runnable example
and [AgentHarness design](design/harness.md) for ownership boundaries.

Start with one of the checked-in profiles:

- `profiles/openclaw-tb2-fix-git-deepseek.json`
- `profiles/openclaw-tb2-fix-git-sglang.json`
- `profiles/openclaw-tb2-fix-git-realtime-deepseek.json`
- `profiles/hermes-tb2-fix-git-deepseek.json`

The SGLang profile references
`configs/sglang/qwen3-8b-local.yaml`. Additional checked-in DeepSeek profiles
select larger Terminal-Bench subsets and execution settings without adding new
component implementations. See the [quick start](quick-start.md) for runnable
commands and [architecture](design.md) for lifecycle and isolation guarantees.
