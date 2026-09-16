## Toolathlon

`profiles/hermes-toolathlon-canvas-list-test-deepseek.json` runs one
[Toolathlon](https://github.com/hkust-nlp/Toolathlon) task. Toolathlon is a
benchmark of 108 tool-use tasks in which the agent works through MCP servers
(a learning-management system, a mail server, a shop, spreadsheets, a
memory store, arXiv, and so on) and is graded by a per-task Python checker
that inspects the resulting workspace and applications.

## What the adapter reuses

Toolathlon's own runner has a "decoupled" mode that already splits a task
the way ARIES does: task preparation and an MCP gateway run inside the task
container, the agent loop runs outside it, and the grader runs inside it
again once the agent is gone. The adapter drives exactly those in-container
pieces through the sandbox and leaves the agent loop to the ARIES harness.

- **Preparation** installs the pinned project tree and the task directory
  into the task image, runs Toolathlon's `container_preprocess` (which seeds
  the workspace and, for application-backed tasks, the application), and
  starts Toolathlon's `container_tool_gateway`, an MCP-over-SSE server on a
  fixed port. Every MCP server the task declares, plus Toolathlon's own
  `claim_done`, is one tool behind that gateway.
- **The harness** reaches the gateway at `http://task-sandbox:<port>/sse`,
  the sandbox's fixed network alias. The adapter adds that endpoint to the
  harness's MCP client itself, as the server named `toolathlon`, so the
  profile names no server for it (which MCP servers a task needs is the
  task's business, and they all sit behind the one gateway). Only the
  Hermes harness has an MCP client today, so `benchmark.type: "toolathlon"`
  requires `harness.type: "hermes"`; OpenClaw follows once it has one. The
  harness's own terminal and file tools still go through the SSH bridge.
  How the gateway's tools appear to the model depends on the Hermes
  version: `v2026.5.29.2` registers each one as a tool named
  `mcp_<server>_<tool>`; `v2026.8.31` (the current pin) lists them as
  `mcp__<server>__<tool>` behind its `tool_describe` and `tool_call` pair,
  so the trajectory records calls to `tool_call` with the MCP tool's name
  as an argument. Verified end to end on both.
- **Evaluation** re-injects the trusted task bundle and the grader, then runs
  Toolathlon's `container_eval`, whose verdict file decides the score. The
  grader trusts nothing the agent could write: the task configuration comes
  from the bundle preserved on the host, and the task status from an
  argument the adapter passes to the evaluator.

The task's instruction is Toolathlon's `docs/task.md` plus the two facts
Toolathlon's agent system prompt supplies: the workspace path and that a
reply without a tool call ends the task. The harness keeps its own system
prompt.

## What differs from Toolathlon's own runner

Two upstream assumptions do not hold in an ARIES sandbox and are handled by
the adapter rather than by weakening the sandbox.

**Host networking.** Toolathlon starts its task container with
`--network host`, so its MCP servers and preprocess scripts reach the
self-hosted applications at `localhost` on fixed ports. An ARIES task
container is on a private per-task network. For tasks that use an
application-backed server (`canvas`, `emails`, `woocommerce`), the adapter
starts a small loopback forwarder inside the container that carries those
ports (`1143`, `1587`, `2525`, `10001`, `10003`, `10005`, `20001`) to the
Docker host, where Toolathlon's deployments publish them. The forwarder
targets the container's default gateway unless `benchmark.toolathlon.app_host`
names the host explicitly. Toolathlon's configuration files are used
unmodified.

**Grader hiding.** Toolathlon copies the grader and ground truth out of the
container with `docker cp` while the agent runs. The adapter archives the
same entries (Toolathlon's own protected-name list), downloads the archive
into the private run directory, removes the originals, and proves their
absence — all before the bridge exists. Evaluation discards anything the
agent left under those names, including capitalization variants of
`README.md`, and restores the archive. This is the Terminal-Bench 2 verifier
pattern applied to Toolathlon's names.

Two smaller differences are deliberate:

- Toolathlon nulls the verdict when its agent loop did not finish cleanly;
  ARIES records harness failure separately and always grades the sandbox
  state, so a task the agent abandoned scores as it stands.
- Not every task can run. Toolathlon's catalogue is 34 MCP servers: 9 run
  inside the sandbox (files, terminal, git, spreadsheets, documents,
  memory, time), 3 are the self-hosted applications, 9 reach the public
  internet without an account, and 13 need a credentialed third-party
  account (GitHub, Google ×6, Hugging Face, Notion ×2, Snowflake, W&B,
  YouTube) or, for `k8s`, a `kind` cluster on the Docker socket with host
  networking. A task whose servers include one of the last group is
  refused at task load with a message naming the server. At the pinned
  revision **53 of the 108 tasks load**: 28 need nothing outside the
  sandbox and the applications, 25 more also reach the public internet
  (the sandbox network is on for them). The other 55 need an account or
  `k8s`; running them would mean provisioning those accounts for every
  run, which is not reproducible in ARIES today.

## Running the example

Prerequisites beyond the normal ARIES requirements are:

- Toolathlon's task image, `docker.io/lockon0927/toolathlon-task-image:1016beta`,
  which carries the Python environment, Node, and the MCP servers;
- for application-backed tasks, the matching Toolathlon deployment running
  on the Docker host (`deployment/canvas`, `deployment/poste`,
  `deployment/woocommerce` in the pinned checkout), publishing the fixed
  ports above;
- outbound network access from the task container, which the adapter turns
  on unconditionally because several servers reach the public internet.

Build and prewarm the checked-in profile from the repository root:

```sh
make build
./bin/aries setup profiles/hermes-toolathlon-canvas-list-test-deepseek.json
```

`setup` installs or verifies the pinned checkout, materializes the two
site configuration files Toolathlon derives from its checked-in examples,
and pulls the task and harness images. Run the profile with:

```sh
./bin/aries profiles/hermes-toolathlon-canvas-list-test-deepseek.json
```

The example uses DeepSeek and therefore needs the credential described in the
[quick start](../quick-start.md#external-deepseek). To select other tasks,
replace `benchmark.tasks` with directory names from `tasks/finalpool` in the
pinned checkout; a task the adapter cannot serve is rejected before any
sandbox starts.

## Configuration

```json
"benchmark": {
  "type": "toolathlon",
  "root": ".cache/toolathlon",
  "tasks": ["canvas-list-test"],
  "environment": {"image": "docker.io/lockon0927/toolathlon-task-image:1016beta"},
  "toolathlon": {"gateway_port": 10086, "app_host": "", "max_steps": 200}
},
"harness": {"type": "hermes"}
```

`benchmark.environment.workdir` is fixed to Toolathlon's agent workspace and
`allow_network` to true; a profile that sets them otherwise is rejected. The
`benchmark.toolathlon` block is optional and its values above are the
defaults. The gateway needs no `harness.mcp` entry: the adapter registers it
with the harness as `toolathlon` (SSE at `task-sandbox` on `gateway_port`,
with a per-call timeout above every backend timeout in Toolathlon's own
server configs, so Toolathlon's timeouts are the ones that fire). A
`harness.mcp.servers` block, if present, adds servers of your own and may
not reuse the name `toolathlon`.

## Artifacts

Under `<output_dir>/<task>/toolathlon/`: `task_bundle.json` (the trusted
task bundle preprocess produced), `artifact-stash.tar` (the hidden grader),
`preprocess.log`, `gateway.log` (every tool call the gateway served),
`eval.log`, and `eval_res.json` (Toolathlon's verdict, with its own
`details` or `failure` text). The trajectory stub Toolathlon's evaluator
reads is kept as `traj_log.json`; the agent's real trajectory is the
harness's artifact.
