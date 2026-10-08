# Architecture

| Ownership | Location |
| --- | --- |
| CLI and implementation switches | `cmd/aries` |
| Profiles, preparation, scheduling, results | `internal/app` |
| Configuration translation, constructors, rollback | `internal/app/wiring/<role>` |
| Four role interfaces and task lifecycle | `pkg/runner` |
| Shared data | `pkg/core` |
| Model settings and shared Chat Completions transport | `pkg/model` |
| Concrete components | `pkg/{benchmark,harness,bridge,sandbox}` |
| Deployment contract and providers | `pkg/deployment` |

- `cmd` injects `app.Wiring`; `internal/app` must not import its wiring packages.
- Harness, sandbox, and bridge controllers consume deployment contracts, not
  concrete providers. Wiring supplies bridge launch settings and runtime metadata;
  controllers must not select providers or inject backend-specific defaults.
  Bridge runtime placement and borrowed sandbox execution are separate dependencies.
  Backend-specific execution and runtime removal belong in deployment providers;
  the shared deployment package holds contracts.
  Deployment resolves addresses for the consuming process. Protocol code consumes
  supplied endpoints; it must not discover interfaces, construct Docker DNS names,
  or assume that host loopback is reachable from a harness. Forward placement
  handles opaquely. See [endpoint handoff](../docs/design/deployment.md#endpoint-handoff).
  Shared bridge control stays outside Runner; Runner receives per-sandbox sessions.
  Shared harness mechanics live in `pkg/harness`; it must not import native
  harnesses. Keep native configuration, readiness, protocols, and results local.
  Shared SSH mechanics live in `pkg/bridge/ssh`; dialects own grammar and workspace
  policy. Wiring owns endpoint/client-staging policy; shared lifecycle applies it.
  Shared SSH serving and root bridge lifecycle must not import concrete dialects
  or harness clients. Root lifecycle receives native factories through wiring.
  OpenClaw's forwarding client lives in `pkg/bridge/ssh/openclaw/client` and must
  not import server grammar.
- Use explicit dependencies, concrete helpers, `context.Context` for external
  work, and Logrus lifecycle logging. Add dependencies only when existing code
  and the standard library are insufficient.

## Task lifecycle

Load → start sandbox → sanitize → start bridge → start/run harness → confirm
harness stop → confirm bridge revocation → evaluate (live sandbox and any fresh
evaluation sandboxes) → remove evaluation sandboxes → remove sandbox → release
logical task attachment. Record every task failure in `TaskResult.Error`.

The application owns one shared bridge service and network per run. Start them
after model preflight, before task admission. After all task cleanup, stop the
service, finalize shared observation, remove shared connectivity and close
transports before persisting the run result. Record shared outcomes separately
from task counts and measurements.

Each admitted occurrence gets fresh owners, even for repeated task IDs. Release
owned resources using fresh bounded cleanup contexts after cancellation. Bridge
revocation closes access and leaves sandbox processes intact for evaluation.
Runner owns explicit bridge revocation; do not add assignment leases or
controller-liveness supervision. Abrupt Runner crashes may need operator cleanup.

Details: [design](../docs/design.md), [contracts](components.md).
