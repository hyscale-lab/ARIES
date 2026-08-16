# Benchmark

`Benchmark` owns task meaning. It loads task definitions, prepares benchmark
inputs, sanitizes the live sandbox before agent access, and independently
evaluates the same sandbox after the harness is stopped and bridge access is
revoked.

## Boundary and lifecycle

Verifier tests and solutions remain private to the benchmark. They are not
placed in the sandbox until positive harness-stop and bridge-revocation gates
have both succeeded. Benchmark evaluation does not depend on whether the agent
harness reported success, and its result is recorded separately.

```mermaid
flowchart TB
    P[Private verifier material]
    S[Prepare live sandbox]
    A[Run harness with<br/>temporary access]
    H[Positively stop harness]
    R[Positively revoke bridge]
    I[Inject verifier material]
    E[Benchmark evaluates<br/>the same live sandbox]
    HO[Harness outcome:<br/>success or failure]
    EO[Evaluation outcome]
    O[Run result keeps<br/>outcomes separate]

    S --> A
    A --> H --> R --> I --> E --> EO --> O
    P --> I
    A --> HO --> O
```

The current implementations are Terminal-Bench 2, Deep Research Bench, and
SWE-Atlas QA, all from an exact pinned checkout. Terminal-Bench 2's task
environment images and workdirs are derived from the selected task data;
setup verifies the pinned checkout and selected task inputs before a run.

Deep Research Bench has no sandbox-resident private verifier tree. Its private
material is a reference report and RACE-dimension rubric compared by an
LLM judge entirely host-side; that material is never uploaded into the
sandbox. `PrepareSandbox` instead confirms the agent's designated report-output
path starts absent, and `Evaluate` downloads the agent's report from that path
after both isolation gates before invoking the judge. The agent's research
work itself still runs entirely inside the sandbox, exactly like Terminal-Bench
2 — only grading moves host-side.

An optional FACT pass layers citation-trustworthiness checking on top of RACE:
it extracts claim/citation pairs from the same downloaded report, fetches each
cited URL host-side through the Jina AI Reader API, and validates the claim
against the fetched content with its own judge model. FACT is strictly
additive — configuring it (or not) never changes the RACE-derived score,
reward, or status.

SWE-Atlas QA follows the Deep Research Bench pattern directly: it has no
sandbox-resident private verifier tree either. Its private material (the
task's rubric and prompts) is read from the pinned host checkout and never
uploaded into the sandbox; the LLM judge call happens entirely host-side.
`PrepareSandbox` confirms the agent's designated answer-output path starts
absent, and `Evaluate` downloads the agent's answer from that path after both
isolation gates, then scores it rubric-by-rubric against the host-resident
judge before writing `reward.txt`/`evaluation_results.json` to the run's
output directory.

## Customization & Contribution Guide

Implement a new benchmark behind the existing `Benchmark` boundary: define its
task loading, pre-agent sandbox preparation, private verifier ownership, and live
sandbox evaluation in a concrete package. Expose an explicit constructor and
command switch, add tests that prove verifier secrecy and evaluation after both
isolation gates, and document setup and supported profiles. Do not introduce
registration, discovery, factories, reflection, DI, or generic plugins.
