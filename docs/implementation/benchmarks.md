# Benchmark evaluation implementations

The [Benchmark contract](../design/benchmark.md) defines the shared boundary.
Its [lifecycle contract](../design/benchmark.md#lifecycle-cancellation-and-failures)
defines when preparation and evaluation may run. User setup and
result interpretation remain in the [benchmark guides](../design/benchmark.md#implementations-and-guides).

Deep Research Bench and SWE-Atlas share the concrete
[Chat Completions client](../../pkg/model/chat.go) for credentials, request
settings, HTTP handling, response limits, and content extraction. Each benchmark
supplies generation defaults and owns its prompts, parsing, scores, and retries.
The shared client sends one request per call; it adds no retry policy.

## Terminal-Bench 2

Task loading derives the image, workdir, limits, and verifier timeout from the
pinned task checkout. Preparation removes private paths and confirms their
absence before bridge access. After the isolation gates, evaluation clears stale
verifier paths, uploads the private test files, and runs `/bin/bash /tests/test.sh`
in the task workdir. It retains stdout, stderr, `ctrf.json`, and `reward.txt`.
Reward must be exactly `0` or `1`; CTRF is validated against that reward.
Command, download, malformed-reward, and inconsistent-CTRF failures are evaluator
errors rather than successful graded outcomes.

Sources: [task loading and preparation](../../pkg/benchmark/terminalbench/terminalbench.go),
[evaluation](../../pkg/benchmark/terminalbench/evaluate.go),
[CTRF validation](../../pkg/benchmark/terminalbench/ctrf.go), and
[tests](../../pkg/benchmark/terminalbench/terminalbench_test.go).

## Deep Research Bench

Deep Research Bench has no sandbox-resident private verifier tree. Its private
material is a reference report and RACE-dimension rubric compared by an
LLM judge entirely host-side; that material is never uploaded into the
sandbox. `PrepareSandbox` instead confirms the agent's designated report-output
path starts absent, and `Evaluate` downloads the report before invoking the judge.

An optional FACT pass layers citation-trustworthiness checking on top of RACE:
it extracts claim/citation pairs from the same downloaded report, fetches each
cited URL host-side through the Jina AI Reader API, and validates the claim
against the fetched content with its own judge model. FACT is strictly
additive — configuring it (or not) never changes the RACE-derived score,
reward, or status.

Preparation also starts SearXNG inside the task sandbox and probes readiness;
images must provide `/opt/searxng-src`, `/opt/searxng-venv/bin/python`, and
`/etc/searxng/settings.yml`. Readiness uses `curl` against port 8888 inside the
sandbox; logs go to `/var/log/searxng.log`. RACE and FACT run
concurrently over the downloaded report when both are enabled. A disabled judge
skips both workloads, but report download still happens first.

### Known implementation gap

The evaluator currently treats **every report download error** as a missing
report: it returns a failed zero-score evaluation with an error message but no
Go error. Transport, permission, and cancellation failures can therefore be
classified as absent agent output. The required distinction between evaluation
outcomes and infrastructure failures is not fully implemented here. SWE-Atlas
already distinguishes `runner.ErrNotFound` from other download errors.

Sources: [preparation](../../pkg/benchmark/deepresearchbench/sandbox.go),
[evaluation and download handling](../../pkg/benchmark/deepresearchbench/evaluate.go),
[RACE](../../pkg/benchmark/deepresearchbench/race.go),
[FACT](../../pkg/benchmark/deepresearchbench/fact.go), and
[evaluator tests](../../pkg/benchmark/deepresearchbench/evaluate_test.go).
See the [usage guide](../benchmarks/deep-research-bench.md) for judge configuration
and score interpretation.

## SWE-Atlas QA

Unlike Terminal-Bench 2's deterministic pass/fail verifier, grading is by an
LLM judge against a per-task rubric (`tests/rubrics.json`), entirely
host-side — like Deep Research Bench, no code runs inside the sandbox during
evaluation. The vendored dataset's own verifier (`tests/test.sh` running
`tests/evaluate_answer.py` inside the sandbox) is not used at all; ARIES
instead ports its rubric-scoring logic directly into Go
([rubrics.go](../../pkg/benchmark/sweatlas/rubrics.go)), calling the judge over HTTP from the
host process. For each rubric, the downloaded answer and the rubric's title
(stripped of any numeric prefix like `"1.1: "`) are sent to the judge model,
whose YES/NO response (tolerating a few upstream response-format quirks) is
normalized and, for rubrics annotated "negative", flipped. The results are
aggregated like the upstream script: `reward = 1` only if at least one
“must have” rubric is scored and every scored “must have” rubric scored 1,
and `agg_score` is the mean over all
scored rubrics (any importance) — both are written to
`reward.txt`/`evaluation_results.json` in the run's output directory (not the
sandbox), and `evaluation.score` is always finite and in `[0, 1]` by
construction.

Rubrics are judged independently with up to eight attempts per rubric.
Unparseable responses retry immediately; request failures use bounded exponential
backoff. Rubrics that remain unscored are omitted from aggregation and recorded
in `judge_errors.log`. Interpret the aggregate together with these errors: a
successful reward alone does not establish that every rubric received a score.
Missing or empty answers fail before the disabled-judge check. Other download
errors propagate as evaluator errors.

Sources: [evaluation](../../pkg/benchmark/sweatlas/evaluate.go),
[rubric scoring](../../pkg/benchmark/sweatlas/rubrics.go), and
[evaluator tests](../../pkg/benchmark/sweatlas/evaluate_test.go).
See the [usage guide](../benchmarks/swe-atlas-qa.md) for profiles and judge settings.

## SWE-bench Pro

ARIES follows the upstream evaluator, `swe_bench_pro_eval.py` in the pinned
evaluator checkout: the agent's patch is the only thing taken from the agent's
sandbox, and tests run in a fresh container started from the same task image.

The public task images contain repository history used to construct the
benchmark. Before the harness receives bridge access, ARIES:

1. runs the first three lines of the row's `before_repo_set_cmd`: reset to
   `base_commit`, `git clean -fd`, and check out `base_commit`;
2. removes Git remotes, refs, reflogs, and unreachable future objects, then
   proves `HEAD` is the base commit and the gold revision is not locally
   reachable;
3. transfers `/app` to the numeric agent identity `65532:65532` and proves the
   agent can write the worktree but cannot write the trusted Git, shell, tar, or
   Python runtimes.

Preparation does not inspect or archive the rest of the image state. Ignored
build output, submodule checkouts, and any other state the image ships stay as
published, as they do upstream. Verifier files never enter the agent's sandbox.
The harness container has no bind mount to the run output directory. Docker
applies `no-new-privileges` to task containers using the non-root agent
identity and positively confirms the option through post-start container
inspection before returning the live sandbox. Benchmark-owned preparation
commands explicitly use root.

This is local hardening, not an embargo on public information. SWE-bench Pro is
a public benchmark and the task network remains enabled so the harness can use
the configured model endpoint and ordinary network tools. A deliberately
adversarial agent can add a remote or retrieve public upstream repositories,
commits, datasets, or discussions. Do not use the public split as a confidential
test set.

Evaluation starts after harness stop and bridge revocation are confirmed. In the
agent's sandbox, ARIES kills and positively confirms the absence of every
process owned by the agent UID, then, as the agent user, runs `git add -A` and
streams `git diff --cached --binary <base_commit>` to a mode-`0600` host
artifact, bounded to 16 MiB. The repository and its Git metadata are
agent-controlled, so no privileged Git command runs on them. If Git cannot
produce the patch, ARIES records why and evaluates an empty patch, as upstream
would for an agent run that produced none. Binary patch sections are removed to
match the upstream evaluator.

ARIES then starts a fresh evaluation sandbox from the task environment with the
image's default user, uploads the patch, the task's `run_script.sh`, and
`parser.py` to `/workspace`, and runs upstream's entry script step by step in
`/app`:

1. `git reset --hard <base_commit>` and `git checkout <base_commit>`;
2. `git apply -v /workspace/patch.diff`;
3. the last line of `before_repo_set_cmd`, which checks out the gold-commit
   verifier files;
4. `bash /workspace/run_script.sh <selected tests>`;
5. `python /workspace/parser.py` on the script's stdout and stderr.

As upstream, a patch that does not apply does not stop evaluation; the tests run
and the apply failure is recorded in `reason.txt`. The script's stdout and
stderr stream directly to mode-`0600` host artifacts with a 256 MiB per-stream
bound; the parser output download is bounded to 16 MiB. The Runner stops the
evaluation sandbox after `Evaluate` returns.

The selected official script and parser determine test records; ARIES requires
all declared `FAIL_TO_PASS` and `PASS_TO_PASS` tests to pass. The agent's
sandbox must provide the optional `StreamExecutor` capability, and the
evaluation sandbox both `StreamExecutor` and `LimitedDownloader`. A sandbox
without them is rejected before it is used.

Sources: [preparation](../../pkg/benchmark/swebenchpro/sandbox.go),
[evaluation](../../pkg/benchmark/swebenchpro/evaluate.go),
[preparation tests](../../pkg/benchmark/swebenchpro/sandbox_test.go), and
[evaluation tests](../../pkg/benchmark/swebenchpro/evaluate_test.go).
See the [usage guide](../benchmarks/swe-bench-pro.md) for pins, prerequisites,
result artifacts, public-data limitations, and licensing.

## Toolathlon

Toolathlon's own decoupled runner already splits a task the way ARIES does.
Preparation installs the pinned project tree and the task directory in the
task image, runs Toolathlon's `container_preprocess`, and starts its
`container_tool_gateway`: one MCP-over-SSE server in front of every MCP server
the task declares, which the harness reaches at the sandbox's network alias.
For tasks backed by Toolathlon's self-hosted applications, a loopback forwarder
inside the sandbox carries the applications' fixed ports to the Docker host.
Before bridge access, preparation archives the grader and ground truth
(Toolathlon's protected-name list) to the private run directory, removes them,
and proves their absence. After the isolation gates, evaluation discards
anything the agent left under those names, restores the archive, and runs
Toolathlon's `container_eval`, whose verdict file decides the score.

The grader runs in the sandbox the agent had root in, so evaluation does not
trust what the agent left of it. The evaluator code is extracted again from
the host checkout, whose revision is re-verified first. The runtime the
checkout does not carry (uv, the interpreter it manages, the virtual
environment, uv's configuration files and the project's top-level files) is
inventoried file by file before bridge access and again before grading; any
difference refuses the evaluation, naming the paths. The image's system
programs are not covered, as for the other benchmarks that verify in the
task container.

Sources: [task loading](../../pkg/benchmark/toolathlon/toolathlon.go),
[preparation](../../pkg/benchmark/toolathlon/sandbox.go),
[evaluation](../../pkg/benchmark/toolathlon/evaluate.go),
[runtime inventory](../../pkg/benchmark/toolathlon/runtime.go), and
[tests](../../pkg/benchmark/toolathlon/toolathlon_test.go).
See the [usage guide](../benchmarks/toolathlon.md) for the task subset, the
application deployments, and what differs from Toolathlon's own runner.
