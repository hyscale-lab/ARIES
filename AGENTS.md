<!-- AUTONOMY DIRECTIVE — DO NOT REMOVE -->
YOU ARE AN AUTONOMOUS CODING AGENT. EXECUTE TASKS TO COMPLETION WITHOUT ASKING FOR PERMISSION.
DO NOT STOP TO ASK "SHOULD I PROCEED?" — PROCEED. DO NOT WAIT FOR CONFIRMATION ON OBVIOUS NEXT STEPS.
IF BLOCKED, TRY AN ALTERNATIVE APPROACH. ONLY ASK WHEN TRULY AMBIGUOUS OR DESTRUCTIVE.
USE CODEX NATIVE SUBAGENTS FOR INDEPENDENT PARALLEL SUBTASKS WHEN THAT IMPROVES THROUGHPUT.
<!-- END AUTONOMY DIRECTIVE -->

# ARIES Agent Contract

ARIES is a small, readable Go benchmark runner. `DESIGN.md` and
`docs/design.md` are the architecture source of truth; `TASKS.md` records
completed work and active cleanup plans.

## Repository boundary

- Read and write only this `aries` repository for ARIES work.
- Run every Git command from this repository.
- `../invitro` is a read-only structural reference.
- `../agent_bench` is read-only workflow archaeology.
- Do not copy either reference repository's architecture or source.
- Keep generated datasets, credentials, run artifacts, and OMX state ignored.

## Design boundaries

The Runner composes exactly four substitutable component roles:

1. `Benchmark`
2. `AgentHarness`
3. `ToolSandbox`
4. `ToolBridge`

Define these interfaces in `pkg/runner`, where they are consumed. Keep shared
`pkg/core` data direct and small. Concrete packages depend on core data, not on
one another, except that a pair-specific bridge may use a narrow capability of
the sandbox it adapts. Construct implementations with explicit constructors
and explicit switches in `cmd/aries`; do not add registration, reflection,
plugin, factory, DI, inheritance, or generic utility frameworks.

The required task lifecycle is:

1. load task;
2. start task sandbox;
3. start bridge;
4. start and run harness;
5. stop harness;
6. revoke bridge;
7. evaluate the still-running sandbox;
8. stop sandbox.

Cleanup is reverse-order and fail-closed. Evaluation is independent of the
harness. Verifier tests and solutions must never be exposed before harness stop
and bridge revocation are positively confirmed. Harness failure and evaluation
outcome remain separate.

## Engineering rules

- Prefer deletion and existing patterns over new abstractions.
- Use the Moby Go SDK for Docker operations; do not shell out to Docker.
- Keep helpers concrete and package-private unless the Runner substitutes them.
- Use `context.Context` for external operations and fresh bounded cleanup
  contexts after cancellation.
- Stop methods are idempotent and positively confirm resource absence.
- Preserve exact argv boundaries; do not construct user commands by string
  concatenation.
- Keep model keys out of JSON profiles, Docker metadata, logs, and results.
- Retain replayable bridge input only in private run artifacts.
- Use Logrus for structured lifecycle logging.
- Add dependencies only when the standard library or an existing dependency is
  insufficient.
- Do not weaken isolation, ownership, credential, or cleanup validation as
  “overly conservative.” Remove only validation that rejects legitimate
  benchmark input without protecting a runtime boundary.

## Change workflow

For each behavior-changing unit:

1. Read `DESIGN.md`, `TASKS.md`, and `git status`.
2. Inspect the relevant implementation and current run evidence.
3. Add a focused regression first when coverage is missing.
4. Make the smallest behavior-preserving or evidence-driven change.
5. Run the smallest real test that proves the claim.
6. Review readability, component boundaries, cleanup, and scope.
7. Update documentation when user-visible behavior or architecture changes.
8. Run release validation once before committing.

Use bounded native subagents only for disjoint implementation, investigation,
or verification slices that materially improve speed or correctness. The lead
owns integration and final verification.

## Validation

Keep these commands working:

```sh
make build
make test
make test-race
make lint
make integration
```

Unit tests and race tests must not require Docker or a paid API. Integration
tests use real local containers and the deterministic fake OpenAI-compatible
endpoint. A live DeepSeek run is optional unless explicitly requested and uses
the ignored repository-root `DEEPSEEK_API.key`.

Before completion, verify the changed behavior, full release checks, no leaked
container/network/process/key, a clean working tree, and a concise commit made
inside `aries`. Report validation gaps explicitly rather than claiming success.

<!-- OMX:AGENTS:START -->
<!-- omx:generated:agents-md -->
# oh-my-codex - Intelligent Multi-Agent Orchestration

AGENTS.md is the top-level operating contract for the workspace. Load installed
OMX skills and role prompts only when the active task needs them; the ARIES
contract above remains authoritative for repository work.

<!-- OMX:RUNTIME:START -->
<!-- OMX:RUNTIME:END -->
<!-- OMX:TEAM:WORKER:START -->
<!-- OMX:TEAM:WORKER:END -->
<!-- OMX:AGENTS:END -->
