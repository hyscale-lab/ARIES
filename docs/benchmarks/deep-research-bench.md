# Deep Research Bench

`profiles/openclaw-drb-smoke1-deepseek.json` runs the checked-in Deep Research
Bench profile instead of Terminal-Bench 2 (`profiles/openclaw-drb-smoke3-deepseek.json`
and `profiles/hermes-drb-smoke1-deepseek.json` select a larger task subset and
the Hermes harness respectively).

Deep Research Bench tasks are open-ended web research: the profile's
`benchmark.environment.image` must have outbound network access and any
research tooling the agent needs preinstalled. Preparation expects SearXNG at
the fixed paths described in the [implementation notes](../implementation/benchmarks.md#deep-research-bench);
an arbitrary network-enabled image is insufficient. Grading is by an LLM judge rather than a
deterministic verifier script, configured with an optional `benchmark.judge`
block naming a separate model. `benchmark.judge` is entirely optional: when
omitted, the judge call reuses the profile's own `model` config, so grading
with the same model that ran the task needs no extra configuration.

To grade with a different (smarter, or cheaper) model than the one under
test, add an explicit `benchmark.judge` block — all four fields are required
together when present. `profiles/openclaw-drb-smoke1-sglang.json` does this
for real: the task runs on a local Qwen model over sglang, but is graded by
DeepSeek:

```json
"benchmark": {
  "type": "deepresearchbench",
  ...
  "judge": {
    "provider": "deepseek",
    "base_url": "https://api.deepseek.com",
    "api_key_env": "DEEPSEEK_API_KEY",
    "model": "deepseek-flash"
  }
}
```

`judge.api_key_env` does not have to match `model.api_key_env` — the judge
call is entirely separate from the harness's own model call, so it's normal
to point it at a different provider and export that provider's key instead:

```sh
export DEEPSEEK_API_KEY=...   # judge, and (for this profile) the harness model
./bin/aries profiles/openclaw-drb-smoke1-sglang.json
```

The agent is instructed to write its final report to a fixed in-container path
before finishing; ARIES downloads it after the harness stops and the bridge is
revoked, then grades it against the pinned dataset's reference report using
the RACE metric across four dimensions (comprehensiveness, insight,
instruction following, readability). Judge artifacts land in
`runs/<run>/<task_id>/evaluation/{report.md,judge_response.json}`;
`run-result.json`'s `evaluation.score` is RACE's overall ratio
(`target/(target+reference)`) scaled to `[0,100]`, and `evaluation.reward` is
`1` at or above the configured pass threshold (default 50). Successful report collection with grading enabled triggers judge API calls in
addition to harness calls. Judge retries and optional FACT add to cost; account
for both when choosing models and task counts.

### Disabling all LLM grading (optional)

Set `"judge": {"enabled": false}` to skip grading entirely — this is a
master switch that turns off **both** RACE and FACT, not just RACE, so no
judge LLM call happens at all for the task. After successful report download,
`evaluation.status` and `evaluation.verifier_status` become `"not_enabled"` (distinct from a graded
task that failed), and `evaluation.score`/`evaluation.reward` are `0`. This
is useful for collecting agent reports without paying for any judge calls,
e.g. to grade them separately offline. A report download failure still returns
a failed zero-score result; see the [known download-classification gap](../implementation/benchmarks.md#known-implementation-gap).

`judge.enabled: false` requires every other `judge` field
(`provider`/`base_url`/`model`/`api_key_env`) to be left unset — they would
otherwise name a judge that never gets used. A `benchmark.fact` block left
in place at the same time is not an error: it's silently ignored (RACE and
FACT are both off), with a warning printed to stderr at startup explaining
why, the same way a missing Jina API key already degrades FACT with a
warning rather than failing the run. Omitting `judge` entirely, or setting
it without `enabled` (or with `enabled: true`), keeps RACE and FACT exactly
as described below.

### FACT citation checking (optional)

A `benchmark.fact` block additionally grades citation trustworthiness with the
FACT metric: it extracts claim/citation pairs from the report, deduplicates
them, and validates each cited URL's content (fetched through the Jina AI
Reader API) against its claim, using its own (typically cheaper) judge model.
`fact.jina_api_key_env`, naming the host environment variable holding the Jina
key, is always required to enable FACT at all — omit the whole `fact` block
(or leave it unset) to skip FACT entirely, at zero extra cost. Its model
fields (`provider`/`base_url`/`model`/`api_key_env`) are optional as a group,
just like `benchmark.judge`: leave all four unset to grade citations with the
profile's own `model`, or set all four together to use a different model. The
checked-in DRB profiles do the former — they enable FACT with only
`jina_api_key_env` set:

```json
"fact": {
  "jina_api_key_env": "JINA_API_KEY"
}
```


FACT is purely additive: its result never affects `evaluation.score`,
`evaluation.reward`, or task status, which come from RACE alone. Its
artifacts, when configured, land alongside the RACE ones as
`fact_report.json` (success) or `fact_error.txt` (failure) — a failed FACT run
does not fail the task.

#### Obtain a Jina API key

Create or retrieve a key from the [Jina dashboard](https://jina.ai/api-dashboard/)
and export it using the environment variable named by `fact.jina_api_key_env`.
Keep the value out of profiles and shared artifacts. Check the provider dashboard
for current account limits and pricing.

### Web search and fetch

Deep Research Bench tasks need the agent to search and read live web pages
from inside the sandbox. Both harnesses support this through
`harness.web_search.enabled: true`, using the DRB task sandbox's built-in
SearXNG instance as the search backend:

- **OpenClaw** needs no further configuration for basic `web_search`/
  `web_fetch`. It also accepts `harness.web_search.extract_api_key_env`,
  naming the host environment variable holding a Tavily API key; when set,
  it additionally enables the `tavily_extract` tool (search itself stays on
  SearXNG either way).
- **Hermes** likewise accepts `harness.web_search.extract_api_key_env`,
  naming the host environment variable holding a Tavily API key. Without it,
  Hermes's `web_search` tool still works, but `web_extract` (reading a page's
  content) has no backend and fails.

For either harness, export the key before the run, e.g.
`export TAVILY_API_KEY=...`, matching the name given in the profile.

The task prompt itself nudges the agent to call `web_fetch`/`web_extract`
rather than reimplement page retrieval with `curl`/`wget`/a custom parser over
the terminal tool, since models don't reliably prefer the dedicated tool on
their own even when it's in their function-calling schema.

#### Obtain a Tavily API key

Create or retrieve a key from the [Tavily dashboard](https://app.tavily.com)
and export it using the environment variable named by
`harness.web_search.extract_api_key_env` (the profiles use `TAVILY_API_KEY`).
Check the provider dashboard for current account limits and pricing.

### Disabling or limiting subagents (Optional)

`harness.subagents.enabled: false` turns off nested agent sessions for either
harness: OpenClaw's `sessions_spawn`/`sessions_yield` tools, or Hermes's
`delegate_task` tool. The checked-in DRB profiles set this explicitly, since
subagent spawning is not useful for this benchmark's single-report task shape
and adds uncontrolled cost.

To bound fan-out instead of disabling it outright, set
`harness.subagents.max_concurrent` to a positive integer. It maps to each
harness's own concurrency knob — OpenClaw's
`agents.defaults.subagents.maxConcurrent` (default 5 per parent) or Hermes's
`delegation.max_concurrent_children` (default 3) — and is ignored when
`enabled` is `false`.
