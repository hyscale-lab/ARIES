## Deep Research Bench

`profiles/openclaw-drb-smoke1-deepseek.json` runs the checked-in Deep Research
Bench profile instead of Terminal-Bench 2 (`profiles/openclaw-drb-smoke3-deepseek.json`
and `profiles/hermes-drb-smoke1-deepseek.json` select a larger task subset and
the Hermes harness respectively). 

Deep Research Bench tasks are open-ended web research: the profile's
`benchmark.environment.image` must have outbound network access and any
research tooling (browser/search CLI) the agent needs preinstalled — ARIES
does not build or provide that image. Grading is by an LLM judge rather than a
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
`1` at or above the configured pass threshold (default 50). Every evaluated task
makes a paid LLM-judge API call in addition to the harness model call — using
a cheaper judge model is recommended for large task counts.

### Disabling all LLM grading (optional)

Set `"judge": {"enabled": false}` to skip grading entirely — this is a
master switch that turns off **both** RACE and FACT, not just RACE, so no
judge LLM call happens at all for the task. `evaluation.status` and
`evaluation.verifier_status` become `"not_enabled"` (distinct from a graded
task that failed), and `evaluation.score`/`evaluation.reward` are `0`. This
is useful for collecting agent reports without paying for any judge calls,
e.g. to grade them separately offline.

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

#### Steps to obtain a Jina API key
1. **Go to the Jina AI Dashboard:** Visit [jina.ai/api-dashboard](https://jina.ai/api-dashboard/).
2. **Sign In or Register:** Log in using your email, GitHub, or Google account.
3. **Navigate to Keys:** Click on **API** in the main navigation, then select **API Key & Billing** (or **Key Manager**).
4. **Generate / Copy Key:** Your secret API key will be displayed under your key management settings. Click to copy it.

> **Note:** New accounts receive **10 million free tokens** for non-commercial testing. Store your key securely, as it serves as a bearer token for authentication.

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

#### Steps to obtain a tavily API key
1. **Go to the Tavily Platform:** Visit [tavily.com](https://www.tavily.com) or go directly to the [Tavily Dashboard](https://app.tavily.com).
2. **Sign Up or Log In:** Register for a new account using your email address, or log in via Google or GitHub OAuth. No credit card is required for the free tier.
3. **Locate Your API Key:** Once signed in, land on the **Overview** page or **API Keys** section of the dashboard.
4. **Copy the Key:** Click to copy your default API key (it starts with the prefix `tvly-`). You can also create additional keys directly from the dashboard if needed.

> **Note:** The free plan includes **1,000 free API credits/searches per month**, which reset on the 1st of every month. Store your key in an environment variable named `TAVILY_API_KEY`.

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