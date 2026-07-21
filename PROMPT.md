You are the OMX implementation lead for ARIES, a clean Go rewrite of Agent
Bench.

# Goal

Build a small, human-readable benchmark runner in:

    ~/projects/aries

The structure and code quality should resemble:

    ~/projects/invitro

The old repository is available at:

    ~/projects/agent_bench

Use agent_bench only to understand intended behavior and workflow. Do not copy
its Python architecture, abstractions, package structure, compatibility code,
or implementation patterns. It is not the design reference.

The first working system must run:

- Terminal-Bench 2 tasks;
- an upstream OpenClaw container as the agent harness;
- one local Docker tool sandbox per task;
- OpenClaw's upstream SSH sandbox backend;
- a remote OpenAI-compatible model endpoint, initially DeepSeek;
- an evaluator after the harness finishes.

All runtime components are local for now. SSH is only the protocol between the
OpenClaw container and the task sandbox. Kubernetes and other execution
platforms will be added later as new implementations.

# Workspace rules

OMX is launched from ~/projects, which contains three repositories.

- Read invitro as the primary structural reference.
- Read agent_bench only for workflow archaeology.
- Write only inside aries.
- Run all git operations inside aries.
- Never edit, clean, reformat, commit, or generate files in agent_bench or
  invitro.
- Start from the clean aries repository; do not migrate files wholesale.

Before coding, inspect all three directories and confirm these boundaries in
aries/DESIGN.md.

# Keep the design small

Do not build a generalized platform before the first end-to-end run.

The MVP has four main component interfaces:

1. Benchmark
2. AgentHarness
3. ToolSandbox
4. ToolBridge

The Runner composes them. Evaluation belongs to the Benchmark but runs
independently of the AgentHarness.

Add an interface only when the Runner needs to substitute an implementation.
Keep implementation helpers concrete and package-private.

Avoid:

- dependency-injection frameworks;
- reflection or init-time registration;
- abstract factories;
- plugin systems;
- generic base classes;
- empty future implementations;
- map[string]any passed between components;
- configuration inheritance or deep merging;
- a large core or utils dumping ground;
- one adapter for every hypothetical future combination.

Use explicit constructors and explicit switch statements.

# Core workflow

A benchmark is a set of tasks.

Every generic Task contains:

- a stable ID;
- an instruction;
- an execution Environment.

The Environment describes what is required to run the task, such as image,
working directory, resources, environment variables, and network policy.

The benchmark implementation may retain private benchmark-specific details,
such as Terminal-Bench verifier files and metadata. Do not put
Terminal-Bench-specific fields in the generic Task.

For every task, the Runner performs exactly this sequence:

1. Ask the Benchmark for the task.
2. Start the task-specific ToolSandbox from Task.Environment.
3. Start the ToolBridge for that harness-sandbox combination.
4. Give the bridge endpoint and model configuration to the AgentHarness.
5. Start the harness container.
6. Send Task.Instruction to the harness.
7. Let the harness interact with the model and sandbox until it finishes or
   times out.
8. Stop the harness.
9. Stop the bridge so the harness can no longer access the sandbox.
10. Ask the Benchmark to evaluate the still-running sandbox.
11. Collect the result and logs.
12. Stop the sandbox.

Cleanup must run in reverse order after errors or cancellation.

# Evaluation boundary

Evaluation must be completely independent of harness execution.

- The harness never runs tests and never calculates a benchmark score.
- The benchmark never depends on OpenClaw.
- Stop the harness before evaluation begins.
- Remove harness access to the sandbox before evaluation begins.
- Keep the tool sandbox alive so the evaluator sees the state produced by the
  agent.
- Do not expose verifier tests or solution files to the harness.
- Upload or mount verifier files only after the harness has stopped.
- Run the evaluator with its own command, timeout, environment, logs, and
  result.
- If the harness fails but leaves a valid sandbox, still run the evaluator and
  record harness failure separately from evaluation outcome.

For Terminal-Bench 2, the benchmark adapter owns task discovery, task parsing,
test injection, test execution, and reward parsing.

# Minimal Go contracts

Start with simple types similar to:

    type Task struct {
        ID          string
        Instruction string
        Environment Environment
    }

    type Environment struct {
        Image       string
        BuildDir    string
        Workdir     string
        CPU         float64
        MemoryMB    int
        StorageMB   int
        GPUs        int
        AllowNetwork bool
        Env         map[string]string
    }

    type Benchmark interface {
        Tasks(context.Context) ([]Task, error)
        Evaluate(context.Context, Task, Sandbox) (Evaluation, error)
    }

    type ToolSandbox interface {
        Start(context.Context, Environment) (Sandbox, error)
    }

    type Sandbox interface {
        Exec(context.Context, Command) (CommandResult, error)
        Upload(context.Context, string, string) error
        Download(context.Context, string, string) error
        Stop(context.Context) error
    }

    type ToolBridge interface {
        Start(context.Context, Sandbox) (ToolEndpoint, error)
        Stop(context.Context) error
    }

    type AgentHarness interface {
        Start(context.Context, HarnessRequest) error
        Run(context.Context, string) (HarnessResult, error)
        Stop(context.Context) error
    }

These signatures are guidance, not a reason to create unnecessary wrapper
types. Refine them when implementation evidence requires it, while preserving
the component boundaries.

Keep result types similarly direct:

- HarnessResult: harness status, final response, duration, and log paths;
- Evaluation: score/reward, verifier status, duration, and log paths;
- TaskResult: task identity plus separate harness and evaluation results;
- RunResult: task results and aggregate summary.

# Repository layout

Follow the simple cmd plus pkg organization used by InVitro:

    aries/
    ├── cmd/
    │   └── aries/
    │       └── main.go
    ├── pkg/
    │   ├── config/
    │   ├── core/
    │   ├── runner/
    │   ├── benchmark/
    │   │   └── terminalbench/
    │   ├── harness/
    │   │   └── openclaw/
    │   ├── sandbox/
    │   │   └── docker/
    │   ├── bridge/
    │   │   └── openclawssh/
    │   └── monitor/
    ├── configs/
    ├── docs/
    ├── Makefile
    ├── go.mod
    ├── README.md
    ├── DESIGN.md
    └── TASKS.md

Do not create more top-level packages until a concrete responsibility requires
them. Keep tests beside the Go package they test.

Keep pkg/core limited to shared data such as Task, Environment, and results.
Define the four small component interfaces in pkg/runner, where they are
consumed.

Dependency direction:

    cmd -> config + runner
    runner -> core interfaces
    concrete components -> core types

Benchmark and harness implementations must not import each other. A
pair-specific bridge may depend on the narrow sandbox capability that it
adapts, but that dependency must remain inside the bridge package. The
composition root in cmd constructs everything and gives the interfaces to the
Runner.

# Configuration

Use one explicit JSON experiment file for the MVP. Do not add profiles,
inheritance, templates, or merging yet.

Example:

    {
      "name": "openclaw-tb2-fix-git-deepseek",
      "benchmark": {
        "type": "terminalbench2",
        "root": ".cache/terminal-bench-2",
        "tasks": ["fix-git"]
      },
      "harness": {
        "type": "openclaw",
        "image": "ghcr.io/openclaw/openclaw:<pinned-version>"
      },
      "sandbox": {
        "type": "docker"
      },
      "bridge": {
        "type": "openclaw-ssh"
      },
      "model": {
        "base_url": "<deepseek-openai-compatible-url>",
        "model": "<model-id>",
        "api_key_env": "DEEPSEEK_API_KEY"
      },
      "output_dir": "runs"
    }

Use concrete Go config structs and strict JSON decoding. Reject unknown fields.
Keep defaults few and visible. Never store API-key values in JSON, logs, or
results.

Use explicit switches in cmd or a small config builder:

    switch cfg.Benchmark.Type {
    case "terminalbench2":
        ...
    default:
        return an error
    }

Do not build a generic factory framework.

# Component responsibilities

## Terminal-Bench 2 benchmark

Use https://github.com/harbor-framework/terminal-bench-2 as the upstream task
format.

The first task is fix-git.

Implement only the task fields required for the initial Terminal-Bench 2
dataset. Parse task.toml, instruction.md, the environment definition, and the
verifier. Fail clearly on an unsupported execution-critical field.

Accept an existing dataset root. Also provide a setup command that clones the
pinned Terminal-Bench 2 revision into aries/.cache/terminal-bench-2 when it is
missing. Keep that cache out of git and do not create another repository beside
aries.

The adapter must:

- list selected task directories;
- return generic Task values;
- create no containers itself;
- keep tests and solution data away from the harness;
- inject tests only during Evaluate;
- execute the task's verifier in the live sandbox;
- parse its reward and logs.

Do not depend on Harbor at runtime and do not claim complete Harbor
compatibility.

## Local Docker sandbox

The sandbox package owns Docker-specific behavior:

- start one container for a task environment;
- exec commands;
- upload and download files;
- expose the information required by a bridge;
- stop and remove the container;
- preserve logs needed for debugging.

Benchmark, harness, and runner packages must not shell out to Docker directly.

## OpenClaw SSH bridge

The first bridge supports the OpenClaw plus local-Docker-sandbox combination.

It must make the task sandbox reachable through SSH from the OpenClaw
container and return the SSH endpoint and credentials required by OpenClaw.
Use ephemeral keys and host-key verification.

The agent must modify the same sandbox filesystem later inspected by the
evaluator. Add an integration test for this invariant.

Keep OpenClaw-specific SSH adaptation in pkg/bridge/openclawssh. Do not leak it
into the generic Runner or Terminal-Bench adapter.

## OpenClaw harness

Use an unmodified, pinned upstream OpenClaw image.

The OpenClaw package owns:

- rendering one task-local OpenClaw config;
- configuring the remote OpenAI-compatible model;
- configuring OpenClaw's upstream SSH sandbox backend from ToolEndpoint;
- mounting the generated config into the container;
- starting and stopping the container;
- waiting for readiness;
- sending one task instruction;
- collecting the final response and logs.

Do not patch or fork OpenClaw. Keep the config renderer inside the OpenClaw
package; it is not a generic component.

If OpenClaw's SSH workspace behavior conflicts with the task workdir, solve the
mapping inside the Docker sandbox and OpenClaw SSH bridge. Do not add
benchmark-specific path logic to the Runner.

## Monitoring

Add monitoring only after the full task path works.

For the MVP, record:

- OpenClaw container logs;
- tool-sandbox logs;
- harness result;
- verifier result;
- one-second CPU and memory samples for both containers;
- OpenClaw trajectory telemetry when available from its upstream telemetry
  interface.

Monitoring observes components; it must not control their lifecycle.

# Implementation order

Implement and commit one component at a time.

## M0 — Inspect and design

- inspect invitro structure and conventions;
- inspect only the necessary agent_bench behavior;
- inspect Terminal-Bench 2 fix-git;
- inspect OpenClaw's SSH backend and config format;
- write short DESIGN.md and TASKS.md in aries.

Do not copy source code from either reference repository.

## M1 — Go skeleton and core runner

- initialize go.mod, cmd/aries, Makefile, and README;
- add the minimal core types and four interfaces;
- add strict JSON config loading;
- implement Runner lifecycle with small fake components;
- test ordering, failure, cancellation, and cleanup.

## M2 — Terminal-Bench 2 task adapter

- parse fix-git into generic Task and Environment;
- keep verifier details private to the adapter;
- add focused parser tests.

## M3 — Local Docker sandbox

- start, exec, transfer files, and stop;
- test with a tiny local fixture image;
- prove cleanup leaves no task container behind.

## M4 — OpenClaw SSH bridge

- expose the sandbox through SSH;
- generate task-local credentials;
- verify command and file access from another container;
- prove the evaluator sees changes made through the bridge.

## M5 — OpenClaw harness

- render and inject config;
- configure the SSH endpoint and DeepSeek-compatible model;
- start the pinned upstream container;
- send one instruction and collect its result;
- test first with a deterministic fake OpenAI-compatible endpoint.

## M6 — Independent evaluator

- stop OpenClaw and the bridge;
- inject Terminal-Bench tests;
- run the verifier directly in the sandbox;
- keep harness and verifier outcomes separate.

## M7 — End-to-end and monitoring

- run fix-git through the complete workflow;
- add basic resource samples and trajectory output;
- run with DeepSeek when credentials are available;
- document exact commands and produced files;
- remove dead code and unnecessary abstractions.

# Human-readable Go rules

- Prefer plain names: Task, Environment, Runner, Sandbox, Evaluation.
- Keep interfaces small and define them where they are consumed.
- Keep constructors explicit.
- Use context.Context for external operations.
- Return wrapped errors with useful component and task context.
- Use log/slog for structured logs.
- Do not call os.Exit or log.Fatal outside main.
- Make Stop methods safe to call more than once.
- Keep command arguments as slices, not concatenated shell strings.
- Keep secrets out of task containers and evaluator environments.
- Add dependencies only when the standard library is insufficient.
- Document non-obvious choices in DESIGN.md, not in long comments.
- Delete temporary code after each milestone.
- Do not create abstractions merely because a second implementation may exist
  later.

The extension test is simple: adding a second benchmark, harness, sandbox, or
bridge should require a new concrete package and one explicit constructor
switch, not edits throughout the Runner.

# OMX working loop

For every milestone:

1. Read DESIGN.md, TASKS.md, and git status in aries.
2. Select one incomplete component.
3. Give implementation agents bounded, non-overlapping tasks.
4. Implement the component and its tests.
5. Run the smallest real integration test.
6. Ask a review agent to check readability, boundaries, cleanup, and scope.
7. Remove unnecessary abstractions found by review.
8. Update DESIGN.md, TASKS.md, and README.
9. Commit the milestone in aries.
10. Continue to the next component.

Use concise commits such as:

- chore: initialize aries go project
- feat(core): add task runner contracts
- feat(terminalbench): load fix-git task
- feat(sandbox): add local docker execution
- feat(bridge): connect openclaw over ssh
- feat(harness): run upstream openclaw
- feat(eval): verify task independently
- feat(monitor): record task resource usage

# Validation

Keep these commands working:

    make build
    make test
    make test-race
    make lint
    make integration

Unit tests must not require Docker or a paid API. Integration tests use real
local containers. Use a deterministic fake OpenAI-compatible endpoint for
repeatable OpenClaw tests, then run the optional live DeepSeek test when the
credential exists. DeepSeek credential is placed in `~/projects/aries/DEEPSEEK_API.key`

The project is complete when:

- aries contains a clean Go implementation;
- agent_bench and invitro remain untouched;
- the Runner's functional workflow depends only on the four component
  interfaces; monitoring may attach as an observer without changing that
  workflow;
- Terminal-Bench tasks map to generic Task and Environment values;
- OpenClaw runs as an unmodified upstream container;
- OpenClaw reaches the task sandbox through its SSH backend;
- the agent and evaluator observe the same sandbox state;
- the harness is stopped before evaluation;
- verifier tests are not available to the harness;
- fix-git runs end to end;
- harness failure and verifier outcome are reported independently;
- all tests and checks pass;
- no container, network, key, or process is leaked;
- README explains the architecture and how to add the next component;
- TASKS.md has no unfinished MVP item;
- the aries working tree is clean and committed.

Do not stop after scaffolding or interface creation. Continue component by
component until the complete fix-git workflow is working and reviewed.
