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
  task's business, and they all sit behind the one gateway). Both harnesses
  have an MCP client: Hermes renders the entry under its `mcp_servers`,
  OpenClaw under its native `mcp.servers` (a remote server with an SSE
  transport) together with the sandbox-gate entry `bundle-mcp`, without
  which OpenClaw loads the server and then filters its tools out of a
  session in sandbox mode. The harness's own terminal and file tools still go
  through the SSH bridge.
  How the gateway's tools appear to the model depends on the Hermes
  version: `v2026.5.29.2` registers each one as a tool named
  `mcp_<server>_<tool>`; `v2026.8.31` (the current pin) lists them as
  `mcp__<server>__<tool>` behind its `tool_describe` and `tool_call` pair,
  so the trajectory records calls to `tool_call` with the MCP tool's name
  as an argument. Verified end to end on both.
- **Evaluation** puts the grader's inputs and code back from the host, checks
  the runtime it will run on, and then runs Toolathlon's `container_eval`,
  whose verdict file decides the score. What the grader reads and executes
  is trusted as follows (the rest of this list is what "trusted" means here):
  - *inputs*: the task configuration comes from the bundle preserved on the
    host at preparation, and the task status from an argument the adapter
    passes; the trajectory file is a stub, since no pinned grader reads it;
  - *grader and ground truth*: the task's protected entries are restored
    from the archive stashed on the host, after anything the agent left
    under those names (including capitalization variants of `README.md`)
    is discarded;
  - *evaluator code*: `container_eval` and everything it imports from the
    project tree (`scripts/`, `utils/`, `configs/`, `main.py`) is extracted
    again from the host checkout, whose revision is re-verified first. The
    task directory is not part of that extraction: its grader and ground
    truth are the stash's, as preprocess left them (several graders read
    ground truth that preprocess writes, such as the IDs of the products it
    seeded), and the checkout's copies would be stale;
  - *runtime*: uv, the interpreter it manages, the project's virtual
    environment, the files uv reads for its configuration, and the top level
    of the project directory are inventoried file by file (a `SHA-256` digest
    per file, plus every symbolic link's target) before the bridge exists and
    again before the grader runs; any change refuses the evaluation, naming
    the paths, and the verdict is "failed" with that reason. The grader runs
    with a private `.pyc` cache prefix and without the user site directory,
    so a planted `.pyc` or user-site `.pth` is never loaded; `.log` files are
    not inventoried (an MCP server logs into the virtual environment while the
    agent works). The inventory
    runs on the image's system Python, not on the virtual environment it
    checks, and releases what it read from the page cache, so the sandbox
    memory ARIES reports (the usage of its control group, cache included) is not raised
    by the 1.5 GB it reads;
  - *not covered*: the image's system programs and libraries — the shell,
    `tar`, `find`, `sha256sum`, the C library. The agent has root in the
    sandbox, and a grader that runs in the same container after the agent
    cannot defend against an agent that replaces those; the other benchmarks'
    same-container verification has the same limit. Grading in a fresh container from the
    pinned image would close it and needs a sandbox capability ARIES does
    not have yet.

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
- A task also lists "local tools": tools of Toolathlon's own agent loop,
  which under ARIES is the harness. `claim_done` is served by the gateway;
  `manage_context`, `history` and `handle_overlong_tool_outputs` are the
  loop's bookkeeping, which Toolathlon's own decoupled runner ignores too
  and the harness does its own way; `python_execute` and `sleep` are what
  the harness's terminal does in the task container (Toolathlon's runner
  executes them on its host). `web_search` has no stand-in but the
  harness's own: a task that lists it loads only when the profile enables
  `harness.web_search`, and is otherwise refused with that message — 14 of
  the 53, all public-internet tasks. A local tool the adapter has no
  mapping for is refused like an unknown server.

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
defaults. `max_steps` does **not** bound the agent: it is Toolathlon's
`max_steps_under_single_turn_mode`, handed to its preprocess for the task
bundle, where it is bookkeeping for Toolathlon's own loop — which does not
run here. What bounds the Hermes agent loop is Hermes's own turn limit
(`max_turns`, rendered as 90) and the run's `agent_timeout_seconds` in the
overrides file; a run that hits either is recorded as such by the harness.

**Concurrency.** The three self-hosted applications are one deployment on
the Docker host, and every sandbox's forwarder reaches the same one. A
task's preprocess resets the state it uses (Canvas courses, mailboxes,
products), so two application-backed occurrences running at once would
corrupt each other. Task load therefore refuses any application-backed
task when `execution.concurrency` is above 1, naming the tasks; with
concurrency 1, looping included, occurrences never overlap. Tasks with no
application run at any concurrency. Isolating application state per
occurrence would need one deployment per sandbox (Toolathlon's instance
prefixes) and per-occurrence ports, which the adapter does not manage.

The gateway needs no `harness.mcp` entry: the adapter registers it
with the harness as `toolathlon` (SSE at `task-sandbox` on `gateway_port`,
with a per-call timeout above every backend timeout in Toolathlon's own
server configuration files, so Toolathlon's timeouts are the ones that fire). A
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
