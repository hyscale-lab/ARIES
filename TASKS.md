# ARIES Tasks

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
  resources. R2 is ready to commit.

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
- [x] Alias OpenClaw's pinned shared workspace path to the benchmark workdir so
  agent tools and evaluation observe the same live container filesystem.
- [x] Retain private `bridges/<attempt>/tool-calls.jsonl` after bridge shutdown,
  expose it in `TaskResult`, and record redacted command metadata, byte counts,
  timing, exit code, and outcome without content or secret values.
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
  Docker inventory. R1 is ready to commit.

R1 is complete only when the last item is checked with fresh evidence.

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
  revoke without restarting the sandbox, and retain redacted tool evidence.

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
