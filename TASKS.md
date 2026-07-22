# ARIES MVP Tasks

Execution baseline: the RALPLAN-DR plan is approved. A milestone is complete
only when its focused checks, smallest real integration, independent review,
documentation, cleanup, and identifiable ARIES-only commit are complete.

## M0 — Evidence, promotion, and repository baseline

- [x] Confirm `aries` as the write boundary and the Git boundary going forward;
  disclose the two earlier read-only `git status` checks in `invitro` and
  `agent_bench`, which changed no files.
- [x] Recheck every pinned TB2 and OpenClaw permalink in `DESIGN.md` and
  execute the pinned OpenClaw image facts: digest, non-root user, `/readyz`,
  agent JSON command, and missing system `ssh`.
- [x] Lock the exact OpenClaw config, non-TTY SSH argv, canonical exec-request
  grammar, session workspace formula, and safe workdir alias. Any mismatch
  returns to RALPLAN-DR.
- [x] Rerun Architect with explicit steelman, antithesis, tradeoff tension, and
  synthesis; then rerun Critic. Both must approve the same plan revision.
- [x] After an explicit execution handoff, promote the approved design and task
  drafts to `DESIGN.md` and `TASKS.md` without semantic drift.
- [x] Add `.gitignore` entries for `.omx/`, `.cache/`, `runs/`, the generated
  temp/config/key root, and `DEEPSEEK_API.key`.
- [x] Review `AGENTS.md` and `PROMPT.md` for secrets and deliberately track both
  as governance and originating requirements.
- [x] Record the clean baseline with `git status --short` and `git ls-files` and
  commit `docs: record ARIES architecture and execution plan`.

## M1 — Go skeleton, config, results, and core Runner

- [x] Initialize `github.com/hyscale-lab/aries`, requested layout, Makefile, and
  strict one-file JSON config with explicit switches.
- [x] Add direct core data and exactly four main Runner interfaces; keep
  `Sandbox` a returned capability.
- [x] Implement exact task lifecycle and separate harness, isolation,
  evaluation, observer, and cleanup outcomes.
- [x] Make every failed `Start` roll back partial acquisition before returning.
  Use a bounded non-cancelled cleanup context and join primary plus cleanup
  errors while preserving the primary error identity.
- [x] Positively verify harness termination and bridge revocation before any
  evaluator injection; otherwise record `blocked_isolation` and clean sandbox.
- [x] Unit-test ordering, real cancellation at every lifecycle cut point,
  partial Start, cleanup timeout, joined errors, repeated Runner runs, and
  result aggregation. Race-safe idempotent concrete `Stop` methods remain
  claim-bearing M3-M5 obligations.
- [x] Commit `feat(core): add task runner contracts`.

## M2 — Pinned Terminal-Bench 2 adapter

- [x] Clone only commit `2fd12b88aafdd04a52c298e3940bcb189f9766d6`
  into `.cache/terminal-bench-2` and verify the revision.
- [x] Parse the real `fix-git` task into generic `Task` and `Environment`; pin
  its image digest and reject unsupported execution-critical fields.
- [x] Keep verifier, tests, digests, stable file metadata, and solution private;
  after both isolation gates pass, clear fixed evaluator paths and inject only
  revalidated enumerated verifier files.
- [x] Parse verifier output and require successful reward exactly `1`; retain
  stdout, stderr, CTRF, and reward artifacts.
- [x] Commit `feat(terminalbench): load pinned fix-git task`.

## M3 — Local Docker sandbox

- [x] Implement one task container, scoped network, workdir mapping, exec,
  transfers, logs, labels, positive inspection, and idempotent stop and remove.
- [x] Build and read-only mount the static exec helper; authenticate its exact
  daemon-issued PID with `SO_PEERCRED`, bound its framed I/O, and positively
  confirm helper exit and socket absence without trusting stale `ExecInspect`
  completion state.
- [x] Roll back container, network, mounts, and processes after each partial
  Start failure with the bounded cleanup policy.
- [x] Prove argument safety, cancellation, workdir identity, and zero leftover
  labeled container, network, private runtime, mount, or process.
- [x] Commit `feat(sandbox): add local docker execution` after independent M3
  review.

## M4 — OpenClaw SSH bridge

- [x] Implement the static helper's exact client argv and pinned config parser;
  reject TTY, unknown or reordered options, forwarding, proxies, subsystems,
  extra destinations, and unsupported directives. Prove the mode-0555 client
  and private UID-1000-owned credential/config volume in the pinned image.
- [x] Implement the server's canonical quoted-argv decoder. Execute decoded argv
  directly; never feed the raw SSH exec string to a fallback shell.
- [x] Accept only the task public key and exec channel. Reject password and
  keyboard-interactive auth, malformed quoting, non-allowlisted env names, PTY,
  forwarding, agent and X11 forwarding, environment requests, subsystems, and
  cross-task reachability; bound connections, handshakes, requests, and
  cancellation cleanup.
- [x] Pre-create the pinned session workspace alias to the real task workdir;
  reject symlinked/overlapping ancestry, ownership-gate cleanup with an
  atomic root leaf plus a private per-attempt marker, recover every exact
  partial state, and prove OpenClaw and evaluator see the same inode and byte
  delta.
- [x] Revoke by always restarting the sandbox, killing SSH descendants,
  proving the old PID/listener/signer absent, deleting credentials, and
  positively checking each condition.
- [x] Commit `feat(bridge): connect openclaw over verified ssh`.

## M5 — Pinned unmodified OpenClaw and deterministic model

- [x] Render the locked task-local config without API-key values; mount static
  client, identity path, and known-hosts path read-only.
- [x] Start the pinned image, await `/readyz`, then run exactly one task turn via
  `node openclaw.mjs agent --session-key TASK_SESSION --message INSTRUCTION
  --json --timeout SECONDS`.
- [x] Build a strict fake OpenAI-compatible state machine that sees no task
  filesystem or Docker socket, rejects unexpected requests, and returns real
  OpenClaw tool calls only after matching the prior tool result.
- [x] State machine: inspect Git status and reflog; inspect the candidate lost
  commit from returned output; merge that dynamic hash into master; verify Git
  state; return a final response. It cannot pre-edit, mount, inject verifier
  data, or bypass OpenClaw.
- [x] Persist redacted model transcript, OpenClaw tool call and result transcript,
  pre/post filesystem and Git delta, and harness logs.
- [x] Preserve upstream `tini`; positively prove nonce/ID ownership, bounded
  graceful exit with KILL fallback, non-running state, removal, and empty
  ARIES-labeled container, volume, and network inventories. Retain failed-Start
  tentative or proven ownership whenever bounded inspection or rollback cannot
  finish, retry transient not-found responses through the proof window, and
  reacquire cleanup ownership only after exact attempt-label proof.
- [x] Commit `feat(harness): run pinned upstream openclaw` after independent M5
  review.

## M6 — Independent evaluator and deterministic `fix-git` proof

- [x] Require confirmed harness termination and bridge revocation, then inject
  the private pinned verifier into the still-live sandbox.
- [x] Run evaluator with independent command, timeout, environment, and logs;
  require reward exactly `1` and both pinned file-hash tests to pass.
- [x] Produce one immutable **M6 functional-oracle manifest** linking model and
  tool transcripts, filesystem delta, verifier output, CTRF, reward, isolation,
  cleanup, and resource inventory, with observer status `not_enabled`.
- [x] Prove ordinary harness failure is evaluated only after isolation succeeds;
  failed isolation injects nothing and remains a separate outcome.
- [x] Commit `feat(eval): verify fix-git independently`; M7 must not mutate the
  M6 manifest.

## M7 — Observer, live-key loader, and monitored release proof

- [x] Start concrete `monitor.Recorder` before `Runner.Run` and stop it after
  Runner cleanup with the bounded cleanup context. It observes run labels and
  never controls lifecycle or scoring.
- [x] Record existing OpenClaw and sandbox logs, one-second CPU and memory
  samples for both containers, and available upstream trajectory; test observer
  partial Start, sampling failure, and retryable Stop without lifecycle control.
- [x] Implement the fixed ignored `DEEPSEEK_API.key` loader anchored to the
  trusted repository `bin/aries` layout. Require current ownership, mode 0600,
  no symlink, stable metadata, and bounded single-line bytes; use the named
  environment fallback only when the file is absent, and never expose the value
  in config, argv, Docker metadata, logs, results, task, or evaluator.
- [x] Authenticate and bound the official DeepSeek `/models` preflight before
  Docker construction, retry only transport/500/503 once after two seconds,
  require the exact official model, and disable thinking only for exact official
  DeepSeek V4 requests.
- [x] Run a fresh deterministic reward-`1` E2E with monitoring and write a
  separate immutable **M7 monitored release manifest** with observer samples and
  outcome. The fresh proof passed both exact verifier cases with reward `1`, 73
  one-second samples covering both container kinds with shared seconds, 27
  hashed artifacts, and no resource leaks. Ordinary integration preserves zero
  or more ignored M6 manifests; the final workspace audit used
  `ARIES_REQUIRE_CANONICAL_M6=1` and preserved the expected canonical hash.
- [x] Pass canary, redaction, exact-byte, tracked-file, task-sandbox, config,
  log, result, telemetry, temp-tree, and empty-resource scans.
- [x] Record optional live DeepSeek evidence with one confirmed preflight,
  reward `1`, 103 one-second samples, confirmed isolation and cleanup, and no
  final ARIES Docker resource.
- [x] Update README with setup, exact run commands, artifact interpretation,
  architecture, and the one-package plus one-explicit-switch extension path.
- [x] Freshly pass `make build`, `make test`,
  `make test-race`, `make lint`, and `make integration`; update DESIGN and README,
  close every MVP checkbox in TASKS, and review the complete diff.

## Read-only release verification

The final release audit also confirms:

- `git status --porcelain` is empty;
- `git ls-files` contains no `.omx`, cache, run, temp config, key, or
  `DEEPSEEK_API.key` path;
- `git log` identifies one M0 through M7 commit;
- immutable M6 and M7 manifests read back with reward `1`, the expected observer
  states, isolation evidence, and empty resource inventories;
- README contains all five acceptance topics and TASKS has no open MVP box;
- neither reference repository has any ARIES write or changed file; the audit
  retains the disclosed two early read-only `git status` checks and reports no
  later Git operation there.
