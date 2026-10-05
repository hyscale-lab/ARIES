# Harness implementations

These mechanisms implement the [AgentHarness contract](../design/harness.md).
Supported modes and image pins are listed in [supported components](../supported.md);
voice setup remains in the [voice guide](../voice_mode.md).

## OpenClaw Gateway and voice

OpenClaw container lifecycle, gateway protocol, and voice-session semantics are
separate concrete responsibilities. `openclaw.Manager` composes the shared
`harness.Runtime` and requests one private service endpoint through deployment. The Docker implementation publishes an
ephemeral host-loopback port. `openclaw/gateway.Client` owns one
authenticated WebSocket protocol connection and bounded fail-closed frame
dispatch. `openclaw/realtime.Runner` consumes that client for talk/chat/tool
session semantics; the gateway package intentionally contains no voice event
parsers or Docker lifecycle.

Text, realtime and voice-transcribe modes share the authenticated Gateway transport. Text sends exactly one pinned-version `agent` request and correlates accepted and terminal responses by both frame request ID and non-empty run ID; an ambiguous send, disconnect, timeout, or protocol mismatch is never retried. Realtime and voice-transcribe require read and write scopes, while text requires write scope. Only sanitized role and
sorted scope metadata may leave the authentication boundary.

Realtime and voice-transcribe both convert the task instruction to staged audio and stream it through an authenticated OpenClaw Gateway Talk session. Realtime owns one `realtime` talk session and may invoke nested agent runs through the same authenticated client. Voice-transcribe owns one `transcription` session only for streaming speech recognition. After the final transcript is accepted, ARIES closes that realtime gateway connection and opens a response-only gateway connection with agent write scope for an OpenClaw agent request using the transcript as text input. Their audio, transcript, result, and optional event records remain private harness artifacts. The separate realtime/TTS credential is staged privately for voice mode and is not part of model configuration, Gateway authentication, or structured results.

OpenClaw retains selected session archive files under `telemetry/`, redacting
credentials. Files keep their basenames; a numeric prefix disambiguates collisions
so distinct entries are retained.

## Hermes

Hermes is the second supported harness and runs a local image built from the
pinned upstream image with only the `hermes-otel` plugin added. It supports text and voice-transcribe modes; `harness.mode: "realtime"` remains OpenClaw's.

`hermes.Manager` starts one foreground native Gateway per task occurrence using
`hermes gateway run --no-supervise --external-supervisor`. The launcher exports
privately staged credentials; the API credential is separate from the model key.
The deployment supplies a private service address. Readiness requires an
authenticated `/v1/models` response.

`hermes/gateway.Client` submits the exact instruction as JSON to `POST /v1/runs`
with a unique task session ID, then polls `GET /v1/runs/{id}`. Native completion,
failure, and interruption remain distinct from transport failure. Admission is
never retried after an ambiguous response. Cancellation requests
`POST /v1/runs/{id}/stop`; only confirmed runtime removal followed by bridge
revocation permits evaluation. An HTTP disconnect or a `stopping` response is
not proof of termination.

Voice-transcribe preserves whole-recording transcription through the configured
Hermes ASR provider. Its transcript then follows the same native Gateway run
path as a text instruction. It does not stream audio through the Hermes API.

Final text comes from the native run's `output`; the trajectory remains Hermes's
SQLite session export. `session-outcome.json` records the native run/session IDs,
terminal state, and submission-to-observation timing. See the
[artifact guide](../run-results.md) for the migration mapping.

### Pinned Gateway compatibility

The inspected `v2026.8.31` tag resolves to source commit
`29112bef099274229cadff79cdff7bf7b99c4b77`. Its
[run API](https://github.com/NousResearch/hermes-agent/blob/29112bef099274229cadff79cdff7bf7b99c4b77/gateway/platforms/api_server_runs.py)
and [agent construction](https://github.com/NousResearch/hermes-agent/blob/29112bef099274229cadff79cdff7bf7b99c4b77/gateway/platforms/api_server.py)
define the request and completion behavior used here.

The Gateway reads `platform_toolsets.api_server`; the old CLI toolset key is no
longer used. `HERMES_YOLO_MODE=true` supplies the former `--yolo` approval policy
before the Gateway imports its tools. The native API does not expose the CLI's
`--ignore-rules` behavior. Native memory, user-profile, background-review, and curator defaults are
preserved. These may add task-local state or model work that historical CLI runs
with `--ignore-rules` omitted; they are not disabled to emulate the CLI. Each task has a fresh private Hermes home and workspace.
The Gateway seeds its default `SOUL.md`, whose identity text matches the old
CLI fallback in this pin. Its API constructor does not forward the general
Gateway context-file suppression setting. Context discovery reads local harness
files, not sandbox files over SSH. ARIES starts in a private workspace; if a
sandbox workdir also exists in the harness image, native discovery can consult
that local path. This remaining difference matters when comparing historical
CLI runs with unusual workdirs.
Bundled skills are synchronized by Gateway startup; the API toolset allowlist
remains explicit, including delegation only when enabled. No CLI fallback is retained.

Hermes reads its SSH target from environment variables, so ARIES sets
`TERMINAL_ENV=ssh` with the bridge's host, port, user, and identity path; this
is upstream's native SSH environment, not an ARIES modification. The working
directory is the sandbox workdir the bridge reports on its endpoint, the
directory every agent command runs in. Hermes prefixes each command with
`builtin cd -- <dir> || exit 126`, so ARIES names that directory both in
`TERMINAL_CWD` and in a `terminal:` section of the rendered configuration
(backend, directory, timeout). `HERMES_HOME`
is relocated to a staged private directory so the image's declared `/opt/data`
volume holds no run state; that one anonymous volume is still created by Docker
and is the only mount the harness tolerates. The model credential is written to
the rendered configuration as a `${NAME}` reference, staged separately as a
private key file, and exported by the launcher inside the container, so no
credential value reaches the configuration, Docker metadata, or results.

Four upstream details are load-bearing. First, `/opt/hermes/bin/hermes` sits
earliest on `PATH` and is a privilege-drop shim: invoked as root it re-execs the
real binary as the image's unprivileged `hermes` user. Everything ARIES stages
is therefore owned by that user, asserted by the container's own start command
because the Engine's copy API resets archive ownership to root. Staging as root
instead leaves the agent unable to read its own configuration, and the failure
surfaces far from its cause. Authenticated Gateway readiness confirms the service can start with the staged
configuration.

Second, Hermes requires `/bin/bash` in the task image, because every tool call
it issues is `bash -c` on the remote.

Third, Hermes accepts only provider names in its own registry, and rejects unknown provider names. The pinned version does not know
`sglang` or a plain `openai` provider, so the renderer maps both backends to
Hermes's generic `custom` provider, which routes to `model.base_url`, in the
rendered config. DeepSeek is a
built-in provider and stays as written.

Fourth, three optional profile blocks render into `config.yaml` only when set:
`model.context_length` / `max_tokens` / `temperature`, `harness.compaction`,
and `harness.hermes.extra_body`. Compaction is a general harness capability and
stays on the shared `harness` block; `extra_body` is a Hermes escape hatch, so
it lives under the type-specific `harness.hermes` block. It is a non-empty JSON
object written as the `extra_body` of one `custom_providers` entry, which
Hermes merges into every chat request for its `custom` provider. Explicit
`model.temperature`, including zero, uses this same request path on `sglang`
and `openai`; temperature is carried explicitly in the custom request body.
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

Every run enables the `hermes_otel` plugin. After images are pulled,
preparation derives `aries-local/hermes:<tag>-otel<version>-<hash>` from the
pinned image with `hermes.OTelDockerfile` through the Docker deployment's
build-if-missing operation, and the harness runs that tag. The plugin is pinned
in `configs/versions.json` under `hermes.otel_plugin` (repository, release, and
revision). The hash covers the base reference, the plugin pin, and the recipe,
so a change to any of them builds a new image; otherwise the cached one is
reused. The build installs the pinned plugin revision into
the Hermes virtual environment and copies it into Hermes's bundled plugin directory
(`/opt/hermes/plugins/hermes_otel`), because ARIES relocates `HERMES_HOME` per
task and the plugin's entry-point group is not one Hermes scans; a base older
than Hermes 0.21 is left unchanged. No OTLP backend is configured, so the
plugin makes no network calls; it keeps spans in SQLite under `HERMES_HOME`,
with content capture off. After the Gateway request finishes, before container
removal, a fixed-argv `python3` exec
dumps them to `telemetry/otel-spans.jsonl`. A missing or empty store is not a
failure; a dump that would exceed the exec output bound keeps the spans that
fit, in end order, and logs a warning; any other dump failure fails the run,
like other artifact collection failures.
Each span carries the wall-clock start and end of one tool call, model call, or
API request, which Hermes's session export does not record. The spans do not
reach bridge execs: Hermes passes no trace context over SSH, so execs are
attributed to a tool call by time only (the bridge record's `timestamp` is the
exec's end), and background work is not linked.

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

## Shared mechanics and connectivity

Each manager holds a named `runtime *harness.Runtime`. Its `RuntimeOptions`
contains the shared deployment, image, output directory, timeouts, logger, and
credential lookup. Native managers retain one options value, with shared mode,
web, subagent, and MCP inputs in `Common harness.Options` and native settings
alongside it. Wiring maps the shared fields once; native constructors retain
their own defaults and supported modes. The runtime owns the task occurrence, allocation and private
archive upload, deployment validation/start, rollback, single-instruction
admission, stop coordination, and transport closure. Native readiness remains
separate from starting the process. Ownership and failure guarantees follow the
[harness contract](../design/harness.md#lifecycle-cancellation-and-failure).

`pkg/harness` also provides the credential owner, private archive construction,
artifact bookkeeping, model and URL validation (including a nonempty hostname),
and shared speech-synthesis inputs. Internal private-file helpers share bounded log filtering and random
ID/token generation. Credential error redaction preserves cancellation without
retaining an original error that could expose a secret. A running request takes an independent credential snapshot so concurrent
cleanup cannot erase its redaction inputs. Native managers choose filenames,
permissions, archive ownership, and launch commands; Hermes's runtime user and
image volume handling remain explicit.

Configuration schemas, readiness, Gateway protocols, cancellation, voice
behavior, and trajectory interpretation remain in the concrete packages. Policy
differences remain there too: OpenClaw can fall back when an extraction credential
is missing, while Hermes requires it. Hermes also retains its reserved
`API_SERVER_*` environment-name restriction and native defaults.

Search URLs come from resolved task connectivity. Deep Research Bench declares
its search service; the task environment resolves the URL. Both native renderers
use `harness.ValidateSearch`, including when called independently of a manager.
Connectivity data remains in `pkg/core`; no renderer embeds a sandbox DNS name.
The typed placement handoff is documented in the
[deployment contract](../design/deployment.md#current-attachment-handoff).

## Deployment and evidence

Harness and sandbox currently require the same local Docker daemon; see
[Docker topology](docker.md#composition-and-topology). Shared ownership and
shutdown rules are in the [harness contract](../design/harness.md#deployment-and-substitution).

Sources: [OpenClaw manager](../../pkg/harness/openclaw/harness.go),
[Gateway client](../../pkg/harness/openclaw/gateway/client.go),
[agent protocol](../../pkg/harness/openclaw/gateway/agent.go),
[Hermes manager](../../pkg/harness/hermes/harness.go),
[Hermes Gateway client](../../pkg/harness/hermes/gateway/client.go),
[Hermes renderer](../../pkg/harness/hermes/config.go), and
[MCP validation](../../pkg/core/mcp.go).
[OpenClaw tests](../../pkg/harness/openclaw/harness_test.go) and
[Hermes tests](../../pkg/harness/hermes/harness_test.go) cover deployment injection,
partial startup rollback, secret handling, request execution, and positive stop.
These are source references, not a claim of a new runtime validation.
