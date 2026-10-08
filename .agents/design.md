# Binding design principles

Obey these principles. Identify conflicts and obtain agreement before changing
an invariant; routine aligned work needs no additional approval.

- ARIES is a research platform for trusted researchers in controlled environments.
  Prioritize experimental correctness, reproducibility, and simple implementations.
  Add multi-tenant authorization, public-service hardening, or consumer-product
  safeguards only when explicitly required. Trust operators and configuration,
  but treat agent-generated commands as untrusted. Preserve credential
  confidentiality, verifier isolation, resource ownership, and confirmed cleanup.
- ARIES runs, evaluates, and measures agent workloads. Preserve measurement
  meaning; distinguish unsupported measurements from measured zero.
  Confirmed runtime teardown must not fail observation of remaining runtimes;
  preserve genuine measurement failures.
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
  are confirmed. Evaluate independently of harness success, in the live task
  sandbox or in fresh Runner-owned sandboxes from the task environment, following
  the benchmark's original methodology; record the two outcomes separately.
- Cleanup reverses ownership, covers partial failure and cancellation, and
  confirms resource absence. Closing a transport is insufficient. Never weaken
  ownership, credential, isolation, or cleanup checks to fit an implementation.

Rationale and current gaps: [human design guide](../docs/design.md).
