# ARIES Tasks

## R19 — Explicit NVIDIA GPU monitoring

1. [x] Reuse the existing occurrence-scoped Recorder lifecycle and
   `ResourceSource` boundary rather than adding a Runner role or independent
   telemetry lifecycle.
2. [x] Add strict optional `monitor.gpu_indices` configuration and select GPU0
   in the checked-in local SGLang profile.
3. [x] Query selected devices through bounded exact `nvidia-smi` arguments and
   retain UUID, utilization, memory, power, and temperature in schema-version-3
   private monitor artifacts.
4. [x] Run focused, race, full release, real-GPU sampling, and cleanup checks.

Completion evidence:

- The production NVIDIA source sampled GPU0 on an H100 NVL and returned its
  stable UUID plus utilization, memory, power, and temperature.
- The production Recorder retained four real GPU samples in a private
  schema-version-3 JSONL/index pair with successful observer status.
- `make build`, `make test`, `make test-race`, `make lint`, and
  `make integration` pass. The checked-in SGLang profile setup succeeds, and
  post-suite inspection finds no ARIES-managed container or network.

## R18 — Managed SGLang shutdown and credential isolation hardening

1. [x] Keep the process-group leader unreaped through a final observer-owned
   KILL sweep, avoid duplicate KILL after successful escalation, and surface
   residual cleanup failures alongside protocol, close, and unexpected-exit
   errors.
2. [x] Let coalesced stop callers detach on their own cancellation, honor caller
   contexts during normal reap and absence confirmation, and bound forced reap
   and confirmation with one fresh five-second context.
3. [x] Remove the configured credential environment entry from the managed
   child without removing prefix-neighbor variables or the executable-first
   PATH contract.
4. [x] Expand strict native YAML boundary and checked-in configuration coverage,
   and document the graceful plus forced shutdown budgets.

Cycle-2 review evidence:

- Startup readiness retries now observe runtime exit during the injected retry
  sleep, publish the runtime's sanitized exit error, join the bounded watcher,
  and never issue another health request; deterministic and 200-iteration race
  regressions lock the handoff.
- SGLang stop results structurally distinguish an unexpected-exit-only result
  from cleanup, protocol, residual-KILL, and log-close failures without changing
  the five-method consumer interface. Application lifecycle logs therefore emit
  `unexpected_exit` plus `stopped` after a naturally exited, positively cleaned
  managed process while preserving the full joined error.
- Documentation distinguishes ARIES structured lifecycle logs, which never
  record environment values, from private child stdout/stderr, which may contain
  non-credential values emitted by the child; the configured credential is
  removed from the child environment before start.

## R17 — Post-runtime-refactor modularity audit and dead-code pruning

Cleanup plan, in evidence-first order:

1. [x] Confirm the refactor retains exactly four Runner roles, explicit command
   switches, the documented `cmd/aries -> internal/app -> pkg/runner` dependency
   direction, and the required lifecycle/isolation regressions.
2. [x] Prove every proposed deletion has no production caller, import
   obligation, or documented current contract; preserve boundary validators
   that protect configuration, ownership, credentials, isolation, or cleanup.
3. [x] Delete only the unused `pkg/model/sglang` chat request/response surface
   and its same-package tests while retaining live model discovery, bounded
   failure categories, URL handling, credential clearing, and HTTP mechanics.
4. [x] Run focused unit, race, vet, package-DAG, architecture, lifecycle, and
   diff checks, then the full release matrix and leak/process/secret checks.
5. [x] Create this audit and cleanup as a standalone commit after `62d98de`.

Initial audit evidence:

- The only references to exported `Message`, `ChatRequest`, `ChatResponse`, and
  `Client.Chat` are their declarations/implementation and same-package tests;
  no production package imports or documentation requires that API.
- `Client.Models`, `Failure` and its categories, `NormalizeBaseURL`, `New`,
  `Close`, and the shared request/response mechanics remain used by application
  preflight and must stay.
- `pkg/runner` still declares only `Benchmark`, `AgentHarness`, `ToolSandbox`,
  and `ToolBridge` as substitutable roles. `Sandbox` remains the live capability
  returned by `ToolSandbox`, not a fifth role.
- `cmd/aries/wiring.go` retains explicit switches for those four roles and the
  model backend. Application orchestration remains under `internal/app`; no
  registration, factory, plugin, reflection, or DI layer is warranted.

Rejected candidates and protected boundaries:

- Do not consolidate explicit switches or introduce a generic deployment or
  runtime registry; the direct switches are the intended extension seams.
- Do not remove or relax strict profile/backend, URL, native-runtime,
  ownership, resource, verifier-secrecy, credential, lifecycle, revocation, or
  positive-absence validation. Those checks defend runtime boundaries rather
  than merely rejecting awkward input.
- Do not prune live SGLang model discovery, failure classification, client
  credential ownership, URL normalization, or HTTP safety mechanics.
- Do not perform formatting-only movement, speculative abstraction, or add a
  dependency; no further modularity defect or dead production surface is
  evidenced.

Completion evidence:

- Removing the chat surface deleted three exported data types, one exported
  method, its two dedicated contract tests, chat-only assertions from two
  shared client tests, and the now-unused test JSON import. No production
  reference remains.
- The post-refactor anti-slop pass rejects ambiguous managed-runtime health
  URLs (including a literal trailing fragment marker), removes dead runtime,
  preflight, and test state, and directly locks health classification and
  bounds, coalesced concurrent shutdown, private/exclusive artifacts,
  partial-start cleanup, and the concrete managed-runtime application
  lifecycle.
- The follow-up ownership audit closes the exit-observation/PID-reuse signal
  window with non-reaping `waitid`, serializes TERM/KILL against observed exit,
  preserves each coalesced stop attempt's immutable result, distinguishes
  retryable per-attempt health timeouts from terminal caller cancellation, and
  removes the unused exported configuration model alias.
- The final simplification removes redundant exit-intention and observer-hook
  state, flattens the single-pass stop path, and replaces scheduler-timing
  assertions with deterministic production-attempt, signal-ownership, and
  reap barriers.
- `go list -deps ./...` and `go vet` pass across all 14 packages. The dependency
  inventory keeps concrete roles separated, with `internal/app` consuming
  `pkg/runner` and the SGLang model-discovery client while the runtime driver
  remains independent of application orchestration.
- Focused unit and race suites pass for `pkg/model/sglang`, `internal/app`,
  `internal/modelruntime/sglang`, `cmd/aries`, and `pkg/runner`, covering the
  explicit switches, runtime lifecycle, model preflight, and Runner isolation
  gates.
- After clearing the Go test cache, `make build`, `make test`,
  `make test-race`, `make lint`, and `make integration` all pass. Post-suite
  inspection finds no ARIES-managed container, network, volume, SGLang process,
  SSH helper process, staged credential, tracked key, or secret-shaped content.

## R16 — Modular managed model runtimes and normalized profiles

1. [x] Reduce `cmd/aries` to CLI process concerns and explicit constructor
   switches; move scheduling, preflight, composition, and persistence into
   `internal/app`.
2. [x] Define the consumer-owned `ModelRuntime` lifecycle/health interface and
   implement managed SGLang under `internal/modelruntime/sglang`, keeping
   inference/model discovery separate.
3. [x] Replace `sglang_file`, `model_runtime`, `model.provider`, and
   `model.model` with the clean-break `runtime.backend/mode/config` and
   `model.id/base_url/api_key_env` schema in every checked-in profile.
4. [x] Preserve the four Runner roles, positive cleanup, verifier secrecy,
   exact argv, private runtime logs, and credential handling.
5. [x] Complete the separate post-refactor extensibility/dead-code audit in its
   own commit.

## R15 — Run-scoped model runtime lifecycle

1. [x] Add strict external/managed model runtime configuration and lock invalid
   provider, executable, timeout, and SGLang-file combinations.
2. [x] Define one narrow runtime interface, retain an explicit
   provider switch, and add a managed SGLang host-process implementation with
   exact argv, private logs, idempotent process-group cleanup, and positive
   absence confirmation.
3. [x] Start the managed runtime before preflight, retry readiness within its
   startup bound, and stop it after all admitted task lifecycle work drains
   using a fresh cleanup context.
4. [x] Preserve external provider behavior, Runner's four roles, evaluation
   isolation, credential handling, and setup behavior.
5. [x] Run focused and full release validation, cancellation and leak checks,
   review the final diff, and record the original lifecycle contribution.

## R14 — Shared local SGLang configuration

- [x] Add a strict native SGLang YAML loader with focused value validation.
- [x] Let profiles reference one reusable YAML file and reject mismatched served
  model names or endpoint ports before setup or runtime side effects.
- [x] Keep remote SGLang endpoint-only operation and the four Runner roles
  unchanged; do not add server or GPU lifecycle management.
- [x] Run focused and full release validation, leak and secret checks, and
  review the final diff.
- [x] Create one concise commit when requested.

## R13 — OpenClaw 2026.7.1 explicit tag

- [x] Add an exact non-`latest` tag-only OpenClaw image validator without
  changing the Terminal-Bench task-image policy.
- [x] Store and launch `ghcr.io/openclaw/openclaw:2026.7.1` exactly, rejecting
  whitespace, missing tags, digests, malformed references, and `latest` at the
  configuration and harness boundaries.
- [x] Update the documented upstream baseline to tag `v2026.7.1`, commit
  `2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4`, and its SSH compatibility
  sources.
- [x] Add regressions for the byte-exact v2026.7.1 workdir-validation,
  ensure-directory, skills-clear, and tar-upload command vectors. Synthesize
  the expected virtual cwd without a filesystem alias, drain only exact skills
  controls, preserve upload evidence, and reject virtual-namespace near matches
  before they can clear or pollute benchmark contents.
- [x] Complete the full release matrix, deterministic integration, authorized
  live DeepSeek compatibility run, retained-log audit, cleanup checks, and
  final review before release.

## R12 — Post-concurrency composition cleanup

Cleanup plan, in regression-first order:

1. [x] Retarget composition coverage to the production
   `buildTaskExperiment` path and occurrence failure coverage to the stable
   `b-002` execution identity while retaining occurrence overflow and formatting
   checks.
2. [x] Delete the test-only `buildExperiment` wrapper, remove the stored
   occurrence index, and privatize package-local experiment fields without
   changing construction, scheduling, lifecycle, cleanup, or isolation behavior.
3. [x] Make `config.ModelConfig` an alias of the identical shared
   `core.ModelConfig` and pass it directly into Runner options while preserving
   strict JSON and credential-field rejection.
4. [x] Run focused unit, race, vet, formatting, and diff checks, then the full
   release matrix and leak, process, secret, status, review, architecture, and
   adversarial-QA gates before the single cleanup commit.

Audit evidence and boundaries:

- `buildExperiment` is referenced only by its same-package composition test;
  production already constructs each occurrence through `buildTaskExperiment`.
- `taskOccurrence.index` is read only by a test; production identity and error
  reporting use the retained global counter and `executionID`.
- `experiment.Runner` and `experiment.Recorder` are package-local composition
  details, and `config.ModelConfig` duplicates `core.ModelConfig` field-for-field.
- Keep the four explicit Runner-role switches, `prepareProfile` validation
  precedence, lifecycle and isolation gates, verifier secrecy, ownership and
  cleanup checks, provider behavior, and SGLang handling unchanged. Do not add a
  registry, factory, plugin, DI layer, shared URL utility, or dependency.

## R9 — Endpoint-only SGLang provider

- [x] Require explicit `deepseek` or `sglang` model provider selection.
- [x] Add a bounded, redirect-rejecting standard-library SGLang Models and
  non-streaming Chat client with sanitized errors and optional bearer auth.
- [x] Add provider-aware preflight and OpenClaw rendering without GPU or server
  lifecycle management, plus a CPU-loadable example profile.

## R8 — Concurrent occurrences and bounded loop admission

- [x] Add strict execution configuration with default concurrency one and an
  optional positive Go-duration loop deadline.
- [x] Preserve ordered duplicates as weights and aggregate results/errors in
  deterministic global occurrence order while bounding active work.
- [x] Compose fresh one-task Runner dependencies per occurrence and use exact
  occurrence IDs for Terminal-Bench private state, artifacts, labels, monitor
  selection, and results.
- [x] Stop loop admissions at the deadline or parent cancellation, then drain
  admitted lifecycle and cleanup work.
- [x] Transfer resource-source ownership once to Recorder and close concrete
  harness/sandbox Moby clients idempotently after lifecycle cleanup.

## R7 — Whole-repository modularity audit and evidence-gated cleanup

Audit baseline: the tree immediately after `fb92446`.

Reproducible evidence:

- [x] `git ls-files | wc -l` reports 70 tracked paths;
  `git ls-files '*.go' | wc -l` reports 54 Go files; and `go list ./...`
  reports 11 packages. `go list -deps ./...` succeeds over 282 dependency
  packages with no cycle or missing package.
- [x] The internal-import edge listing from `go list -f` shows the composition
  root importing the concrete components, while component packages use shared
  core data, consumed Runner interfaces, and the narrow `containerimage`
  helper; no concrete role imports another concrete role and no fifth Runner
  role appears.
- [x] A production declaration-name inventory followed by `git grep -w`
  reverse-reference checks covered 387 distinct function, method, type,
  variable, and constant names. Every name had another tracked-Go occurrence;
  manual review of interfaces, entry points, tests, and build-tagged files found
  no demonstrably dead production symbol or file.
- [x] `go vet ./...` passes. In the restricted execution sandbox,
  `go test ./...` passed every non-bridge package and failed only nine
  listener-dependent bridge tests because `listen tcp4 127.0.0.1:0` returned
  `socket: operation not permitted`; the same command run without that socket
  restriction passes all packages.
- [x] `git ls-files | grep -v '\.go$'` reports 16 non-Go paths: seven root
  contract/build/license/task files, two configs, three docs, two Go module
  files, and two profiles. Reference and key scans tie the configs and profiles
  to loaders, tests, and runnable guidance; the docs, Makefile, ignore rules,
  module metadata, license, and agent contract are intentional repository
  surfaces rather than orphan cleanup candidates.

Architecture and regression locks:

- [x] Keep `cmd/aries.buildTaskExperiment` as the visible per-occurrence
  composition root with one
  explicit type switch per `Benchmark`, `AgentHarness`, `ToolSandbox`, and
  `ToolBridge`. `TestBuildTaskExperimentUsesExplicitTypeSwitches` locks the
  supported constructors and role-specific unsupported-type errors.
- [x] Keep the support checks in `cmd/aries.prepareProfile`.
  `TestPrepareProfileRejectsUnknownComponentsBeforeSetup` proves the entire
  setup profile is rejected before clone, task load, or image pull side effects.
  This deliberate second composition-root touchpoint preserves validation error
  precedence; it does not couple component selection into the Runner.
- [x] Keep Runner lifecycle and evaluation gates locked by
  `TestRunnerSuccessOrdering`,
  `TestRunnerIsolationFailureNeverEvaluatesEvenAfterCleanupRetry`, and
  `TestRunnerBlocksEvaluationWhenFailedHarnessStartCannotBeStopped`.
- [x] Keep bridge and harness credential/revocation checks locked by
  `TestLatePartialStartCleansListenerCredentialsAndAuditWithoutAliasBranches`,
  `TestStartFailureRemovesOnlyContainerAndClearsSecret`, and
  `TestStopFailsUntilContainerAbsenceCanBeConfirmed`.
- [x] Keep Docker identity and positive-absence checks locked by
  `TestManagerStopRejectsNilAndForeignSandbox`,
  `TestStartUsesTypedOptionsAndStopIsIdempotent`, and
  `TestExecCancellationReturnsTerminationConfirmationFailure`.
- [x] Keep strict configuration and checked resource conversion locked by
  `TestDecodeRejectsInvalidConfig`,
  `TestLoadRuntimeOverridesStrictSparseAndChecked`,
  `TestValidateEnvironmentRejectsResourceConversionOverflow`, and
  `TestHarnessRejectsInvalidResourcesBeforeContainerCreate`.
- [x] Keep verifier secrecy and task/path safety locked by
  `TestLoadFixGitMapsGenericTaskAndKeepsVerifierPrivate`,
  `TestRecursiveVerifierTreeStaysPrivateUntilEvaluate`,
  `TestPrepareSandboxRemovesThenProvesVerifierPathsAbsent`, and
  `TestNewRejectsUnsafeAndDuplicateTasks`.

Cleanup decision and ordered completion gates:

1. [x] Inventory source, dependencies, symbols, reverse references, non-Go
   surfaces, and behavior locks before proposing a source edit.
2. [x] Require focused regression evidence for every deletion or boundary
   change. No candidate met that threshold, so Task 5 remains a
   documentation-only audit result with no production or test churn.
3. [x] Run Task 5 pre-commit audit and diff checks, confirming only this R7
   record changed and no generic framework, dependency, or cosmetic churn was
   introduced.
4. [ ] Create the dedicated third change commit after `c7455eb` and `fb92446`
   with the truthful message `chore: record whole-repository cleanup audit`.
5. [ ] After all three change commits exist, run `make build`, `make test`,
   `make test-race`, `make lint`, and `make integration`; do not count the
   audit-only vet/unit evidence above as the full release gate.
6. [ ] Confirm no leaked container, network, process, staged credential, or key;
   run secret scans; verify the exact three-commit history and clean tree. Record
   these post-commit facts only in the external Ultragoal ledger/final report,
   with no fourth bookkeeping commit.

Rejected candidates and rationale:

- Removing an unreferenced-looking production declaration or file was rejected
  because the whole-repository symbol audit found no demonstrably dead one.
- Collapsing component construction into registration, factory, plugin, DI, or
  generic utility machinery was rejected because the four explicit switches
  already provide the required extension seams with less indirection.
- Deduplicating `prepareProfile` support checks was rejected because their
  preflight position prevents side effects and fixes validation error order.
- Deleting or relaxing validators was rejected where the checks defend runtime
  isolation, ownership, credentials, resource bounds, lifecycle, or cleanup.
- Pruning profiles, docs, Make targets, or ignore entries was rejected because
  the audit tied each item to an intentional user, release, or artifact surface.
- Formatting-only movement, speculative abstractions, and new dependencies were
  rejected because they add review risk without evidence of dead code or a
  modularity defect.

## R6 — Runtime isolation, sparse overrides, and raw SSH audit

- [x] Add optional dedicated strict-JSON runtime overrides with checked sparse
  CPU, memory, and agent-timeout conversion; retain an inactive one-task profile
  and apply the five-task profile's present resources to both containers.
- [x] Inject ARIES-owned task-container timezone and noninteractive environment
  values without adding environment values to lifecycle metadata.
- [x] Add benchmark-owned pre-harness removal and positive absence proof for
  verifier paths, with fresh pinned-checkout reverification and post-revocation
  verifier injection.
- [x] Retain correlated structured and exact-wire raw SSH evidence through one
  asynchronous, combined-budget, fail-closed writer.
- [x] Pass focused configuration, Runner, Docker, OpenClaw, Terminal-Bench, and
  non-listener bridge unit/race tests plus formatting and diff checks.
- [x] Complete privileged loopback SSH, Docker integration, full release, leak,
  and secret scans before the release commit.

## R5 — Live five-task reliability

- [x] Diagnose the five-task live run without treating reward `0` as an ARIES
  failure: `overfull-hbox` had bridge calls delayed up to 104 seconds and timed
  out, while `schemelike-metacircular-eval` accumulated 114–315 second calls
  before OpenClaw's 772-second stuck-session recovery triggered a takeover.
- [x] Remove sandbox-wide Docker exec serialization while retaining the
  existing token-correlated exit trailer and positive Docker process check.
- [x] Add a real concurrent SSH regression proving eight parallel tool calls,
  confirmed revocation of four active calls, and evaluator access to the
  unchanged live sandbox.
- [x] Disable OpenClaw native filesystem tools for arbitrary Terminal-Bench
  images that do not provide the helper's undeclared `python3` dependency;
  keep unmodified upstream OpenClaw and shell access through SSH exec.
- [x] Derive the run directory from the experiment profile name rather than the
  first task and task count.
- [x] Replace the stale research-oriented `AGENTS.md` with the current ARIES
  engineering, boundary, lifecycle, and verification contract.

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
- [x] Alias OpenClaw's pinned shared workspace path for shell commands so agent
  tools and evaluation observe the same live container filesystem; disable the
  upstream Python-dependent filesystem helpers instead of translating a second
  protocol.
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
