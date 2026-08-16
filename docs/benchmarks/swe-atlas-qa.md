## SWE-Atlas QA

[SWE-Atlas](https://github.com/scaleapi/SWE-Atlas) (Scale AI) has three
tracks — Codebase QA, Test-Writing, and Refactoring — with different task and
grading shapes. ARIES implements only the QA track (`data/qa`, "Codebase
Q&A"); Test-Writing and Refactoring are out of scope.

`profiles/openclaw-sweatlasqa-smoke1-deepseek.json` runs the checked-in
SWE-Atlas QA profile against one real task
(`task-6905333b74f22949d97ba998`, a codebase-onboarding question about
Automattic's `wp-calypso`), with DeepSeek as both the harness model and the
judge. `profiles/openclaw-sweatlasqa-smoke1-sglang.json` runs the same task
against an external SGLang-served harness model instead
(`configs/sglang/qwen3-8b-local.yaml`), while keeping DeepSeek as the judge —
the runtime backend that serves the agent and the model that grades its
answer are independent choices; nothing requires them to match.
`profiles/openclaw-sweatlasqa-subset20-deepseek.json` runs a fixed 20-task
subset of `data/qa` with DeepSeek as both the harness model and the judge, at
`execution.concurrency: 3`.

### Task shape

Each task is a directory under the pinned checkout containing `task.toml`
(same `schema_version = "1.1"` shape as Terminal-Bench 2's task files — a
pre-built `docker_image`, resource limits, agent/verifier timeouts),
`instruction.md` (the codebase question, read and passed to the agent
verbatim — unlike Deep Research Bench, ARIES adds no prompt wrapper), and a
private `tests/` directory (`rubrics.json`, `system_prompt.txt`,
`user_prompt_template.txt`, optional `prompt.txt`) that ARIES reads directly
from the host checkout — none of it is ever uploaded into the sandbox.

The agent is expected to write its final answer, wrapped in
`<<FINAL_ANSWER>>` tags, to `/logs/agent/answer.txt` inside the sandbox —
`instruction.md` itself instructs this, so ARIES does not need to augment the
prompt the way Deep Research Bench does for its report path. `PrepareSandbox`
confirms this path starts absent before the harness gets bridge access, and
`Evaluate` downloads it once, after both isolation gates — the only sandbox
material grading ever touches.

### Grading

Unlike Terminal-Bench 2's deterministic pass/fail verifier, grading is by an
LLM judge against a per-task rubric (`tests/rubrics.json`), entirely
host-side — like Deep Research Bench, no code runs inside the sandbox during
evaluation. The vendored dataset's own verifier (`tests/test.sh` running
`tests/evaluate_answer.py` inside the sandbox) is not used at all; ARIES
instead ports its rubric-scoring logic directly into Go
(`pkg/benchmark/sweatlas/rubrics.go`), calling the judge over HTTP from the
host process. For each rubric, the downloaded answer and the rubric's title
(stripped of any numeric prefix like `"1.1: "`) are sent to the judge model,
whose YES/NO response (tolerating a few upstream response-format quirks) is
normalized and, for rubrics annotated "negative", flipped. The results are
aggregated exactly like the upstream script: `reward = 1` only if every
scored "must have" rubric scored 1, and `agg_score` is the mean over all
scored rubrics (any importance) — both are written to
`reward.txt`/`evaluation_results.json` in the run's output directory (not the
sandbox), and `evaluation.score` is always finite and in `[0, 1]` by
construction.

A `benchmark.judge` block is **required** (unlike Deep Research Bench, where
it is optional and falls back to the profile's own model) — judge-graded
rubric scoring is this benchmark's entire output, so there is no sensible
default and no way to disable grading; `judge.enabled` must not be set at
all for this type.

```json
"benchmark": {
  "type": "sweatlasqa",
  "root": ".cache/swe-atlas-qa",
  "tasks": ["task-6905333b74f22949d97ba998"],
  "judge": {
    "provider": "deepseek",
    "base_url": "https://api.deepseek.com",
    "api_key_env": "DEEPSEEK_API_KEY",
    "model": "deepseek-v4-flash"
  }
}
```

task.toml's `[verifier.env]` block declares the keys `EVAL_API_KEY`,
`EVAL_BASE_URL`, and `EVAL_MODEL`, but their *values* in the dataset are
shell-style template placeholders (e.g. `"${OPENAI_API_KEY}"`), meant to be
expanded by Scale's own reference harness ("Harbor") from its own process
environment — they are not literal secrets. Since grading no longer execs
anything inside the sandbox, ARIES never reads or synthesizes a verifier
environment from this block at all; it's decoded and ignored.

`judge.model` must be a string format matching whatever `judge.base_url`
endpoint expects — the sample task's own default
(`anthropic/claude-opus-4-5-20251101`) is an OpenRouter-style composite
string, which is not portable to every endpoint. The checked-in profile
above uses DeepSeek's own API with a plain DeepSeek model ID, which is
internally consistent for that endpoint; picking a different `judge.base_url`
means picking a `judge.model` string that endpoint actually accepts.

### Setup

`configs/versions.json`'s `sweatlasqa` block pins the checkout; run
`./bin/aries setup profiles/openclaw-sweatlasqa-smoke1-deepseek.json` (or
`profiles/openclaw-sweatlasqa-smoke1-sglang.json`, or the equivalent setup
entry point) before the first run.
