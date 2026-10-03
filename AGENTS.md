# ARIES agent router

`.agents/` contains concise, binding guidance for coding agents. `docs/` contains
human-facing explanations and guides. Obey [design.md](.agents/design.md) and the
contracts relevant to your task. Read only relevant agent pages; consult human
docs when more detail is needed.

| Task | Agent reference |
| --- | --- |
| Design principles and substitution constraints | [.agents/design.md](.agents/design.md) |
| Code ownership, dependencies, task lifecycle | [.agents/architecture.md](.agents/architecture.md) |
| Four component contracts | [.agents/components.md](.agents/components.md) |
| Profiles, deployment, models, credentials | [.agents/profiles.md](.agents/profiles.md) |
| Tests, completion, run evidence and cleanup | [.agents/validation.md](.agents/validation.md) |

When behavior, architecture, configuration, or workflows change, update affected
`.agents/` references and corresponding `docs/` pages together. Keep both accurate
against code and tests. When supported components or profile fields change,
update `docs/supported.md` and `docs/configuration.md`, and check quick-start
examples. Report code gaps without silently weakening principles.

Complete authorized work and relevant verification without permission handoffs
for routine steps. Ask when scope is consequentially ambiguous or destructive.
Keep edits and Git operations inside this repository; preserve unrelated work.
Use installed skills when relevant and bounded native subagents when
useful; the lead owns integration and final verification.
