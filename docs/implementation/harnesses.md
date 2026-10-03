# Harness implementations

These mechanisms implement the [AgentHarness contract](../design/harness.md).
Supported modes and image pins are listed in [supported components](../supported.md);
voice setup remains in the [voice guide](../voice_mode.md).

## OpenClaw Gateway and voice

OpenClaw container lifecycle, gateway protocol, and voice-session semantics are
separate concrete responsibilities. `openclaw.Manager` owns its runtime through the deployment capability and
requests one private service endpoint. The Docker implementation publishes an
ephemeral host-loopback port. `openclaw/gateway.Client` owns one
authenticated WebSocket protocol connection and bounded fail-closed frame
dispatch. `openclaw/realtime.Runner` consumes that client for talk/chat/tool
session semantics; the gateway package intentionally contains no voice event
parsers or Docker lifecycle.

Text, realtime and voice-transcribe modes share the authenticated Gateway transport. Text sends exactly one pinned-version `agent` request and correlates accepted and terminal responses by both frame request ID and non-empty run ID; an ambiguous send, disconnect, timeout, or protocol mismatch is never retried. Realtime and voice-transcribe require read and write scopes, while text requires write scope. Only sanitized role and
sorted scope metadata may leave the authentication boundary.

Realtime and voice-transcribe both convert the task instruction to staged audio and stream it through an authenticated OpenClaw Gateway Talk session. Realtime owns one `realtime` talk session and may invoke nested agent runs through the same authenticated client. Voice-transcribe owns one `transcription` session only for streaming speech recognition. After the final transcript is accepted, ARIES closes that realtime gateway connection and opens a response-only gateway connection with agent write scope for an OpenClaw agent request using the transcript as text input. Their audio, transcript, result, and optional event records remain private harness artifacts. The separate realtime/TTS credential is staged privately for voice mode and is not part of model configuration, Gateway authentication, or structured results.

## Hermes

Hermes is the second supported harness and runs the pinned upstream image
unmodified. It supports text and voice-transcribe modes; `harness.mode: "realtime"` remains OpenClaw's.

`hermes.Manager` owns one container held at an idle command, so ARIES decides
when the agent starts rather than the image entrypoint. One task instruction is
delivered by executing a staged wrapper that runs the Hermes one-shot
(`hermes --ignore-rules --yolo --model … --provider … -z …`) and reports its
status through a delimited exit trailer. The instruction is passed as a single
argument vector element, never interpolated into a shell string. `--toolsets` is
deliberately not passed: on the pinned version its validator can return a bare
`None` that the caller unpacks, so the agent would exit before doing any work.
Toolsets come from the rendered configuration instead.

In voice-transcribe mode, unlike OpenClaw, Hermes exposes no realtime gateway session. It synthesizes the task prompt into audio, sends the whole recording to the configured Hermes ASR provider, stores the transcript, and then starts the same Hermes one-shot agent flow with the transcript as the text instruction.

Hermes exposes no control protocol, so there is no gateway client. The final
response is the one-shot's standard output, and the message-level trajectory is
Hermes's own SQLite session store exported to standard output. Container logs,
both output streams, the redacted configuration, and the session export are
private harness artifacts.

Hermes reads its SSH target from environment variables, so ARIES sets
`TERMINAL_ENV=ssh` with the bridge's host, port, user, and identity path; this
is upstream's native SSH environment, not an ARIES modification. The working
directory is the sandbox workdir the bridge reports on its endpoint, the
directory every agent command runs in. Hermes prefixes each command with
`builtin cd -- <dir> || exit 126`, so ARIES names that directory both in
`TERMINAL_CWD` and in a `terminal:` section of the rendered configuration
(backend, directory, timeout): without that section the v2026.5.29.2 CLI
replaces `TERMINAL_CWD` with its own process directory. `HERMES_HOME`
is relocated to a staged private directory so the image's declared `/opt/data`
volume holds no run state; that one anonymous volume is still created by Docker
and is the only mount the harness tolerates. The model credential is written to
the rendered configuration as a `${NAME}` reference, staged separately as a
private key file, and exported by the wrapper inside the container, so no
credential value reaches the configuration, Docker metadata, or results.

Four upstream details are load-bearing. First, `/opt/hermes/bin/hermes` sits
earliest on `PATH` and is a privilege-drop shim: invoked as root it re-execs the
real binary as the image's unprivileged `hermes` user. Everything ARIES stages
is therefore owned by that user, asserted by the container's own start command
because the Engine's copy API resets archive ownership to root. Staging as root
instead leaves the agent unable to read its own configuration, and the failure
surfaces far from its cause. The readiness probe runs `hermes --version` rather
than only stat-ing the staged files, because those checks run as root and pass
regardless of ownership.

Second, Hermes requires `/bin/bash` in the task image, because every tool call
it issues is `bash -c` on the remote.

Third, Hermes accepts only provider names in its own registry, and the one-shot
rejects an unknown name before any request. Neither pinned version knows
`sglang` or a plain `openai` provider, so the renderer maps both backends to
Hermes's generic `custom` provider, which routes to `model.base_url`, in the
rendered config and in the one-shot's `--provider` argument. DeepSeek is a
built-in provider and stays as written.

Fourth, three optional profile blocks render into `config.yaml` only when set:
`model.context_length` / `max_tokens` / `temperature`, `harness.compaction`,
and `harness.hermes.extra_body`. Compaction is a general harness capability and
stays on the shared `harness` block; `extra_body` is a Hermes escape hatch, so
it lives under the type-specific `harness.hermes` block. It is a non-empty JSON
object written as the `extra_body` of one `custom_providers` entry, which
Hermes merges into every chat request for its `custom` provider. Explicit
`model.temperature`, including zero, uses this same request path on `sglang`
and `openai`; the pinned one-shot ignores the model YAML temperature field.
Native DeepSeek temperature and duplicate temperature settings in the profile
and extra body are rejected. Hermes expands
`${NAME}` references in its configuration from the process environment, so the
harness exports `ARIES_RUN_ID` and `ARIES_TASK_ID` into the container and a
profile can tag every request with the task; a profile may reference only those
two, which keeps the credential reference out of request bodies. The profile
loader also rejects any field, at any depth, named like a credential, so a
literal key cannot reach the retained `config.yaml` or the request bodies. The `v2026.8.31` image also sets
`HERMES_WRITE_SAFE_ROOT=/opt/data`, which makes `write_file` and `patch` refuse
every sandbox path; the harness clears it, because the sandbox is the isolation
boundary and the tools act on it over SSH.

## Model Context Protocol (`MCP`)

ARIES supports configuring Model Context Protocol (`MCP`) servers for agent harnesses via `harness.mcp_servers`.

`core.MCPServerConfig` defines the server configuration (name, command/`args` for `stdio`, or `url` for `SSE`/`HTTP`). Configuration validation is enforced by `core.ValidateMCPServer`:

- Server names must not contain `whitespace` or control characters.
- Either an executable command or an absolute `HTTP`/`HTTPS` `url` must be specified, never both.
- Environment variable mappings are split into benign plain text (`env`) and host credentials (`secret_env`):
  - `env` maps target variables to plain text values that do not contain control characters.
  - `secret_env` maps target variables to host environment variable names. Raw secrets are rejected at validation time; rendered configurations persist `${NAME}` placeholders, and harness session startup stages credentials into private key files (`0600`) exported by in-container launcher scripts rather than exposing them in container environment metadata.
  - Both `env` and `secret_env` are supported for command servers and rejected for `url` servers.

Harnesses manage `MCP` execution and network boundaries as follows:

- **Command servers (`stdio`)** execute directly inside the agent container environment.
- **`URL` servers (`HTTP`/`SSE`)** connect over the network to remote endpoints rather than running inside the local container.
- **OpenClaw**: Configures `MCP` servers in the rendered `openclaw.json` under `mcp.servers`. In `sandboxed` execution, `MCP` tool access is gated by appending `"bundle-mcp"` to `tools.sandbox.tools.alsoAllow`.
- **Hermes**: Renders configured `MCP` servers into `config.yaml` under `mcp_servers`, enabling in-container agent discovery and invocation.

MCP tools do not automatically pass through the SSH ToolBridge or inherit
task-sandbox isolation.

## Deployment and evidence

Harness and sandbox currently require the same local Docker daemon; see
[Docker topology](docker.md#composition-and-topology). Shared ownership and
shutdown rules are in the [harness contract](../design/harness.md#deployment-and-substitution).

Sources: [OpenClaw manager](../../pkg/harness/openclaw/harness.go),
[Gateway client](../../pkg/harness/openclaw/gateway/client.go),
[agent protocol](../../pkg/harness/openclaw/gateway/agent.go),
[Hermes manager](../../pkg/harness/hermes/harness.go),
[Hermes renderer](../../pkg/harness/hermes/config.go), and
[MCP validation](../../pkg/core/mcp.go).
[OpenClaw tests](../../pkg/harness/openclaw/harness_test.go) and
[Hermes tests](../../pkg/harness/hermes/harness_test.go) cover deployment injection,
partial startup rollback, secret handling, request execution, and positive stop.
These are source references, not a claim of a new runtime validation.
