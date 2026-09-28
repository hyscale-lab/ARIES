# Supported implementations

ARIES uses explicit profile values and command switches. This page lists the
implementations present in the repository; it is not a promise of automatic
plugin discovery.

| Category | Implementation and status | ARIES ownership | Configuration and examples |
| --- | --- | --- | --- |
| Agent harness | **OpenClaw** — text is the default mode; realtime voice is also supported | Starts and stops the pinned harness container; retains private text or realtime artifacts; can enable web search/fetch, Tavily-backed web extract, and disable subagent spawning | `harness.type: "openclaw"`; omit `harness.mode` or use `"agent"` for text; use `"realtime"` with `profiles/openclaw-tb2-fix-git-realtime-deepseek.json`; `harness.web_search.enabled`, `harness.web_search.extract_api_key_env`, `harness.subagents.enabled` |
| Agent harness | **Hermes** — text only; the pinned upstream image is used unmodified. Verified against `v2026.5.29.2` and `v2026.8.3`; pinned at `v2026.8.31` | Starts and stops the pinned harness container; runs one Hermes one-shot and retains its output, container log, and exported session trajectory; can enable web search and Tavily-backed web extract; can set the model window, the compaction trigger, and an opaque per-request `extra_body` | `harness.type: "hermes"`; requires `bridge.type: "hermes-ssh"`; `profiles/hermes-tb2-fix-git-deepseek.json`, `profiles/hermes-tb2-fix-git-vllm-compaction.json`; `harness.web_search.enabled`, `harness.web_search.extract_api_key_env`, `harness.compaction`, `harness.hermes.extra_body`, `model.context_length`, `model.max_tokens`, `model.temperature` |
| Agent harness | **Codex** — text only; unmodified CLI `0.157.1` with its native remote executor | Owns a separate harness container, private model configuration, and JSONL trajectory; supports Responses API model servers | `harness.type: "codex"`; requires `bridge.type: "codex-ssh"` and `harness.codex.executable`; `profiles/codex-tb2-fix-git-openai.json`; [Codex guide](codex.md) |
| Benchmark | **Terminal-Bench 2** — 89 verifier-based terminal tasks in the pinned checkout | Verifies the pinned checkout, loads tasks, sanitizes verifier paths, and evaluates | `benchmark.type: "terminalbench2"`; checkout and revision in `configs/versions.json` |
| Benchmark | **Deep Research Bench** — 100 LLM-judged open-ended research tasks, with optional FACT citation checking | Verifies the pinned dataset checkout, loads prompts, confirms the agent's report path starts absent, downloads the produced report, grades it against the reference report with RACE, and optionally validates its citations with FACT | `benchmark.type: "deepresearchbench"`; checkout and revision in `configs/versions.json`; requires `benchmark.environment` and a `benchmark.judge` model block; optional `benchmark.fact` block requires `fact.jina_api_key_env` |
| Benchmark | **SWE-Atlas QA** — LLM-judged codebase Q&A benchmark, graded entirely host-side (only the QA track of SWE-Atlas is implemented) | Verifies the pinned checkout, loads tasks, sanitizes the agent's answer path, downloads the answer after both isolation gates, and grades it against a rubric via a host-side judge model call | `benchmark.type: "sweatlasqa"`; checkout and revision in `configs/versions.json`; requires a `benchmark.judge` model block (mandatory, no disable); forbids `benchmark.environment`/`benchmark.fact` |
| Benchmark | **SWE-bench Pro (public split)** — 731 repository issue-resolution tasks | Verifies the pinned dataset and evaluator checkouts, derives each image from `dockerhub_tag`, sanitizes local repository history before agent access, keeps verifier tests private, and scores the candidate with the pinned task script and parser | `benchmark.type: "swebenchpro"`; dataset and evaluator pins in `configs/versions.json`; `profiles/openclaw-sbpro-smoke1-deepseek.json`; [benchmark guide](benchmarks/swe-bench-pro.md) |
| Benchmark | **RoadmapBench** — 115 software version-upgrade tasks with partial completion rewards | Loads sparse pinned task data, sanitizes verifier/oracle staging, and runs official private tests after both isolation gates | `benchmark.type: "roadmapbench"`; Codex `smoke1`, `all115`, and `qwen38-27b-xhigh-first20` profiles; [benchmark guide](benchmarks/roadmapbench.md) |
| Tool sandbox | **Docker** — current supported sandbox | Owns task containers and networks through the Moby Go SDK | `sandbox.type: "docker"`; task images come from pinned benchmark data or the profile's benchmark environment |
| Tool bridge | **OpenClaw SSH** — pair-specific bridge for OpenClaw | Owns temporary SSH access, evidence, credentials, listener, sessions, and revocation | `bridge.type: "openclaw-ssh"`; requires `bin/aries-ssh` beside `bin/aries` |
| Tool bridge | **Hermes SSH** — pair-specific bridge for Hermes | Same ownership; accepts Hermes's own SSH grammar and denies its `~/.hermes` file sync | `bridge.type: "hermes-ssh"`; requires `harness.type: "hermes"`; needs no helper binary because Hermes runs OpenSSH itself |
| Tool bridge | **Codex SSH** — native executor RPC over one authenticated SSH session | Stages the pinned executor and descendant supervisor in the task container; confirms all executor descendants are reaped before evaluation | `bridge.type: "codex-ssh"`; requires `bin/aries-codex-ssh` and `bin/aries-codex-exec` beside `bin/aries` |
| Model service | **DeepSeek** — supported external OpenAI-compatible endpoint | Validates model access; does not own the service | `runtime.backend: "deepseek"`, `runtime.mode: "external"`; `profiles/openclaw-tb2-fix-git-deepseek.json` |
| Model service | **SGLang** — supported external or ARIES-managed runtime | Validates both modes; in managed mode owns one host process for the profile run | `runtime.backend: "sglang"`; `profiles/openclaw-tb2-fix-git-sglang.json`; `configs/sglang/qwen3-8b-local.yaml` |
| Model service | **OpenAI-compatible server** — external only; vLLM, `llama.cpp`, gateways, hosted endpoints | Confirms the served model through one bounded `/v1/models` request; never starts, configures, or stops the server | `runtime.backend: "openai"`, `runtime.mode: "external"`; `profiles/hermes-tb2-fix-git-vllm.json` |

## Configuration boundaries

The four Runner implementations are chosen by `benchmark.type`, `harness.type`,
`sandbox.type`, and `bridge.type`. At present, the values in the table are the
only wired choices. Concrete construction is explicit rather than registered or
discovered.

Each bridge implements exactly one harness's SSH grammar, so the harness and
bridge values must be paired: `openclaw` with `openclaw-ssh`, `hermes` with
`hermes-ssh`, and `codex` with `codex-ssh`. A crossed pair is rejected before the run starts. Hermes
additionally requires `/bin/bash` in the task image, because every tool call it
issues is `bash -c` on the remote.

Codex requires an `openai` or `sglang` endpoint implementing streaming
`/v1/responses`, including tool calls. Passing `/v1/models` discovery alone does
not establish that capability. A Chat Completions-only endpoint, including the
configured native DeepSeek backend, cannot serve this harness. Codex accepts
`model.context_length`, native reasoning effort and developer instructions
under `harness.codex`, and `harness.subagents` controls. `max_tokens`,
`temperature`, compaction, voice, and dedicated web-search overrides remain
unsupported. Codex can research through the task sandbox's shell and SearXNG.

Model services sit outside the four-role Runner. DeepSeek must be external and uses the
configured remote base URL. SGLang accepts an external endpoint without a local
launch file, or a managed mode with an explicit Python executable, startup and
stop timeouts, and optional validated GPU indices. The `openai` backend is any
other OpenAI-compatible server; it names the endpoint's API rather than a
runtime ARIES prepares, is external only, and has no native file. DeepSeek is
not a managed runtime, and SGLang is not a fifth Runner role.

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
- `profiles/hermes-tb2-fix-git-vllm.json` — the same run against an external vLLM server through the `openai` backend
- `profiles/hermes-tb2-fix-git-vllm-compaction.json` — the vLLM run with an explicit model window, a 64K compaction cap, and a per-task `extra_body`
- `profiles/codex-roadmapbench-smoke1-openai.json` — one RoadmapBench task with Codex and an external Responses API server
- `profiles/codex-roadmapbench-all115-openai.json` — all 115 pinned RoadmapBench tasks in task-ID order, with the same Codex and external Responses API settings; replace its endpoint and model placeholders
- `profiles/codex-roadmapbench-qwen38-27b-xhigh-first20.json` — the first 20 tasks in that order with `Qwen/Qwen3.8-27B`, `xhigh`, and up to three native Codex subagents; ultra is its delegation prompt strategy, and the endpoint remains a placeholder
- `profiles/codex-tb2-fix-git-openai.json` — Codex with an external Responses API server; replace its endpoint and model placeholders
- `profiles/openclaw-drb-smoke1-deepseek.json` — Deep Research Bench, DeepSeek harness model, DeepSeek judge model, web search with Tavily extract
- `profiles/openclaw-drb-smoke3-deepseek.json` — Deep Research Bench, larger task subset, web search with Tavily extract
- `profiles/hermes-drb-smoke1-deepseek.json` — Deep Research Bench, Hermes harness with web search and Tavily extract
- `profiles/openclaw-sweatlasqa-smoke1-deepseek.json` — SWE-Atlas QA, one codebase-onboarding task, DeepSeek harness and judge model
- `profiles/openclaw-sweatlasqa-smoke1-sglang.json` — SWE-Atlas QA, one codebase-onboarding task, external SGLang harness model with a DeepSeek judge model
- `profiles/openclaw-sweatlasqa-subset20-deepseek.json` — SWE-Atlas QA, 20-task subset, DeepSeek harness and judge model
- `profiles/openclaw-sbpro-smoke1-deepseek.json` — one public SWE-bench Pro task with OpenClaw and DeepSeek
- `profiles/hermes-sbpro-smoke1-deepseek.json` — the same public SWE-bench Pro task with Hermes and DeepSeek

The SGLang profile references
`configs/sglang/qwen3-8b-local.yaml`. Additional checked-in DeepSeek profiles
select larger Terminal-Bench subsets and execution settings without adding new
component implementations. See the [quick start](quick-start.md) for runnable
commands and [architecture](design.md) for lifecycle and isolation guarantees.
SWE-bench Pro has additional data, image-architecture, isolation, and licensing
notes in its [benchmark guide](benchmarks/swe-bench-pro.md).
