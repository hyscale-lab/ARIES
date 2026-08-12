# ARIES Tasks

## R22 — Centralized E2B-like OpenClaw sandbox bridge

Plan, regression-first:

1. [x] Pin the OpenClaw v2026.7.1 extension and backend contract from official
   source before production integration. Evidence is locked in
   `pkg/harness/openclaw/testdata/v2026.7.1-sandbox-plugin-contract.json` and
   `plugin_contract_test.go` against signed tag object
   `842a951d5d0843aa6eb77575dc9867bf0603835c`, commit
   `2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4`, and byte hashes of the exact
   upstream SDK, backend, filesystem, config, tool, and OpenShell reference
   files. The supported seam is a full-mode local plugin calling
   `registerSandboxBackend`, selected by `agents.defaults.sandbox.backend`,
   returning a `SandboxBackendHandle` whose attached command path is
   `buildExecSpec` plus a local client argv. Native `read`/`write`/`edit` use
   `SandboxFsBridge`; `apply_patch` remains disabled for the initial path. The
   task grant token is staged in a private file named by nonsecret plugin
   config because v2026.7.1 plugin entries have no SecretRef field. Version
   discrepancies are explicit in the fixture: upstream has no built-in E2B
   backend, direct Process.Start/SendSignal plugin methods, typed original argv,
   or filesystem ListDir method. The ARIES REST routes, headers, streaming
   framing, signal client, and ListDir implementation therefore remain ARIES
   protocol work for later R22 steps rather than invented upstream APIs.
2. [x] Preserve the Runner's four roles and existing isolation order. The
   `ToolBridge` signature already models a task-scoped grant adapter, so it is
   unchanged: `Start` receives the exact live sandbox and `Stop` remains the
   idempotent positive revocation gate. Focused regressions lock
   the exact sandbox capability, harness-stop -> bridge-stop -> evaluation
   order, partial-Start cleanup, and permanent evaluation blocking after an
   isolation failure. Application composition now names the occurrence seam
   `TaskBridgeFactory` and passes a run-bound factory explicitly into
   `buildTaskExperiment`; current wiring binds the same OpenClaw SSH or Hermes
   SSH constructor, while a future application-scoped service can bind a method
   that returns distinct grant adapters sharing one owner. This adds no Runner
   role, listener, registry, protocol, or service framework. The pinned
   OpenClaw `buildExecSpec(command: string, ...)` limitation remains explicit:
   this seam neither promises original typed argv nor parses the shell command
   to reconstruct it.
3. [x] Establish application-scoped centralized bridge ownership before task
   admission and stop it with a fresh bounded context after all admitted
   occurrences drain. `PreparedBridge` carries the run-bound
   `TaskBridgeFactory` plus its optional infrastructure `Stop`; SSH and Hermes
   return their unchanged occurrence constructor with no shared-service stop.
   The new `openclawe2b.Server` owns exactly one
   `net.ListenConfig.Listen(ctx, "tcp4", "0.0.0.0:0")` listener, rejects every
   request as unavailable until authenticated dispatch exists, and idempotently
   shuts down HTTP serving while positively waiting for `Serve` to exit. Task
   endpoints will substitute each owned Docker-network gateway for the wildcard
   address while retaining the one shared port in later steps. Focused and race
   regressions lock one prepare per run, service lifetime around occurrence
   cleanup, wildcard TCP4 ownership, startup failure, repeated stop, and
   listener absence. No published port, shared ingress network, host networking,
   bridge container, Docker socket exposure, authentication, or protocol
   endpoint was added.
4. [x] Make `openclaw-e2b` a real explicitly selected prepared bridge. One
   `openclawe2b.Server` starts per E2B run and supplies distinct occurrence
   `Grant` adapters while SSH and Hermes construct no centralized server. Each
   grant binds the exact Docker sandbox, its IPv4 network gateway, a random
   128-bit sandbox ID, and the SHA-256 verifier for a random 256-bit access
   token in the concurrency-safe `sandboxID -> registration` map. Token bytes
   live only in a private `0600` task artifact staged as
   `/run/aries/e2b/access.token`; the endpoint carries the sandbox ID, token
   source/target paths, task network, and
   `http://<task-gateway>:<shared-port>`, never token bytes. `ConnContext`
   retains the accepted socket's local destination and authorization validates
   that IP, `E2b-Sandbox-Id`, and `X-Access-Token` before reading the body or
   dispatching. Authenticated routes deliberately return not implemented.
   Revocation atomically changes active -> revoking and zeroes the verifier
   before waiting for admitted requests, then removes the registration, marks
   it revoked, confirms map absence, and removes the token file; retries remain
   idempotent and fail closed. Focused/race regressions cover shared port and
   distinct grants, cross-task/missing/wrong/unknown/revoked credentials,
   destination mismatch, pre-body rejection, independent revocation, admitted
   request drain, partial Start cleanup, explicit selection, one server per
   preparation, SSH/Hermes exclusion, and concurrent grant churn.
5. [x] Implement `POST /v1/process/start` as an attached NDJSON stream through
   the pair-specific Docker `ExecProcessStream` capability without changing the
   existing SSH/Hermes `ExecStream` path. The REST `cmd` and `args` remain typed
   argv. A separate wrapper launches a `setsid` child that writes its own `$$`
   group-leader PID to the unique exec state file and a token-bound private
   stderr frame, then blocks on fd 3. ARIES consumes and validates that complete
   frame, emits and flushes exactly one start event, invokes the callback, writes
   `ARIES_EXEC_START_<token>` on the private Docker attach input, and closes that
   input before the child executes. Token-matched PID and exit framing is
   filtered while arbitrary binary and PID-looking user stdout/stderr remains
   byte-exact. Each raw chunk becomes base64 JSON data, nonzero exit remains a
   normal final end event, and post-start errors/cancellation are represented in
   that final event. The attached request context retains the existing targeted
   TERM -> KILL -> positive-absence cancellation path and never stops the task
   container. Unit/race regressions cover authentication before parsing,
   malformed and pre-start failure, one start before output, fragmented private
   framing, short simulated execution, binary separated multi-chunk output,
   zero/nonzero result framing, post-start errors, cancellation, concurrent
   starts within/across grants, exact argv/env, and unchanged legacy Docker exec
   tests. A build-tagged real-Docker regression compares the callback PID with
   child `$$` and covers `/bin/true`, but Docker is unavailable in this
   environment; execution of that test remains explicitly pending for R22 step
   10 integration validation and is not claimed here.
6. [x] Implement `POST /v1/process/send-signal` and sandbox-scoped active
   process ownership. The centralized map keys by `(sandboxID, childPID)` and
   retains the exact registration, pair-specific Docker `ProcessRef`, sandbox
   capability, and a monotonic occurrence generation; completion deletes only
   the identical entry pointer, so an older completion cannot erase a reused
   PID generation. Docker keeps the reference's exec ID, unique state path, and
   random generation private. `SendProcessSignal` accepts only that reference
   plus `SIGNAL_SIGTERM` or `SIGNAL_SIGKILL`; its in-container helper re-reads
   the unique state file, requires its group leader to equal the reported PID,
   and signals that exact negative process group. Authentication still precedes
   body reads. Success is `{"ok":true}` JSON; malformed/unsupported requests,
   unknown/exited/cross-sandbox PIDs, stale references, and helper failures are
   structured fail-closed errors. Process registration occurs before the start
   event/callback returns and therefore before the private launch handshake;
   completion is generation-safe. Revocation first invalidates the token and
   process admission, applies the existing targeted TERM -> KILL -> confirmed
   absence cleanup to every process in that registration, drains admitted
   attached requests, confirms the sandbox process set is empty, then removes
   the registration. Server shutdown uses the same path for every grant.
   Focused/race regressions cover pre-body authentication, TERM/KILL, unsupported
   and unknown/exited processes, equal PIDs in isolated sandboxes, generation
   reuse, registration-before-release, completion, selective revocation,
   shutdown cleanup, concurrent starts and signal/revocation state, exact Docker
   helper arguments, and unchanged legacy exec coverage. Docker is unavailable
   here, so real-container signal delivery and PID-generation behavior remain
   pending for R22 step 10 and are not claimed.
7. [x] Implement raw `GET/POST /v1/files?path=...` plus
   `POST /v1/filesystem/{stat,list-dir,make-dir,remove,move}` through a narrow
   Docker filesystem capability. Authentication by destination, sandbox ID,
   and token still completes before any body read or sandbox dispatch. Raw
   reads/writes preserve binary and zero-length content, are bounded at 64 MiB,
   use Docker archive copy directly, create parents, and never materialize a
   host path. Stat/list metadata comes from Docker archive stat/tar headers and
   returns path, name, `file|directory|symlink|other`, size, octal permissions,
   modification time when present, and link target when applicable; listing
   filters and sorts exactly one directory level. Parent creation, recursive
   removal, and rename use the existing attached Docker exec implementation
   with exact argv to `/bin/mkdir`, `/bin/rm`, and `/bin/mv`; user paths are
   never shell-concatenated. Paths must be absolute, NUL-free, and already
   normalized. Every modifying operation rejects `/`, while dirty normalized
   equivalents such as `/.` and `/workspace/..` fail validation before Docker.
   Archive reads reject symlinks as raw files; stat/list report them without
   following them. Docker extraction and exact-argv utilities retain normal
   in-container symlink semantics, confined to the exact unmounted task
   sandbox. Focused/race regressions cover pre-body authentication, binary and
   zero-byte create/overwrite, parents, file/directory/missing stat, depth-one
   listing, recursive mkdir/remove, root variants, move, malformed requests,
   sandbox isolation, revocation, concurrent operations, archive metadata, and
   exact helper argv. No lifecycle, PTY, stdin, reconnect, detached execution,
   code execution, ConnectRPC, or unrecognized protocol route was added.
   Docker is unavailable here, so real-container archive, rename, symlink, and
   recursive-removal behavior remains pending for R22 step 10 and is not claimed.
8. [x] Implement and stage the pinned OpenClaw v2026.7.1 local `aries-e2b`
   sandbox backend plugin. Its full-mode entry calls
   `registerSandboxBackend("aries-e2b", {factory, resolveWorkdir})` with no
   manager or lifecycle surface and returns the exact pinned
   `SandboxBackendHandle`. `buildExecSpec` accepts the upstream command string
   as-is and returns the local `helper.mjs` argv, task workdir/environment, and
   `stdinMode: "pipe-closed"`; the helper sends explicit
   `/bin/bash -lc <command>` argv to attached `Process.Start`, decodes NDJSON
   binary stdout/stderr, propagates the final exit code, and aborts the request
   on termination. It exposes no PTY or user stdin. `runShellCommand` uses the
   same client, preserves its explicit script arguments, returns pinned
   `{stdout: Buffer, stderr: Buffer, code}`, honors `allowFailure` and
   `AbortSignal`, and rejects nonempty stdin. `createFsBridge` implements only
   `resolvePath`, `readFile`, `writeFile`, `mkdirp`, `remove`, `rename`, and
   `stat` against the raw/semantic REST routes; it invents no `listDir`.
   ToolEndpoint now carries the exact task workdir in addition to bridge address
   and sandbox ID. OpenClaw JSON selects backend `aries-e2b`, loads
   `/opt/aries/openclaw/aries-e2b`, and contains only address, sandbox ID,
   workdir, and `/run/aries/e2b/access.token`; token bytes remain solely in the
   staged private `0600` file. Native `read`, `write`, and `edit` are enabled by
   denying only `apply_patch`; the SSH config/archive path and its broader deny
   list remain unchanged. Embedded plugin, manifest, package, shared client,
   and executable helper are copied through the existing runtime archive.
   Focused executable Node tests cover headers and token-file reads, exact bash
   request payload, streamed binary output, exit/cancellation, buffered shell
   behavior, and every pinned fs method without opening sockets. Go unit/race
   tests lock config selection, no token bytes, SSH stability, archive modes,
   exact pinned registration strings, and absence of manager/lifecycle/PTY/
   stdin/reconnect/detached/ConnectRPC surfaces. Loading the plugin inside the
   pinned OpenClaw image remains deferred to R22 step 10 because Docker is
   unavailable here and is not claimed.
9. [x] Add focused unit and race coverage first: unchanged Runner ordering and
   SSH behavior; one server across concurrent occurrences; gateway destination,
   sandbox-ID, and token isolation; wildcard-listener routing across two task
   networks; immediate fail-closed revocation; attached start framing before
   user output; actual child-PID accuracy for short-lived commands; nonterminal
   and terminal signals; PID reuse; exact argv; raw and semantic filesystem
   behavior; handler/process/evidence drain; idempotent unregister; and server
   startup/shutdown failures. The steps 1–8 regression inventory was mapped to
   every invariant above and only genuinely missing failure coverage was added:
   failed process revocation and failed centralized shutdown now remain
   unauthorized, retain cleanup state, and can be retried to positive absence.
   Focused unit/race suites, repository-wide compile-only checks, integration-
   tagged compile-only checks, build, vet/format, and `git diff --check` pass
   locally. Real gateway routing, child PID/signal delivery, Docker filesystem
   semantics, and pinned-image plugin loading remain step 10 claims because this
   machine has no Docker daemon.

10. [x] Add real-Docker and deterministic pinned-OpenClaw integration proving
   two concurrent task networks reach the same listener through their own
   gateways, cannot reach one another's gateway, route identical PID values
   independently, revoke one grant while the other remains usable, expose no
   verifier material before positive revocation, and leave no managed listener,
   process, container, network, credential, or key behind. The exact pinned
   OpenClaw v2026.7.1 image (source commit `2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4`)
   required `activation.onStartup` in the staged plugin manifest; without it the
   gateway discovered but did not activate the backend. Real Docker now proves
   container-originated two-network routing to one shared port, cross-network and
   credential isolation, exact binary process streams and ordering, real PID,
   cancellation, TERM/KILL, stale and cross-sandbox rejection, zero-byte and
   binary filesystem operations, metadata/listing/root/symlink semantics,
   selective revocation, active-process server shutdown, pinned plugin exec and
   native read/write/edit with apply_patch denied, secret absence, and positive
   cleanup. `make integration`, `make test`, `make test-race`, and `git diff
   --check` pass; post-suite inspection finds no managed resource or process.
11. [x] Update the architecture, bridge alternatives, supported-implementation,
    configuration, profile, harness, sandbox, and quick-start documentation with
    exact versioned upstream citations. The checked-in one-task DeepSeek profile
    now selects `openclaw-e2b`; non-paid setup validation passes. Immutable links
    to OpenClaw v2026.7.1 source commit `2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4`
    cover the sandbox SDK, backend handle, filesystem bridge, and full-mode plugin
    example. `make build`, `make test`, `make test-race`, `make lint`, and `make
    integration` pass. Citation reachability, `git diff --check`, mode-0600 run
    evidence, DeepSeek-key absence, temporary-token absence, and post-suite
    container, network, and process leak checks also pass.

Protected boundaries:

- The centralized server is surrounding application infrastructure, not a
  fifth Runner role; the shared `ToolBridge` interface remains task-scoped.
- ARIES alone owns sandbox lifecycle. A harness cannot create, delete, pause,
  resume, or otherwise manage a task sandbox.
- Model keys and bridge tokens do not enter JSON profiles, Docker metadata,
  logs, results, or retained nonsecret configuration.
- A wildcard socket may accept TCP on other local IPv4 destinations, but HTTP
  dispatch is denied unless the connection destination is the registered task
  gateway. If future policy forbids even that TCP handshake, use one centralized
  server with dynamically managed gateway-bound listeners instead of weakening
  request authorization or task-network isolation.

## R21 — GPU metrics data-flow cleanup

Cleanup plan, based on `8b4930c` and executed regression-first:

1. [x] Lock managed implicit and explicit GPU selection plus defensive slice
   ownership at backend preparation and occurrence construction.
2. [x] Carry the effective selection through the existing run-scoped
   `PreparedBackend` seam and give every occurrence a fresh copy.
3. [x] Delete per-occurrence native YAML resolution and the ineffective
   assignment to the by-value profile copy.
4. [x] Re-audit Runner roles, validation, concrete package boundaries, and
   cleanup ownership before the standalone cleanup commit.

Audit and protected boundaries:

- `prepareBackend` was the run-scoped owner of native SGLang validation and GPU
  topology resolution, while `newSandbox` redundantly reopened and resolved the
  same native file for every occurrence. `PreparedBackend` was the existing
  narrow seam needed to reuse the result.
- `pkg/runner` still defines exactly four substitutable roles: `Benchmark`,
  `AgentHarness`, `ToolSandbox`, and `ToolBridge`. `Sandbox` remains the live
  capability returned by `ToolSandbox`; monitoring remains Recorder-owned.
- The cleanup retains explicit command switches and all strict profile,
  native-runtime topology/index, NVIDIA argv/identity/count/metric, credential,
  ownership, isolation, revocation, cleanup, and positive-absence validation.
  No registry, factory, plugin, DI layer, generic deployment abstraction, or
  dependency was added.
- `combinedResourceSource` remains live and necessary to preserve the single
  Recorder source lifecycle while sampling and closing container and GPU
  sources. No additional production deletion was reference-proven.

Implemented cleanup and pre-commit evidence:

- `PreparedBackend.EffectiveGPUIndices` owns the resolved list. Runtime
  construction receives a separate copy, and every `NewSandbox` invocation
  receives a fresh copy. Explicit configured order remains intact for
  `CUDA_VISIBLE_DEVICES`; the NVIDIA source retains its existing sorted query
  and output normalization.
- `resolveRuntimeGPUIndices`, its direct test use, the second native YAML load
  and topology resolution, and the dead by-value profile assignment are gone.
  Reference checks find one `LoadNativeConfig` and one `ResolveGPUIndices`, both
  in backend preparation.
- Focused application/command, backend, configuration, monitor, and NVIDIA
  tests pass, including race tests. `git diff --check`, `go vet ./...`,
  `go list -deps ./...`, explicit-composition tests, the four-role inventory,
  and the fail-closed concrete-package direct-import graph pass.
- `make build`, `make test`, `make test-race`, and `make lint` pass. The
  deterministic integration suite reached its Docker-backed tests but could
  not access `/var/run/docker.sock` in this worker environment; it failed only
  with Docker permission-denied errors, so integration and Docker resource
  absence remain explicit environment gaps rather than claimed passes.
- Fail-closed host-process, tracked/staged/ignored key-path, and secret-shaped
  working-diff checks pass. No managed SGLang or `aries-ssh` helper process was
  found. The pre-commit status contains only the reviewed cleanup and this task
  record; generated cache/build artifacts remain ignored.

## R20 — Managed SGLang GPU selection

1. [x] Keep GPU selection inside the managed SGLang runtime configuration.
2. [x] Derive default local device indices from native parallel topology and
   reject inconsistent explicit selections before side effects.
3. [x] Share the resolved list between the child `CUDA_VISIBLE_DEVICES` and the
   NVIDIA resource source without changing generic monitoring.

## R19 — Explicit NVIDIA GPU monitoring

1. [x] Reuse the existing occurrence-scoped Recorder lifecycle and
   `ResourceSource` boundary rather than adding a Runner role or independent
   telemetry lifecycle.
2. [x] Add strict explicit host-device selection for the NVIDIA source; R20
   later moved ownership of that selection to managed SGLang.
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
