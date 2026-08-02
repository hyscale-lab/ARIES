# ARIES

ARIES is a small, readable Go runner for reproducible agent benchmarks. It
executes tasks concurrently while keeping the agent harness, task sandbox, tool
access, and independent evaluation under separate ownership.

The current end-to-end path combines OpenClaw, Terminal-Bench 2, Docker, an
OpenClaw SSH bridge, and either DeepSeek or SGLang model serving.

## Key Features & Capabilities

- **Controlled concurrency.** Profiles select ordered task occurrences and a
  concurrency bound. Every admitted occurrence receives fresh components and
  completes evaluation and cleanup.
- **Fail-closed isolation.** The harness is stopped and tool access is
  positively revoked before private verifier material is introduced. The live
  sandbox is evaluated independently and then removed.
- **Modular runtimes and components.** A Runner composes four small roles:
  `Benchmark`, `AgentHarness`, `ToolSandbox`, and `ToolBridge`. Implementations
  are selected through explicit constructors and command switches.
- **Flexible model serving.** Model services sit outside the four Runner roles.
  ARIES supports external endpoints and an ARIES-managed SGLang process for the
  duration of a run.
- **Private, replayable evidence.** Structured results and component artifacts
  preserve run evidence while keeping credentials out of profiles, structured
  logs, Docker metadata, and results.

## Supported Implementations

ARIES currently supports:

| Role or service | Implementation |
| --- | --- |
| Agent harness | OpenClaw |
| Benchmark | Terminal-Bench 2 |
| Tool sandbox | Docker through the Moby Go SDK |
| Tool bridge | Harness - Tool Sandbox SSH bridge |
| Model service | External DeepSeek; external or ARIES-managed SGLang |

See [Supported implementations](docs/supported.md) for status, ownership,
configuration keys, and checked-in examples.

## Getting Started

ARIES requires Linux, a local Docker Engine, Go, Git, Make, and network access
to the configured model service and required image registries.

```sh
make build
./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json
```

Every run idempotently prepares the pinned benchmark checkout and required
container images before creating run artifacts or contacting the model
service. `aries setup PROFILE.json` remains available as an optional prewarm;
it never starts a managed runtime, loads model weights, or contacts an external
model endpoint.

The DeepSeek example requires an API key and can incur charges. Follow the
[Quick start](docs/quick-start.md) for secure credential setup, the first run,
SGLang alternatives, result checks, and troubleshooting.

## Architecture & Design

For each task, ARIES loads benchmark data, starts and sanitizes a sandbox,
grants temporary bridge access, runs and stops the harness, revokes the bridge,
evaluates the still-running sandbox, and finally removes the sandbox. Harness
execution and evaluation outcomes remain separate.

Read the [Architecture](docs/design.md) for the complete lifecycle, isolation
gates, concurrency model, and artifact boundaries. Component and extension
guides cover the [benchmark](docs/design/benchmark.md),
[agent harness](docs/design/harness.md), [tool sandbox](docs/design/sandbox.md),
[tool bridge](docs/design/bridge.md), and
[model runtime platform service](docs/design/runtime.md).

## Roadmap & Research Goals

[Aries Roadmap](https://github.com/orgs/hyscale-lab/projects/7)

ARIES is intended to support research on reproducible agent evaluation,
including task-level concurrency, strict tool isolation, independent scoring,
and comparisons across explicitly configured agent and model runtimes. Future
work may add rigorously tested implementations behind the existing component
boundaries and expand repeatable experiment workflows. These are research
directions, not claims of currently supported integrations.

## Industry Collaborators

ARIES is built and maintained in collaboration with many industry collaborators:
Amazon Web Service, Microsoft, AMD Singapore, NCSpeech

## Community, Contributing & Contact

Contributions are welcome through
[GitHub issues](https://github.com/hyscale-lab/aries/issues) and pull requests.
Before proposing a component, read the relevant architecture guide and keep the
four-role Runner lifecycle, explicit construction, isolation gates, and cleanup
guarantees intact. Questions and design proposals can use the issue tracker so
the discussion remains available to other users and contributors.

## Citation
```
@misc{kondrashov2026rethinkingaicloudinfrastructure,
      title={Rethinking AI Cloud Infrastructure for Agentic Serving Systems with the Aries Experimentation Framework}, 
      author={Leonid Kondrashov and Hongrui Liu and JooYoung Park and Boxi Zhou and Zonghao Liu and Chengzhi Lu and Riccardo Mancini and Esha Choukse and Haris Javaid and German Sviridov and Tao Peng and Chen Zhao and Anastasia Avdeeva and Aleksei Gusev and Marios Kogias and Luo Mai and Dmitrii Ustiugov},
      year={2026},
      eprint={2607.29069},
      archivePrefix={arXiv},
      primaryClass={cs.DC},
      url={https://arxiv.org/abs/2607.29069}, 
}
```

## License

The Aries codebase is licensed under MIT License. See the [MIT License](LICENSE) file for details.
