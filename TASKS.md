# ARIES Tasks

## R4 — Repository cleanup and generic Terminal-Bench 2

Cleanup plan, in regression-first order:

1. [x] Lock current lifecycle, `fix-git`, secret handling, monitoring, and
   evaluation behavior; add dataset-backed tests for a heterogeneous five-task
   selection, recursive verifier trees, generic CTRF results, and all tasks in
   the pinned Terminal-Bench 2 revision.
2. [x] Delete linker-confirmed dead command wrappers, unreachable monitor
   artifact state, speculative `Environment.BuildDir`, redundant verifier
   snapshot machinery, obsolete prompt provenance, and empty source folders.
3. [x] Remove validations that reject legitimate inputs without protecting a
   runtime boundary: fix-git-only task values, descriptive TOML additions,
   exact CTRF test names/counts, executable-layout checks, exact `0600` secret
   mode, and uppercase-only environment names.
4. [x] Keep grounded fail-safe behavior that positively confirms container,
   process, bridge, or network cleanup. Replace the masking optional-telemetry
   string match with typed Docker not-found classification.
5. [x] Replace the single fix-git image pin with a data-only source-to-digest
   catalog covering the pinned dataset; keep task selection opaque and ordered
   in Go code.
6. [x] Load generic task resources, workdir, instruction, timeout, and complete
   private `tests/` tree; pass the task timeout through Runner to the harness;
   keep solutions private and evaluation independent.
7. [x] Add a checked-in five-task profile covering alternate workdirs,
   resources, timeout shapes, optional metadata, and nested verifier files.
8. [x] Run build, unit, race, lint, dataset, Docker integration, deterministic
   OpenClaw E2E, review, and adversarial QA; update documentation and commit a
   clean tree with no managed resources.

Fallback inventory:

- OpenClaw and Docker cleanup recovery is a grounded fail-safe because absence
  is positively confirmed and error evidence is retained; keep it.
- The bridge's two accepted OpenClaw temporary-root layouts are a narrow
  compatibility path; keep it and add the missing regression case.
- Optional telemetry's broad `"not found"` error match is masking fallback
  slop; replace it before other cleanup passes.

## R3 — Resource accuracy and replayable artifacts

- [x] Keep Docker Stats instead of adding cAdvisor; move Docker collection into
  the sandbox package behind monitor's runtime-neutral `ResourceSource`.
- [x] Derive CPU percentages from cumulative counters across samples, publish
  schema-v2 runtime fields, and add a real busy-container assertion.
- [x] Retain exact bridge argv, shell command, stdin encoding/content, duration,
  byte counts, and outcomes; do not fabricate OpenClaw tool-call IDs that never
  cross SSH.
- [x] Retain the placeholder-only rendered `openclaw.json`, use upstream
  read-write SSH workspace access, and eliminate the gateway write failure.
- [x] Use task-readable run/component paths and structured Logrus console plus
  private run-file logging.
- [x] Require Go 1.26.5 and document cross-harness bridge alternatives without
  implementing speculative adapters.
- [x] Pass build, unit, race, lint, integration, deterministic OpenClaw E2E,
  review, and adversarial QA with fresh evidence and no resource leaks.

## R2 — Lifecycle ownership, version files, and runnable documentation

- [x] Put sandbox shutdown on `ToolSandbox` while keeping the returned
  `Sandbox` limited to exec and file transfer capabilities.
- [x] Move upstream revision and image pins into strict
  `configs/versions.json`; require concrete packages to receive them through
  constructors without hardcoded fallbacks.
- [x] Move the runnable experiment into `profiles/` without adding inheritance
  or merging.
- [x] Make `aries setup PROFILE.json` prepare the exact dataset checkout and
  required images through the Moby SDK.
- [x] Add `docs/quick-start.md` and `docs/design.md`, and document why the
  SSH-to-Docker-exec bridge cannot be only a raw relay.
- [x] Treat a trusted container that exits between monitor list and inspect as
  a normal lifecycle race while preserving strict identity and label checks.
- [x] Pass the complete release checks, obtain review approval, run the
  checked-in setup path, and complete a live DeepSeek run with reward `1`, 17
  completed bridge tool records, successful monitoring, and no leaked Docker
  resources.

## R1 — Docker SDK, host SSH bridge, and simplification

User review identified three issues in the first implementation: Docker access
was unnecessarily low-level, the SSH server executed inside the task container
instead of translating requests to Docker exec, and transport-specific code and
tests obscured the design. R1 replaces that implementation without changing the
four Runner component boundaries or the independent evaluator.

- [x] Replace Docker CLI parsing and hand-written Engine HTTP in the sandbox,
  harness, and monitor with the pinned official Moby client/API modules and
  small package-private typed client subsets.
- [x] Delete the task-container exec helper, private socket/PID protocol,
  uploaded SSH server, raw Docker transport, harness initialization volume, and
  their implementation-specific lifecycle machinery.
- [x] Run the SSH listener in the host ARIES bridge and translate every accepted
  SSH exec request into `ExecStream` on the exact Docker sandbox passed to the
  bridge.
- [x] Preserve binary and late stdin, separate stdout and stderr, exact exit
  codes, atomic stream byte counts, dynamic endpoint configuration, strict
  host-key verification, and syntactically valid upstream environment names,
  including `ARIES_*`.
- [x] Run each tool command in a private process group; on cancellation use
  typed Moby detached termination plus `ContainerTop` absence proof without
  stopping, restarting, or recreating the sandbox, and propagate confirmation
  failure so the Runner blocks evaluation.
- [x] Alias OpenClaw's pinned shared workspace path for shell commands and map
  native filesystem roots bidirectionally so agent tools and evaluation observe
  the same live container filesystem.
- [x] Retain private `<task>/bridge/tool-calls.jsonl` after bridge shutdown,
  expose it in `TaskResult`, and record replayable command/stdin inputs, byte
  counts, timing, exit code, and outcome without model credentials.
- [x] Add a fake-executor bridge contract test, a real Docker bridge mutation
  test, and a deterministic pinned-OpenClaw E2E assertion requiring completed
  bridge tool records, monitor samples for both container kinds, and reward `1`.
- [x] Remove duplicated release oracles and tests that only defended deleted
  transports; keep focused tests at component and behavior boundaries.
- [x] Keep documentation compact and current; pass `make build`, `make test`,
  `make test-race`, `make lint`, and `make integration` with reward `1`, host
  SSH tool records, cancellation without sandbox restart, both monitor kinds,
  and isolation/cleanup confirmed; report no reachable or imported
  `govulncheck` vulnerability; obtain independent code-review approval,
  architect clearance, and a 20/20 UltraQA cancellation cycle with an empty
  Docker inventory.

## MVP baseline — completed before R1

### M0 — Evidence and repository boundary

- [x] Record `aries` as the only write and Git-operation boundary.
- [x] Use `invitro` only for structure and `agent_bench` only for workflow
  archaeology; disclose the two early read-only status checks.
- [x] Pin the Terminal-Bench task/image and the upstream OpenClaw source/image.
- [x] Verify OpenClaw's SSH config, non-TTY argv, workspace formula, readiness,
  and one-turn command against the pinned source and image.

### M1 — Core Runner and configuration

- [x] Initialize the small `cmd` plus `pkg` Go layout and strict JSON
  configuration with explicit component switches.
- [x] Define direct core data and exactly four main interfaces in `pkg/runner`.
- [x] Implement ordered lifecycle, reverse cleanup, cancellation, isolation
  gates, separate outcomes, and focused fake-component tests.

### M2 — Terminal-Bench 2 adapter

- [x] Clone and verify only the pinned dataset revision.
- [x] Parse `fix-git` into generic `Task` and `Environment` values and reject
  unsupported execution-critical fields.
- [x] Keep verifier metadata private, inject tests only after isolation, run the
  verifier independently, and parse CTRF and reward artifacts.

### M3 — Local Docker sandbox

- [x] Start one labeled task container and network from the generic environment.
- [x] Implement typed exec, streaming exec, archive upload/download, log capture,
  positive inspection, idempotent cleanup, and no-leak integration coverage.

### M4 — OpenClaw SSH bridge

- [x] Implement the pinned `aries-ssh` client contract, ephemeral credentials,
  strict host verification, canonical command decoding, and cancellable sessions.
- [x] Proxy SSH exec to the same Docker sandbox, map the shared workspace,
  revoke without restarting the sandbox, and retain tool evidence.

### M5 — Upstream OpenClaw harness

- [x] Render task-local config for the remote model and upstream SSH backend.
- [x] Run one unmodified pinned OpenClaw container, await readiness, send one
  instruction, collect the response/logs/telemetry, and remove the container.
- [x] Test with a deterministic OpenAI-compatible endpoint and keep secrets out
  of Docker metadata and retained artifacts.

### M6 — Independent evaluator

- [x] Require confirmed harness stop and bridge revocation before verifier
  injection while keeping the task sandbox alive.
- [x] Preserve harness and evaluation outcomes independently and prove a valid
  sandbox is evaluated after ordinary harness failure.
- [x] Run the real pinned verifier and require the expected `fix-git` reward.

### M7 — Monitoring and end-to-end path

- [x] Compose the observer outside the Runner and record one-second CPU/memory
  samples for the task and OpenClaw containers.
- [x] Retain sandbox, harness, evaluation, monitor, result, and available
  upstream trajectory artifacts.
- [x] Add secure ignored-file DeepSeek credential loading and bounded official
  model preflight without exposing key values.
- [x] Run the complete deterministic local path and document the explicit
  one-package plus one-switch extension rule.
