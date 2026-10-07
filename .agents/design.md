# Binding design principles

Obey these principles. Identify conflicts and obtain agreement before changing
an invariant; routine aligned work needs no additional approval.

- ARIES runs, evaluates, and measures agent workloads. Preserve measurement
  meaning; distinguish unsupported measurements from measured zero.
- Runner composes exactly four replaceable roles: Benchmark, AgentHarness,
  ToolSandbox, and ToolBridge. Preserve their behavior, ownership, and isolation
  [contracts](components.md). Model runtime infrastructure adds no role.
- Separate component behavior from execution mechanisms through explicit
  deployment dependencies. Deployment owns runtime operations; TaskEnvironment
  separately owns task attachment. Logical components need not be services.
- Bridges adapt native protocols while preserving operation semantics,
  cancellation, errors, and ownership.
- Keep selection in `cmd/aries`, construction/rollback in `internal/app/wiring`,
  and behavior in `pkg`. Avoid registries and generic orchestration frameworks.
- Withhold verifier material until harness termination and bridge revocation
  are confirmed. Evaluate the same live sandbox independently of harness success;
  record the two outcomes separately.
- Cleanup reverses ownership, covers partial failure and cancellation, and
  confirms resource absence. Closing a transport is insufficient. Never weaken
  ownership, credential, isolation, or cleanup checks to fit an implementation.

Rationale and current gaps: [human design guide](../docs/design.md).
