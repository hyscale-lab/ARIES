# Binding design principles

These principles must be obeyed. Before implementing a conflicting change,
identify the conflict and obtain agreement on a design revision. Routine aligned
work needs no additional approval.

- ARIES runs, evaluates, and measures agent workloads across model backends,
  harnesses, tool execution, and sandbox resources. Preserve measurement meaning.
- Runner composes exactly four roles: Benchmark owns tasks, preparation, and
  independent evaluation; AgentHarness owns agent/model interaction; ToolSandbox
  provides the live task environment; ToolBridge adapts harness tool protocols.
  Substitutions must preserve behavior, ownership, and isolation contracts.
- Separate component behavior from execution mechanisms. Components needing an
  environment receive explicit deployment dependencies. Deployment owns runtime
  operations; TaskEnvironment separately owns task attachment. Logical components
  need not be separate services. Require capabilities appropriate to placement.
- Bridge compatibility includes operation semantics, cancellation, errors, and
  ownership. Preserve harness-specific adaptation wherever needed.
- Keep selection switches in `cmd/aries`, configuration translation, construction,
  and rollback in `internal/app/wiring/<role>`, and concrete behavior in `pkg`.
  Keep dependencies explicit; avoid registries and generic orchestration frameworks.
  Model runtime infrastructure remains outside the four Runner roles.
- Identify each measurement boundary. Distinguish unsupported measurements from
  measured zero; document existing gaps rather than claiming full conformance.
- Keep verifier material private until harness termination and bridge revocation
  are positively confirmed. Evaluate the same live sandbox independently of
  harness success; record both outcomes separately.
- Cleanup reverses ownership order, handles partial failures and cancellation,
  and positively confirms resource absence. Never weaken ownership, credential,
  isolation, or cleanup checks to accommodate an implementation.

Human rationale and current gaps: [design principles](../docs/design.md).
