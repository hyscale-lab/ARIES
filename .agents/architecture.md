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
  concrete providers. Bridge assignment control stays outside Runner.
  Shared harness mechanics live in `pkg/harness`; it must not import native
  harnesses. Keep native configuration, readiness, protocols, and results local.
  Shared SSH mechanics live in `pkg/bridge/ssh`; dialects own grammar and workspace
  policy. The forwarding client must not import dialects. Root bridge lifecycle
  receives native factories through wiring and must not import concrete dialects.
- Use explicit dependencies, concrete helpers, `context.Context` for external
  work, and Logrus lifecycle logging. Add dependencies only when existing code
  and the standard library are insufficient.

## Task lifecycle

Load → start sandbox → sanitize → start bridge → start/run harness → confirm
harness stop → confirm bridge revocation → evaluate (live sandbox and any fresh
evaluation sandboxes) → remove evaluation sandboxes → remove sandbox → remove
task attachment. Record every task failure in `TaskResult.Error`.

Each admitted occurrence gets fresh owners, even for repeated task IDs. Drain
admitted work through cleanup using fresh bounded contexts after cancellation.

Details: [design](../docs/design.md), [contracts](components.md).
