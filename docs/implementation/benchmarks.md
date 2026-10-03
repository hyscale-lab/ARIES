# Benchmark evaluation implementations

The [Benchmark contract](../design/benchmark.md) defines the shared boundary.
Its [lifecycle contract](../design/benchmark.md#lifecycle-cancellation-and-failures)
defines when preparation and evaluation may run. User setup and
result interpretation remain in the [benchmark guides](../design/benchmark.md#implementations-and-guides).

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

The public task images contain repository history used to construct the
benchmark. Before the harness receives bridge access, ARIES:

1. resets the repository to the row's `base_commit` and proves it is clean;
2. checks out exactly the official gold-commit verifier files and snapshots
   them privately;
3. privately snapshots ignored build artifacts already present in the image;
4. restores the base worktree and removes verifier staging data;
5. removes Git remotes, refs, reflogs, and unreachable future objects, then
   proves the gold revision is not locally reachable;
6. privately snapshots the sanitized Git metadata, transfers `/app` to the
   numeric agent identity `65532:65532`, and proves the agent can write the
   worktree but cannot write the trusted Git, shell, tar, or Python runtimes.

The verifier, ignored-build, and sanitized-Git snapshots are host artifacts
outside both the task and harness containers. They are mode `0600` under a
mode `0700` private directory. The harness container has no bind mount to the
run output directory. Docker applies `no-new-privileges` to task containers
using the non-root agent identity and positively confirms the option through
post-start container inspection before returning the live sandbox.
Benchmark-owned preparation and evaluation commands explicitly use root.

This is local hardening, not an embargo on public information. SWE-bench Pro is
a public benchmark and the task network remains enabled so the harness can use
the configured model endpoint and ordinary network tools. A deliberately
adversarial agent can add a remote or retrieve public upstream repositories,
commits, datasets, or discussions. Do not use the public split as a confidential
test set.

Evaluation captures staged, tracked, and untracked candidate changes against
the privately restored sanitized Git baseline. The raw download is bounded to
16 MiB before host writes complete,
and binary patch sections are removed to match the evaluator policy. Evaluation
then restores the pinned base plus the image's initial ignored build artifacts,
applies the candidate, and injects the private verifier files, task-specific
script, and parser. Harness-created ignored artifacts are removed before the
initial image snapshot is restored, which makes evaluation start from the
fresh-image build baseline rather than agent-created caches.

Before any private verifier input is staged, and again after the test process
returns, ARIES kills and positively confirms the absence of every process owned
by the agent UID. Verifier paths are installed only through non-symlink parent
directories and become root-owned read-only files. The test script runs as the
non-root agent; its stdout and stderr stream directly to mode-`0600` host
artifacts with a 256 MiB per-stream bound. The parser then runs as root with an
empty environment, isolated Python mode, and a root-only script. Private
container staging is removed and positively proved absent on every evaluation
return path.

The selected official script and parser determine test records; ARIES requires
all declared `FAIL_TO_PASS` and `PASS_TO_PASS` tests to pass. This implementation
requires the optional `LimitedDownloader` and `StreamExecutor` sandbox
capabilities. A sandbox without them is rejected before evaluation proceeds.

Sources: [preparation](../../pkg/benchmark/swebenchpro/sandbox.go),
[evaluation](../../pkg/benchmark/swebenchpro/evaluate.go),
[preparation tests](../../pkg/benchmark/swebenchpro/sandbox_test.go), and
[evaluation tests](../../pkg/benchmark/swebenchpro/evaluate_test.go).
See the [usage guide](../benchmarks/swe-bench-pro.md) for pins, prerequisites,
result artifacts, public-data limitations, and licensing.
