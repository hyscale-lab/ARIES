# ARIES

ARIES is a small, human-readable Go benchmark runner under construction. Its
first complete path will run the pinned Terminal-Bench 2 `fix-git` task through
an unmodified upstream OpenClaw container, OpenClaw's SSH sandbox backend, one
local Docker task sandbox, and an evaluator that runs only after harness access
has been revoked.

## Status

M0 establishes the accepted architecture, upstream compatibility pins,
repository boundaries, and implementation checklist. No runnable Go benchmark
system exists yet; build, test, setup, and run commands will be documented only
when their implementations land in later milestones.

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
