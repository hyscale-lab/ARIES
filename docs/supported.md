# Supported implementations

**Harnesses and tool sandboxes currently require the same local Docker daemon.**
Remote Docker servers and mixed deployment backends are not supported. See the
[deployment configuration](configuration.md#deployment-configuration) for socket
settings and supported topology.

This page summarizes capabilities and limitations. Use the
[quick start](quick-start.md) for a first run, the
[configuration reference](configuration.md) for profile fields, and
[run results](run-results.md) for artifacts and troubleshooting.

## Support matrix

| Category | Supported implementation | Guide |
| --- | --- | --- |
| Agent harness | **OpenClaw** — text, realtime, and voice-transcribe modes; web tools and configurable subagent spawning | [Harness configuration](configuration.md), [realtime mode](configuration.md#realtime-openclaw-mode), [voice guide](voice_mode.md) |
| Agent harness | **Hermes** — text and voice-transcribe modes; web tools, context compaction, and custom request bodies for compatible backends | [Hermes configuration](configuration.md#hermes-context-window-compaction-and-request-extra-body), [voice guide](voice_mode.md) |
| Benchmark | **Terminal-Bench 2** — verifier-based terminal tasks | [Quick start](quick-start.md) |
| Benchmark | **Deep Research Bench** — open-ended research reports with RACE grading and optional FACT citation checking; grading can be disabled | [Benchmark guide](benchmarks/deep-research-bench.md) |
| Benchmark | **SWE-Atlas QA** — codebase Q&A with host-side rubric grading; grading can be disabled; only the QA track is implemented | [Benchmark guide](benchmarks/swe-atlas-qa.md) |
| Benchmark | **SWE-bench Pro** — public issue-resolution split with pinned task scripts and parser | [Benchmark guide](benchmarks/swe-bench-pro.md) |
| Tool sandbox and deployment | **Docker** — local containers managed through the Moby Go SDK | [Deployment configuration](configuration.md#deployment-configuration), [Docker implementation](implementation/docker.md) |
| Tool bridge | **OpenClaw SSH** and **Hermes SSH** — embedded, harness-specific adapters | [SSH bridge implementation](implementation/ssh-bridges.md) |
| Model service | **DeepSeek** — external endpoint | [Model backends](configuration.md#model-backends) |
| Model service | **SGLang** — external endpoint or one ARIES-managed host process per run | [Model backends](configuration.md#model-backends) |
| Model service | **OpenAI-compatible server** — external only, including vLLM, llama.cpp, gateways, and hosted endpoints | [Model backends](configuration.md#model-backends) |

Image and dataset revisions are pinned in [versions.json](../configs/versions.json).
Runnable combinations are provided in [profiles/](../profiles/); benchmark setup,
judge settings, and credential requirements are documented in the
[configuration reference](configuration.md#benchmark-settings) and benchmark
guides above.

## Current limitations

- Each SSH bridge supports its corresponding harness. Crossed pairs are rejected
  before execution. Hermes requires `/bin/bash` in the task image; its bridge
  rejects Hermes's private `~/.hermes` file synchronization. OpenClaw requires
  `bin/aries-ssh` beside `bin/aries`.
- Realtime mode is OpenClaw-only and needs a separate TTS credential. See the
  [realtime setup](configuration.md#realtime-openclaw-mode).
- External model servers are operated separately from ARIES. ARIES does not
  install SGLang or download model weights for managed runs. Endpoint checks
  verify access to the configured model; they do not establish support for every
  server-specific extension.
- SWE-bench Pro has additional image-architecture, isolation, and licensing
  requirements; consult its [benchmark guide](benchmarks/swe-bench-pro.md).
- Measurement availability and deployment portability have known gaps described
  in the [design principles](design.md#measurement-meaning-and-current-gaps). These limits
  matter when comparing runs across implementations.

## Roadmap

Kubernetes deployment and a shared gRPC sandbox protocol with E2B compatibility
are planned targets, **not currently supported capabilities**. E2B compatibility
is a target, not a specified or verified API/version contract, and does not imply
support in every harness. Kubernetes placement is recognized by configuration
but rejected before runtime effects. Docker is the current deployment provider;
bridges currently run embedded and use pair-specific SSH protocols.

See the [deployment contract](design/deployment.md),
[Docker implementation](implementation/docker.md), and
[SSH bridge implementation](implementation/ssh-bridges.md) for current boundaries.
