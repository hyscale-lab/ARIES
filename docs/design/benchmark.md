# Benchmark

`Benchmark` owns task meaning: discovery, preparation of agent-visible inputs,
private verifier material, and independent evaluation of the live task sandbox.
It does not own the sandbox lifecycle or decide whether isolation is confirmed.

## Operations and ownership

The [interface](../../pkg/runner/interfaces.go) has three operations:

| Operation | Meaning |
| --- | --- |
| `Tasks(context.Context) ([]core.Task, error)` | Load task instructions, environment requirements, and timeouts. Keep verifier inputs out of returned agent-visible data. |
| `PrepareSandbox(context.Context, core.Task, Sandbox) error` | Prepare and sanitize the started sandbox before bridge access. Confirm private or stale output paths are absent; fail closed if preparation cannot establish its conditions. |
| `Evaluate(context.Context, core.Task, Sandbox) (core.Evaluation, error)` | Evaluate that same sandbox after confirmed harness termination and bridge revocation. Preserve benchmark scoring semantics and return infrastructure errors separately from ordinary failed outcomes. |

Concrete implementations own their loaded task metadata, pinned input validation,
private verifier files, judge credentials, and evaluation artifacts. Runner owns
ordering and passes the live `Sandbox` capability; the benchmark uses its
execution and transfer operations rather than constructing a deployment.

## Lifecycle, cancellation, and failures

[Runner](../../pkg/runner/runner.go) loads tasks, starts the sandbox, prepares it,
starts bridge and harness, runs the harness, then positively stops the harness
and revokes the bridge. Only then does it call `Evaluate`; sandbox shutdown
follows evaluation. A failed preparation stops progression before agent access.
A failed isolation gate blocks evaluation and verifier exposure.

Evaluation is independent of harness success, with separate result fields.
Runner passes the original run context to evaluation, so cancellation can still
prevent grading; confirmed isolation is necessary but does not promise evaluation
completion. Isolation and cleanup use bounded contexts independent of run
cancellation. Implementations must honor the contexts supplied to external work
and release any temporary private staging even on failure.

Verifier tests and solutions remain private until both isolation gates succeed.
Host-side judges retain reference reports and rubrics on the host throughout;
container-based verifiers may inject their private inputs only after those gates.

## Substitution requirements

A replacement must preserve task meaning, score interpretation, resource
ownership, failure distinctions, and verifier isolation. Optional sandbox
capabilities are explicit requirements: SWE-bench Pro needs bounded downloads and
streamed execution. A capability mismatch must fail rather than silently weaken
isolation or artifact bounds. Document unsupported measurements separately from
measured zero values; see the [known DRB download gap](../implementation/benchmarks.md#known-implementation-gap).

Use an explicit constructor in the concrete benchmark package, construction and
configuration mapping in [benchmark wiring](../../internal/app/wiring/benchmark),
and a selection switch in [cmd/aries](../../cmd/aries/wiring.go). Add focused
coverage of preparation, evaluator errors, and isolation before exposing a new
implementation. Do not introduce registration or generic plugin frameworks.

## Implementations and guides

- Terminal-Bench 2: [quick start](../quick-start.md), [evaluation mechanism](../implementation/benchmarks.md#terminal-bench-2).
- Deep Research Bench: [usage](../benchmarks/deep-research-bench.md), [host-side RACE and FACT](../implementation/benchmarks.md#deep-research-bench).
- SWE-Atlas QA: [usage](../benchmarks/swe-atlas-qa.md), [rubric scoring](../implementation/benchmarks.md#swe-atlas-qa).
- SWE-bench Pro public split: [usage](../benchmarks/swe-bench-pro.md), [private snapshots and evaluation](../implementation/benchmarks.md#swe-bench-pro).

See [design principles](../design.md) for the obligations shared by all roles.
