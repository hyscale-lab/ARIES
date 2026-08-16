# Supported implementations

ARIES uses explicit profile values and command switches. This page lists the
implementations present in the repository; it is not a promise of automatic
plugin discovery.

| Category | Implementation and status | ARIES ownership | Configuration and examples |
| --- | --- | --- | --- |
| Agent harness | **OpenClaw** — text is the default mode; realtime voice is also supported | Starts and stops the pinned harness container; retains private text or realtime artifacts; can enable web search/fetch, Tavily-backed web extract, and disable subagent spawning | `harness.type: "openclaw"`; omit `harness.mode` or use `"agent"` for text; use `"realtime"` with `profiles/openclaw-tb2-fix-git-realtime-deepseek.json`; `harness.web_search.enabled`, `harness.web_search.extract_api_key_env`, `harness.subagents.enabled` |
| Agent harness | **Hermes** — text only; the pinned upstream image is used unmodified. Verified against `v2026.5.29.2` and `v2026.8.3` | Starts and stops the pinned harness container; runs one Hermes one-shot and retains its output, container log, and exported session trajectory; can enable web search and Tavily-backed web extract | `harness.type: "hermes"`; requires `bridge.type: "hermes-ssh"`; `profiles/hermes-tb2-fix-git-deepseek.json`; `harness.web_search.enabled`, `harness.web_search.extract_api_key_env` |
| Benchmark | **Terminal-Bench 2** — verifier-based terminal task benchmark | Verifies the pinned checkout, loads tasks, sanitizes verifier paths, and evaluates | `benchmark.type: "terminalbench2"`; checkout and revision in `configs/versions.json` |
| Benchmark | **Deep Research Bench** — LLM-judged open-ended research benchmark, with optional FACT citation checking | Verifies the pinned dataset checkout, loads prompts, confirms the agent's report path starts absent, downloads the produced report, grades it against the reference report with RACE, and optionally validates its citations with FACT | `benchmark.type: "deepresearchbench"`; checkout and revision in `configs/versions.json`; requires `benchmark.environment` and a top-level `judge` model block; optional top-level `fact` block requires `fact.jina_api_key_env` |
| Benchmark | **SWE-Atlas QA** — LLM-judged codebase Q&A benchmark, graded entirely host-side (only the QA track of SWE-Atlas is implemented) | Verifies the pinned checkout, loads tasks, sanitizes the agent's answer path, downloads the answer after both isolation gates, and grades it against a rubric via a host-side judge model call | `benchmark.type: "sweatlasqa"`; checkout and revision in `configs/versions.json`; requires a top-level `judge` model block (mandatory, no disable); forbids `benchmark.environment`/`fact` |
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
- `profiles/openclaw-drb-smoke1-deepseek.json` — Deep Research Bench, DeepSeek harness model, DeepSeek judge model, web search with Tavily extract
- `profiles/openclaw-drb-smoke3-deepseek.json` — Deep Research Bench, larger task subset, web search with Tavily extract
- `profiles/hermes-drb-smoke1-deepseek.json` — Deep Research Bench, Hermes harness with web search and Tavily extract
- `profiles/openclaw-sweatlasqa-smoke1-deepseek.json` — SWE-Atlas QA, one codebase-onboarding task, DeepSeek harness and judge model
- `profiles/openclaw-sweatlasqa-smoke1-sglang.json` — SWE-Atlas QA, one codebase-onboarding task, external SGLang harness model with a DeepSeek judge model
- `profiles/openclaw-sweatlasqa-subset20-deepseek.json` — SWE-Atlas QA, 20-task subset, DeepSeek harness and judge model

The SGLang profile references
`configs/sglang/qwen3-8b-local.yaml`. Additional checked-in DeepSeek profiles
select larger Terminal-Bench subsets and execution settings without adding new
component implementations. See the [quick start](quick-start.md) for runnable
commands and [architecture](design.md) for lifecycle and isolation guarantees.
