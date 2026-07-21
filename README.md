# ARIES

ARIES is a small, human-readable Go benchmark runner under construction. Its
first complete path will run the pinned Terminal-Bench 2 `fix-git` task through
an unmodified upstream OpenClaw container, OpenClaw's SSH sandbox backend, one
local Docker task sandbox, and an evaluator that runs only after harness access
has been revoked.

## Status

M0 established the accepted architecture, upstream compatibility pins,
repository boundaries, and implementation checklist. M1 adds the Go module,
strict experiment configuration, direct shared data, and the lifecycle Runner
with unit-tested failure, cancellation, isolation, and cleanup behavior.

Concrete Terminal-Bench, Docker, SSH, and OpenClaw adapters land in M2 through
M5. Until then the CLI validates the experiment and deliberately reports that
the known component types are not wired; it does not substitute mocks or empty
implementations.

- [DESIGN.md](DESIGN.md) records component boundaries, lifecycle ordering,
  evaluation isolation, pinned upstream contracts, secret flow, and M0 runtime
  probe evidence.
- [TASKS.md](TASKS.md) is the milestone and release checklist from M0 through
  the deterministic end-to-end proof and monitoring work.

## M0 boundaries

All implementation and generated state belongs inside this repository.
`invitro` was consulted only as a structural reference and `agent_bench` only
for workflow archaeology; neither repository was written or changed. The
design record discloses two early read-only `git status` checks there, before
the ARIES-only Git boundary was locked.

Local workflow state, the Terminal-Bench cache, run artifacts, generated
credentials/configuration, and `DEEPSEEK_API.key` are ignored. `AGENTS.md` and
`PROMPT.md` are retained as the repository governance and originating
requirements. Secret values must never enter experiment JSON, results,
evaluator environments, logs, telemetry, or tracked files.

## Current commands

Go 1.22 or newer is required. These commands are local and require neither
Docker nor a model API:

```sh
make build
make test
make test-race
make lint
make integration
```

`make integration` runs the integration-tagged Go test selection. M1 has no
Docker integration case yet, so it currently exercises the same unit-only
packages with the tag enabled. The CLI accepts one strict JSON file:

```sh
./bin/aries configs/openclaw-tb2-fix-git-deepseek.json
```

The example contains the API-key environment variable name only. At M1 this
command exits with the explicit M2-M5 not-implemented error after validation.

## M1 package shape

- `cmd/aries` loads one experiment and owns explicit component-type switches.
- `pkg/config` rejects unknown JSON fields and applies only the visible
  `output_dir: runs` default.
- `pkg/core` contains benchmark-independent tasks, commands, requests, and
  distinct result records.
- `pkg/runner` defines the four substitutable roles and owns lifecycle order.

The Runner positively gates evaluation on successful harness `Stop` and bridge
`Stop`. An ordinary harness `Run` error is retained but does not suppress the
independent evaluator when both gates succeed. A failed gate permanently marks
evaluation `blocked_isolation`; cleanup retries may release resources but never
retroactively expose verifier material. Cleanup uses a fresh bounded context
derived with `context.WithoutCancel`, and joined errors preserve both the
functional failure and cleanup failures.
