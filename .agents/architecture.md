# Architecture

| Ownership | Location |
| --- | --- |
| CLI and implementation selection | `cmd/aries` |
| Profiles, preparation, scheduling, results | `internal/app` |
| Construction, configuration translation, rollback | `internal/app/wiring/<role>` |
| Four role interfaces and task lifecycle | `pkg/runner` |
| Shared data | `pkg/core` |
| Model settings and Chat Completions transport | `pkg/model` |
| Concrete components | `pkg/{benchmark,harness,bridge,sandbox}` |
| Deployment contracts and providers | `pkg/deployment` |

- `cmd` injects `app.Wiring`; `internal/app` must not import wiring packages.
- Controllers depend on deployment contracts, not providers. Wiring supplies
  provider choices/defaults, launch settings, and runtime metadata. Keep bridge
  placement and borrowed sandbox execution as separate dependencies. The shared
  deployment package holds contracts; providers own backend-specific operations.
- Deployment owns runtime operations, attachment, publication, and endpoint
  resolution for the consuming process. Forward placement handles opaquely.
  Protocol code must not derive addresses or assume host loopback is reachable.
  See [endpoint handoff](../docs/design/deployment.md#endpoint-handoff).
- Shared harness, bridge, and SSH lifecycle must not import native harnesses,
  dialects, or clients. Wiring injects factories and endpoint/client-staging policy.
  Dialects own grammar/workspace rules; harnesses own native config, readiness,
  protocols, and results. Harness-side SSH clients (the OpenClaw client,
  `pkg/bridge/ssh/client`, `pkg/harness/codex/executor`) must not import server
  grammar. Wiring injects Codex's sandbox staging, removed only after confirmed release.
- Use explicit dependencies, concrete helpers, `context.Context` for external
  work, and Logrus lifecycle logs. Reuse native APIs and standard/existing
  libraries; add dependencies only for missing capabilities, rather than writing
  generic protocols. Compatibility paths need supported versions and actual
  callers. Use Moby for Docker.

## Task lifecycle

Load → start/sanitize sandbox → register bridge → start/run harness → confirm
harness stop → confirm bridge revocation → evaluate → remove evaluation sandboxes
→ remove task sandbox → release attachment. Record every task failure in
`TaskResult.Error`.

The app owns one shared bridge and network per run; Runner receives per-sandbox
sessions. Start shared infrastructure after model preflight, before task admission.
After task cleanup, stop and remove the shared bridge runtime, finalize shared
observation, remove connectivity, and close transports before persisting results.
Record shared outcomes separately from task counts and measurements.

Load the selected catalog once per run; keep shared metadata immutable and
verifier inputs benchmark-private. Give every occurrence fresh mutable benchmark
state and runtime owners, including repeated task IDs. Runner explicitly revokes
sessions; use fresh bounded cleanup contexts after cancellation. No assignment
leases or controller-liveness supervision.

Details: [design](../docs/design.md), [contracts](components.md).
