# Supported implementations

ARIES uses explicit profile values and command switches. This page lists the
implementations present in the repository; it is not a promise of automatic
plugin discovery.

| Category | Implementation and status | ARIES ownership | Configuration and examples |
| --- | --- | --- | --- |
| Agent harness | **OpenClaw** — current supported harness | Starts and stops the pinned harness container; retains private harness artifacts | `harness.type: "openclaw"`; pin in `configs/versions.json`; both one-task profiles below |
| Benchmark | **Terminal-Bench 2** — current supported benchmark | Verifies the pinned checkout, loads tasks, sanitizes verifier paths, and evaluates | `benchmark.type: "terminalbench2"`; checkout and revision in `configs/versions.json` |
| Tool sandbox | **Docker** — current supported sandbox | Owns task containers and networks through the Moby Go SDK | `sandbox.type: "docker"`; task images come from pinned Terminal-Bench task data |
| Tool bridge | **OpenClaw SSH** — current supported pair-specific bridge | Owns temporary SSH access, evidence, credentials, listener, sessions, and revocation | `bridge.type: "openclaw-ssh"`; requires `bin/aries-ssh` beside `bin/aries` |
| Model service | **DeepSeek** — supported external OpenAI-compatible endpoint | Validates model access; does not own the service | `runtime.backend: "deepseek"`, `runtime.mode: "external"`; `profiles/openclaw-tb2-fix-git-deepseek.json` |
| Model service | **SGLang** — supported external or ARIES-managed runtime | Validates both modes; in managed mode owns one host process for the profile run | `runtime.backend: "sglang"`; `profiles/openclaw-tb2-fix-git-sglang.json`; `configs/sglang/qwen3-8b-local.yaml` |

## Configuration boundaries

The four Runner implementations are chosen by `benchmark.type`, `harness.type`,
`sandbox.type`, and `bridge.type`. At present, the values in the table are the
only wired choices. Concrete construction is explicit rather than registered or
discovered.

Model services includes the Runner. DeepSeek must be external and uses the
configured remote base URL. SGLang accepts an external endpoint backed by its
native YAML, or a managed mode with an explicit Python executable, startup and
stop timeouts, and optional validated GPU indices. DeepSeek is not a managed
runtime, and SGLang is not a fifth Runner role.

Start with one of the checked-in profiles:

- `profiles/openclaw-tb2-fix-git-deepseek.json`
- `profiles/openclaw-tb2-fix-git-sglang.json`

The SGLang profile references
`configs/sglang/qwen3-8b-local.yaml`. Additional checked-in DeepSeek profiles
select larger Terminal-Bench subsets and execution settings without adding new
component implementations. See the [quick start](quick-start.md) for runnable
commands and [architecture](design.md) for lifecycle and isolation guarantees.
