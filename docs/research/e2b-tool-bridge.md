# E2B and `envd` as a model for an ARIES tool bridge

## 1. Purpose, method, and how to read this

This document answers one question: can E2B's in-sandbox daemon `envd`, or its `RPC` schema, replace
the SSH wire protocol in `pkg/bridge/hermesssh` while preserving every guarantee recorded in
[the Hermes bridge functional inventory](../design/hermes-bridge-inventory.md).

Two evidence classes are used and are never mixed:

- **Verified.** Read directly from primary sources — E2B's own source code, its `proto` files, its
  architecture document, and the source of third-party projects. Each such claim carries a `URL`.
- **Inference.** A conclusion drawn from verified facts but not stated by any source. Every such
  paragraph or row begins with the word *Inference*.

Where a question could not be settled from primary sources, that is stated instead of guessed.

All external `URL`s in this document were fetched and returned `HTTP 200` at the time of writing.
Source line references are to the `main` branch of the named repository, which moves; the
constants and method names quoted are the anchor, not the line number.

## 2. What E2B is, architecturally

E2B is an open-source platform for running untrusted, model-generated code. Its infrastructure lives
in [`e2b-dev/infra`](https://github.com/e2b-dev/infra) and its client libraries in
[`e2b-dev/E2B`](https://github.com/e2b-dev/E2B). Both are Apache-2.0
([`infra` LICENSE](https://github.com/e2b-dev/infra/blob/main/LICENSE),
[E2B `LICENSE`](https://github.com/e2b-dev/E2B/blob/main/LICENSE)).

The authoritative internal description is
[`docs/ARCHITECTURE.md`](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md), which
states two design ideas: a sandbox is a *resumed snapshot* (templates are pre-booted `VM` snapshots;
memory pages arrive lazily by `userfaultfd`, the root filesystem is a copy-on-write overlay served
over `NBD`), and *control plane and data plane are separate* — "Sandbox traffic never passes through
the API."

### 2.1 Components, and where each runs

Verified from the services table and system diagram in
[`docs/ARCHITECTURE.md`](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md):

| Component | Package | Runs on | Role |
| --- | --- | --- | --- |
| API | `packages/api` | host, API nodes | Public `REST` control plane: sandbox lifecycle, placement, auth, quotas |
| Orchestrator | `packages/orchestrator` | host, every sandbox node | Runs `Firecracker` virtual machines; create, pause, resume, kill; `gRPC` :5008, sandbox proxy :5007 |
| Template manager | `packages/orchestrator` (role) | host, build nodes | Builds templates from Docker images |
| Client proxy | `packages/client-proxy` | host, API nodes | Edge router: `<port>-<sandboxID>.<domain>` to the owning node |
| Dashboard API | `packages/dashboard-api` | host | Web dashboard backend, not used by the `SDK` |
| **`envd`** | `packages/envd` | **inside every guest `VM`** | The only E2B component in the guest: process and filesystem `API` for `SDK`s, port 49983 |

State lives in `PostgreSQL` (teams, templates, snapshots), `Redis` (running-sandbox routing catalog),
`ClickHouse` (metrics and events) and object storage. Deployment is `Terraform` plus `Nomad` on `GCP`
or `AWS`.

Two distinct paths reach `envd`, both from outside the guest:

- **Data path.** `SDK` → load balancer → `client-proxy` (:3002) → the owning node's orchestrator
  proxy (:5007) → the sandbox's network-slot IP → `envd` :49983.
- **Control path.** The orchestrator reaches `envd`'s control routes directly over the host network
  at the slot IP. Those routes (`/init`, `/upgrade`, the freeze and thaw hooks) are marked
  `x-internal: true` in `spec/envd.yaml`, and the sandbox proxy answers them `404` for every method
  so they are unreachable through a public sandbox `URL`.

Guest metadata — the sandbox ID and the hash of `envd`'s access token — is delivered through
`Firecracker`'s metadata service (`MMDS`).

Nothing but `envd` and the user's own processes runs inside the guest. Everything else is host-side.

## 3. `envd` in depth

### 3.1 Process model

Verified from [`packages/envd/main.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/main.go):

`envd` is a single Go binary (module `github.com/e2b-dev/infra/packages/envd`). Its `README` describes
it as a "Daemon that runs inside a sandbox that allows interacting with the sandbox via calls from the
SDK" ([`packages/envd/README.md`](https://github.com/e2b-dev/infra/blob/main/packages/envd/README.md)).

It is one process serving one `net/http` server bound to `0.0.0.0:49983` (`defaultPort = 49983`),
with `ReadTimeout` and `WriteTimeout` set to zero and an `idleTimeout` of 640 seconds. A `chi` router
multiplexes two protocol faces onto that single port. It is not an `init` system and does not
supervise anything: in E2B's own template it is *supervised by* `systemd`, declared as a simple
always-restart unit running `/bin/bash -l -c /usr/bin/envd`
([`debug.Dockerfile`](https://github.com/e2b-dev/infra/blob/main/packages/envd/debug.Dockerfile)).

Relevant command-line flags, all in `parseFlags`:

| Flag | Default | Effect |
| --- | --- | --- |
| `-isnotfc` | `false` | "run outside of `Firecracker` (skips `MMDS` poll and HTTP log exporter)" |
| `-port` | `49983` | listen port |
| `-cgroup-root` | `/sys/fs/cgroup` | `cgroup` v2 root |
| `-no-cgroups` | `false` | `"disable cgroup management; use a no-op cgroup manager instead"` |
| `-verbose` | `false` | write logs to `stdout` |
| `-resume-handover` | `false` | internal, live self-upgrade |

The `cgroup` manager already falls back to a `no-op` manager whenever `cgroup` v2 setup fails
(`createCgroupManager`), so `cgroup` control is best-effort rather than a hard requirement.

### 3.2 Protocol and serialization

Verified. `envd` speaks **Connect `RPC`** (`connectrpc.com/connect`), not raw `gRPC`. The generated
handlers are mounted onto the same `chi` multiplexer as the `REST` routes — `spec.NewProcessHandler(service,
interceptors)` returns a path and an `http.Handler` which `Handle` mounts
([`internal/services/process/service.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/service.go)).

The generated procedure paths are, verbatim, from
[`process.connect.go`](https://github.com/e2b-dev/infra/blob/main/packages/shared/pkg/grpc/envd/process/processconnect/process.connect.go):

```text
/process.Process/List        /process.Process/Connect     /process.Process/Start
/process.Process/Update      /process.Process/StreamInput /process.Process/SendInput
/process.Process/SendSignal  /process.Process/CloseStdin
```

The message schema is `protobuf`, but the wire encoding the `SDK`s actually use is **Connect's `JSON`
codec**: the JavaScript `SDK` constructs its transport with `createConnectTransport({ baseUrl:
this.envdApiUrl, useBinaryFormat: false })`
([`packages/js-sdk/src/sandbox/index.ts`](https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/index.ts)).
The Python `SDK` uses the `connectrpc` package over an HTTP/2 client
([`packages/python-sdk/e2b/envd/rpc.py`](https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/envd/rpc.py)).
Streaming rides the Connect streaming framing — a five-byte prefix of flags plus length, then a
`JSON` payload — over ordinary HTTP; the protocol is specified at
[`connectrpc.com/docs/protocol`](https://connectrpc.com/docs/protocol/). A Go server or client is
available as [`connectrpc/connect-go`](https://github.com/connectrpc/connect-go).

This matters more than it looks: because the codec is `JSON` and the framing is documented, the
schema can be spoken without any `protobuf` toolchain. Section 5 records a project that does exactly
that.

### 3.3 The `Process` service

Verified verbatim from
[`spec/process/process.proto`](https://github.com/e2b-dev/infra/blob/main/packages/envd/spec/process/process.proto):

```proto
service Process {
    rpc List(ListRequest) returns (ListResponse);
    rpc Connect(ConnectRequest) returns (stream ConnectResponse);
    rpc Start(StartRequest) returns (stream StartResponse);
    rpc Update(UpdateRequest) returns (UpdateResponse);
    rpc StreamInput(stream StreamInputRequest) returns (StreamInputResponse);
    rpc SendInput(SendInputRequest) returns (SendInputResponse);
    rpc SendSignal(SendSignalRequest) returns (SendSignalResponse);
    rpc CloseStdin(CloseStdinRequest) returns (CloseStdinResponse);
}
```

The command itself is a four-field message, and this is the whole of it:

```proto
message ProcessConfig {
    string cmd = 1;
    repeated string args = 2;
    map<string, string> envs = 3;
    optional string cwd = 4;
}
```

`StartRequest` adds an optional `PTY` (with `cols`/`rows`), an optional `tag`, and an optional
`stdin` boolean. Processes are addressed after start by `ProcessSelector`, a `oneof` of `pid` or
`tag`.

Every stream — from `Start` or from a later `Connect` — carries `ProcessEvent`:

```proto
message ProcessEvent {
    oneof event { StartEvent start = 1; DataEvent data = 2; EndEvent end = 3; KeepAlive keepalive = 4; }
    message StartEvent { uint32 pid = 1; }
    message DataEvent { oneof output { bytes stdout = 1; bytes stderr = 2; bytes pty = 3; } }
    message EndEvent { sint32 exit_code = 1; bool exited = 2; string status = 3; optional string error = 4; }
    message KeepAlive {}
}
```

Note the properties this schema does and does not have:

- `stdout` and `stderr` are separate `oneof` arms, so the streams stay distinguished. Good.
- `exit_code` is `sint32` — signed and unbounded. There is no `0..255` domain in the schema.
- `EndEvent` carries a free-text `status` and an `optional string error`, so a daemon-side error
  message can reach the client. ARIES's inverse rule — fixed error strings, no sandbox error text in
  artifacts — is a recording rule, not a wire rule, so this is compatible but not enforced.

`stdin` is not part of the `Start` stream. It travels on separate calls: `SendInput` (unary) or
`StreamInput` (client-streaming, with the comment "Client input stream ensures ordering of
messages"). `CloseStdin` signals `EOF`, and the `proto` states "Only works for non-`PTY` processes. For
`PTY`, send `Ctrl+D` (0x04) instead."

Signals are `SIGNAL_SIGTERM` (15) and `SIGNAL_SIGKILL` (9) only. There is no arbitrary-signal escape
hatch and no process-group concept in the schema.

### 3.4 The `Filesystem` service, and file content

Verified from
[`spec/filesystem/filesystem.proto`](https://github.com/e2b-dev/infra/blob/main/packages/envd/spec/filesystem/filesystem.proto):

```proto
service Filesystem {
  rpc Stat(StatRequest) returns (StatResponse);
  rpc MakeDir(MakeDirRequest) returns (MakeDirResponse);
  rpc Move(MoveRequest) returns (MoveResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc Remove(RemoveRequest) returns (RemoveResponse);
  rpc WatchDir(WatchDirRequest) returns (stream WatchDirResponse);
  rpc CreateWatcher(CreateWatcherRequest) returns (CreateWatcherResponse);
  rpc GetWatcherEvents(GetWatcherEventsRequest) returns (GetWatcherEventsResponse);
  rpc RemoveWatcher(RemoveWatcherRequest) returns (RemoveWatcherResponse);
}
```

**File *content* is not in the `RPC` schema at all.** Reading and writing bytes happens over the `REST`
face: `GET /files` and `POST /files` (`multipart`), plus `POST /files/compose`, defined in
[`spec/envd.yaml`](https://github.com/e2b-dev/infra/blob/main/packages/envd/spec/envd.yaml). The full
`REST` surface is `/health`, `/metrics`, `/envs`, `/files`, `/files/compose`, `/init`, `/freeze`,
`/unfreeze`, `/collapse`, `/fsfreeze`, `/fsthaw`, and the unlisted `/upgrade`.

This is directly relevant to ARIES: the bridge's file-sync denial rule
(`pkg/bridge/hermesssh/grammar.go:47-76`) has to cover a second surface that is not `RPC` at all if
`envd` is adopted whole, and there is no configuration flag in `envd` that disables `/files`.

### 3.5 Execution semantics

Verified from
[`internal/services/process/handler/handler.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/handler/handler.go):

Every command is wrapped in a shell. `envd` builds

```go
oomWrapperScript := fmt.Sprintf(`echo %d > /proc/$$/oom_score_adj && exec %s"${@}"`, ...)
wrapperArgs := append([]string{"-c", oomWrapperScript, "--", req.GetProcess().GetCmd()}, req.GetProcess().GetArgs()...)
cmd := exec.CommandContext(ctx, "/bin/sh", wrapperArgs...)
```

`argv` boundaries survive, because the client's `cmd` and `args` are passed as positional parameters
and re-expanded with `"${@}"` — there is no string concatenation of the user command. But `/bin/sh`
is unconditionally on the path, and the wrapper writes to `/proc/$$/oom_score_adj` and optionally
uses `ionice` and `nice`.

The process runs under `syscall.SysProcAttr{Credential: {Uid, Gid, Groups}}`, so `envd` must itself
run as `root` (or hold `CAP_SETUID`/`CAP_SETGID`) and the target user must exist in the guest's
`/etc/passwd`.

Working directory (`permissions.ExpandAndResolve` in
[`internal/permissions/path.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/permissions/path.go),
`execcontext.ResolveDefaultWorkdir` in
[`internal/execcontext/context.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/execcontext/context.go)):

```go
func ResolveDefaultWorkdir(workdir string, defaultWorkdir *string) string {
	if workdir != "" { return workdir }
	if defaultWorkdir != nil { return *defaultWorkdir }
	return ""
}
```

**The client's `cwd` wins.** The daemon default applies only when the client sends none. `~` is
expanded against the user's home, a relative path is joined onto the home, and the resolved path
must exist — but there is no character-class or shape validation comparable to ARIES's `validWorkdir`
(`pkg/bridge/hermesssh/workspace.go:66-85`).

Environment: `envd` sets `PATH` from its own environment, `HOME`, `USER` and `LOGNAME` from the
resolved user, then the daemon defaults, then **every** client-supplied entry from
`ProcessConfig.envs`, last value winning. There is no filter. ARIES's rule that no client
environment variable reaches the sandbox has no counterpart here.

### 3.6 Streaming, cancellation, and lifetime

This is the most consequential difference from the ARIES bridge, and it is verified from
[`internal/services/process/start.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/start.go):

```go
// Create a new context with a timeout if provided.
// We do not want the command to be killed if the request context is cancelled
procCtx, cancelProc := context.Background(), func() {}
if requestTimeout > 0 { // zero timeout means no timeout
    procCtx, cancelProc = context.WithTimeout(procCtx, requestTimeout)
}
```

`envd` deliberately **decouples process lifetime from the `RPC` stream**. The process context is
rooted at `context.Background()`. When the stream is cancelled or the client disconnects, the handler
returns but a detached `goroutine` continues to `proc.Wait()` and reap the child. The only bounded
lifetime comes from the `Connect-Timeout-Ms` request header (`determineTimeoutFromHeader`), which
`envd` repurposes as a process deadline; killing otherwise requires an explicit `SendSignal`.

Reinforcing this, terminal events are retained for `terminatedRetentionTTL = 30 * time.Second` after
exit so a *reconnecting* client can still recover the exit code
([`service.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/service.go)).
The whole design assumes processes outlive their connections — the exact opposite of the ARIES
guarantee that revocation aborts in-flight work (`bridge.go:997-1011`, `bridge.go:812`).

`KeepAlive` events are emitted on a ticker on both the process and filesystem-watch streams to hold
long-idle streams open through proxies.

### 3.7 Authentication and authorization

Two independent, unrelated mechanisms, both verified.

**Username selection, not a secret.**
[`internal/permissions/authenticate.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/permissions/authenticate.go)
registers `connectrpc.com/authn` middleware whose `AuthenticateUsername` reads HTTP Basic auth,
**discards the password**, and resolves the username to an operating-system user:

```go
username, _, ok := req.BasicAuth()
if !ok { return nil, nil }   // no username: fall through to the default user
u, err := GetUser(username)
```

This selects *which operating-system user the command runs as*. It authenticates nothing.

**The access token.**
[`internal/api/auth.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/api/auth.go)
wraps the whole handler in `WithAuthorization`, which compares an `X-Access-Token` header against a
token held in memory. Three properties matter:

1. **If the token is not set, everything is open.** The `switch` has a case for "token is set" and a
   case for the live-upgrade pre-`init` window, and no default — an `envd` that never received a
   token serves every route unauthenticated.
2. `GET /files`, `POST /files`, `POST /init` and `GET /health` are in `authExcludedPaths`, permanently
   exempt from the header check. `/files` is instead gated by query-parameter signing:
   `v1_ + SHA256("path:operation:username:token[:expiry]")`, compared in constant time
   (`generateSignature`, `validateSigning`).
3. The token is delivered by `POST /init`, and `/init` authenticates itself against a hash published
   in `Firecracker` `MMDS`
   ([`internal/api/init.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/api/init.go)).

The `/init` validation logic is worth quoting because it decides whether a non-`Firecracker`
deployment can be secured at all:

```go
matchesMMDS, mmdsExists := a.checkMMDSHash(ctx, requestToken)
switch {
case matchesMMDS:                                   return nil
case !a.accessToken.IsSet() && !mmdsExists:         return nil // first-time setup
case !requestTokenSet:                              return ErrAccessTokenResetNotAuthorized
default:                                            return ErrAccessTokenMismatch
}
```

and `checkMMDSHash` opens with `if a.isNotFC { return false, false }`.

*Inference*, drawn from those two fragments together: outside `Firecracker`, the first
`POST /init` to arrive may set the access token, and every later attempt to change it is rejected.
So a Docker-hosted `envd` **can** be given a per-session bearer token — but the security of that
token is first-caller-wins. Any process that reaches port 49983 before the intended controller does
takes ownership of the daemon. In ARIES's ordering the bridge starts before the harness, but the task
image's own entry point runs earlier still, so the race is real and would have to be closed by
network placement, not by `envd`.

### 3.8 Guest footprint

Verified from `main.go` and `internal/host/mmds.go`. Beyond the listening socket, `envd` inside a
sandbox:

- creates `/run/e2b` and writes `/run/e2b/.E2B_SANDBOX` (and, under `Firecracker`,
  `.E2B_SANDBOX_ID` and `.E2B_TEMPLATE_ID`);
- creates and manages `cgroup` v2 subtrees named `ptys`, `socats`, and `user` under the `cgroup` root,
  with `cpu.weight`, `io.weight`, `memory.high` and `memory.max` values it computes from host
  metrics;
- runs a port scanner every second and spawns a `socat` process per discovered listening port to
  republish it on `eth0` (`publicport.NewForwarder`);
- polls `MMDS` and exports logs over HTTP unless `-isnotfc` is set;
- installs certificate-authority certificates (`host.NewCACertInstaller`) and can rewrite `/etc/hosts` on `/init`.

### 3.9 License and a ready-made Go client

`envd` is Apache-2.0, as is the whole of `e2b-dev/infra`. A **generated Go Connect client already
exists in the repository** at
[`packages/shared/pkg/grpc/envd/process/processconnect/process.connect.go`](https://github.com/e2b-dev/infra/blob/main/packages/shared/pkg/grpc/envd/process/processconnect/process.connect.go),
with a sibling for the filesystem service, exposing `NewProcessClient(httpClient, baseURL, opts...)`
and `NewProcessHandler(svc, opts...)`. Both a client and a server can be built from it without
writing any generator configuration.

The friction is the module: `packages/shared` declares `go 1.26.6`
([`go.mod`](https://github.com/e2b-dev/infra/blob/main/packages/shared/go.mod)) against ARIES's
`go 1.26.5`, and it is a large module carrying storage clients, telemetry, and feature-flag code that
ARIES has no use for. Copying only the two `.proto` files and generating into ARIES's own tree
avoids that entirely; the `proto` files are Apache-2.0 like the rest, so a `NOTICE`-style attribution
is the whole obligation.

## 4. Piecemeal adoption

### 4.1 Does `envd` assume `Firecracker`, a specific `rootfs`, or E2B's control plane?

Verified answers, one by one.

| Assumption | Present? | Evidence |
| --- | --- | --- |
| `Firecracker` | **No, explicitly optional.** `-isnotfc` "run outside of `Firecracker` (skips `MMDS` poll and HTTP log exporter)"; `checkMMDSHash` short-circuits on `isNotFC` | `main.go`, `internal/api/init.go` |
| An `init` system | **No.** `envd` is an ordinary `main()` that serves HTTP; `systemd` supervises it in E2B's template but nothing in the code requires it | `main.go`, `debug.Dockerfile` |
| `cgroup` v2 | **No.** `-no-cgroups` selects a `no-op` manager, and `createCgroupManager` already falls back to one on any failure | `main.go` |
| A specific `rootfs` | **Partly.** `/bin/sh` must exist; the target user must exist in `/etc/passwd`; `/proc` must be mounted; `socat` is needed only for port forwarding | `handler/handler.go`, `internal/port` |
| Running as `root` | **Yes, effectively.** `syscall.Credential` `setuid`/`setgid` per process | `handler/handler.go` |
| E2B's control plane | **No for `RPC`; yes for auth.** The `Process` and `Filesystem` services need no orchestrator. The access token arrives only via `/init`, whose non-`Firecracker` path is first-caller-wins | `internal/api/auth.go`, `init.go` |

### 4.2 Could `envd` run in an ordinary Docker container?

**Yes — verified, and E2B ships the recipe.** The `envd` `README` documents `make start-docker` as
"(re)build the `envd` daemon and start a Docker container with `envd` running inside", and says the
`SDK`s can be pointed at it with `E2B_DEBUG=true`
([`README.md`](https://github.com/e2b-dev/infra/blob/main/packages/envd/README.md)). The `Makefile`
target builds `debug.Dockerfile` and runs `docker run --name envd -p 49983:49983 ...`
([`Makefile`](https://github.com/e2b-dev/infra/blob/main/packages/envd/Makefile)). The image installs
`systemd`, creates a `user` account, and enables an `envd.service` unit
([`debug.Dockerfile`](https://github.com/e2b-dev/infra/blob/main/packages/envd/debug.Dockerfile)).

Two qualifications. First, that image is E2B's own, built for debugging — it is not a demonstration
that `envd` runs inside an *arbitrary benchmark* image. Second, `envd` is developed against
`Firecracker`; the Docker path is a development convenience, and nothing in the repository commits to
keeping it working.

### 4.3 The consequence ARIES cares about: a daemon in the evaluated container

The current bridge executes through the Docker Engine `API` from the host, using
`Sandbox.ExecStream`. The task container therefore needs no ARIES component inside it, and
`.agents/BRIDGE-ALTERNATIVES.md:274` records the alternative as rejected for exactly that reason:

> **Install or require `sshd` in every task image** — Moves daemon, user, key, port, and process
> ownership into the evaluator's sandbox. […] The evaluator sees daemon and credential setup
> mutations unrelated to the task. **Rejected.**

An `envd`-style daemon inside the task container **has the same problem, and a larger footprint than
`sshd`**. Against each clause of that rejection:

| Rejection clause | `sshd` | `envd` in the task container |
| --- | --- | --- |
| Daemon, user, key, port ownership moves into the sandbox | yes | yes — plus `/run/e2b`, `cgroup` subtrees, a `socat` per listening port, certificate-authority installation, an `/etc/hosts` rewrite on `/init` |
| Every heterogeneous benchmark image must run and safely configure it | yes | yes — must provide `/bin/sh`, a matching `/etc/passwd` entry, and let `envd` run as `root` |
| Another service to stop and audit | yes | yes, and harder: `envd` intentionally lets processes outlive their streams (§3.6), so "the daemon is gone" no longer implies "the work is gone" |
| Evaluator sees setup mutations unrelated to the task | yes | yes, and more of them |
| Transport logs depend on image configuration | yes | yes — `envd` logs through `zerolog` to `stdout` or an HTTP exporter, not to an artifact ARIES controls |

There is one clause `envd` avoids: it needs no privileged credential *distribution* into the image
because the token is pushed over the wire at `/init` rather than baked into the image. That is a
narrow win and does not change the verdict.

Additionally, the file-transfer denial that `pkg/bridge/hermesssh/grammar.go` implements would have
to cover `POST /files`, `GET /files`, `Filesystem.MakeDir`, `Filesystem.Move` and
`Filesystem.Remove` — a much wider surface than five `argv` prefixes, and one the daemon offers by
construction with no way to switch off.

*Conclusion (inference, but a direct one): running `envd` inside the task container should be
rejected on the same grounds and by the same argument already recorded for `sshd`.*

### 4.4 Would running the daemon in the harness container instead be coherent?

**No.** The daemon's entire function is to execute processes in the filesystem and process namespace
it lives in. An `envd` inside the Hermes harness container would execute commands *in the harness
container*, which is the one place the agent's work must not land: the verifier inspects the task
container, and `pkg/runner` hands the bridge the exact `Sandbox` the evaluator later receives.

The coherent placement is neither container. It is **ARIES's own process on the host**, speaking the
`envd` protocol face outward to the harness and `Sandbox.ExecStream` inward to the task container —
which is structurally identical to what `pkg/bridge/hermesssh` already does with SSH, and is exactly
the shape section 5 shows a third party has already built.

### 4.5 Adopting only the protocol schema

**Feasible, and demonstrated by others.** Three properties make it unusually cheap:

- The schema is two small `.proto` files with no E2B-specific types in them. `ProcessConfig` is four
  fields; there is no sandbox ID, no template reference, no orchestrator handle anywhere in
  `Process` or `Filesystem`.
- The wire codec the `SDK`s use is `JSON`, not binary `protobuf` (§3.2), so a server can be written
  against the Connect framing without a `protobuf` toolchain — or with one, since
  [`connectrpc/connect-go`](https://github.com/connectrpc/connect-go) generates idiomatic Go
  handlers.
- Connect's unary and server-streaming calls work over HTTP/1.1; only bidirectional streaming needs
  HTTP/2 ([`connectrpc.com/docs/protocol`](https://connectrpc.com/docs/protocol/)). `Process.Start` is
  server-streaming, and `stdin` arrives on separate calls, so a full command implementation needs no
  bidirectional stream.

A schema-only adoption keeps the ARIES architecture unchanged: the bridge remains a host-side server
that terminates the harness's calls and re-issues them as `core.Command` values through
`ExecStream`. Nothing enters the evaluated container.

### 4.6 Licensing per option

| Option | License obligation |
| --- | --- |
| Vendor and run the `envd` binary | Apache-2.0. Redistribution of the binary requires the license text, a `NOTICE`, and change notices. Also implies tracking upstream, whose `pkg/version.go` "must be bumped on every behavior change" |
| Import `packages/shared/.../processconnect` as a Go module | Apache-2.0, and a `go 1.26.6` toolchain floor plus a large unrelated dependency tree |
| Copy the two `.proto` files and generate into ARIES | Apache-2.0 attribution on the copied files. Smallest obligation; ARIES already carries `google.golang.org/protobuf` as an indirect dependency |
| Reimplement an ARIES-native schema informed by `envd` | None. Interface shapes are not copyrightable in any jurisdiction relevant here, and nothing needs to be copied |

## 5. Existing projects that use E2B

Verified from each project's own source or documentation.

| Project | What it uses E2B for | `SDK` or `envd` directly | Notes |
| --- | --- | --- | --- |
| [`harbor-framework/harbor`](https://github.com/harbor-framework/harbor) | An `e2b` environment backend for agent evaluation, alongside Docker, `Daytona` and `Modal` | Official Python `SDK` (`from e2b import AsyncSandbox`) | **The closest analogue to ARIES.** `Harbor` is the harness that runs Terminal-Bench ([documentation](https://www.harborframework.com/docs/tutorials/running-terminal-bench)); its `exec` goes through `sandbox.commands.run(cmd=..., background=True, cwd=..., envs=..., timeout=..., user=...)` then `handle.wait()`. Apache-2.0. [`src/harbor/environments/e2b.py`](https://github.com/harbor-framework/harbor/blob/main/src/harbor/environments/e2b.py) |
| [`huggingface/smolagents`](https://github.com/huggingface/smolagents) | `E2BExecutor`, one of several remote Python executors (`Docker`, `Modal`, `Blaxel`) | `e2b_code_interpreter` `SDK` | Ships an `e2b.toml` template definition. [`remote_executors.py`](https://github.com/huggingface/smolagents/blob/main/src/smolagents/remote_executors.py), [`e2b.toml`](https://github.com/huggingface/smolagents/blob/main/e2b.toml) |
| [`BitMiracle-AI/Dormice`](https://github.com/BitMiracle-AI/Dormice) | **Implements the `envd` protocol face itself** so the official `e2b` `SDK` works against a self-hosted Docker + `gVisor` daemon | Neither — it *is* the server | See below. Apache-2.0 |
| [`e2bgateway/e2bgateway`](https://github.com/e2bgateway/e2bgateway) | A Go gateway presenting "a fully compatible interface aligned with the official E2B client protocol", routing to [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox) and [`alibaba/OpenSandbox`](https://github.com/alibaba/OpenSandbox) | Neither — it *is* the server | Small and young, but it is a second independent instance of the same idea. Apache-2.0 |
| `LangChain`, `LlamaIndex`, `CrewAI`, `Vercel` AI `SDK`, and others | Code-interpreter tools | Official `SDK`s | E2B's own integration index: [`docs.e2b.dev/quickstart/connect-llms`](https://docs.e2b.dev/quickstart/connect-llms) |

Not found: any evidence that Inspect (`UK AISI`) ships an E2B sandbox provider; searching its
documentation and repository surfaced Docker, `Kubernetes`, `Modal`, `Proxmox` and `Vagrant` providers
only. Stated as *not determined* rather than absent — a third-party provider may exist outside the
main repository.

### 5.1 `Dormice`: the precedent that matters most

[`Dormice`](https://github.com/BitMiracle-AI/Dormice) is a self-hosted sandbox daemon that
implements `envd`'s Connect `RPC` surface itself, so that the unmodified official `e2b` `SDK` works against
Docker containers. Its own header comment states the architecture in one sentence
([`packages/server/src/e2b/envd/index.ts`](https://github.com/BitMiracle-AI/Dormice/blob/main/packages/server/src/e2b/envd/index.ts)):

```text
The envd surface: what the official SDK reaches through its `sandboxUrl` option. [...] Connect RPC
rides the JSON codec (the SDK sets `useBinaryFormat: false`); files ride plain HTTP. The daemon
itself plays the role of envd -- there is no agent inside the container.
```

Its process face
([`envd/process.ts`](https://github.com/BitMiracle-AI/Dormice/blob/main/packages/server/src/e2b/envd/process.ts))
hand-writes the Connect envelope (`FLAG_MESSAGE`, `FLAG_END_STREAM`, `envelope(...)`) and emits
`{ event: { data: { stdout: <base64> } } }` and `{ event: { end: { exitCode, exited, status } } }`
frames directly, without any `protobuf` runtime.

This is a working existence proof of exactly the architecture ARIES would need: the `envd` protocol,
spoken by a host-side server, executing into Docker containers that contain no daemon. It does not
provide ARIES's isolation guarantees — it is not built for adversarial evaluation — but it settles
the feasibility question.

### 5.2 Has anyone swapped a shell or SSH transport for a structured `RPC` one?

Yes, and inside Hermes itself. `Hermes`'s pluggable terminal backends include `ssh`, `docker`,
`daytona`, `modal`, `vercel_sandbox` and `singularity`
([`agent/terminal_env_registry.py`](https://github.com/NousResearch/hermes-agent/blob/main/agent/terminal_env_registry.py)).
The `Daytona` backend replaces the SSH transport with a structured `SDK` call while keeping the same
payload:

```python
response = sandbox.process.exec(shell_cmd, timeout=timeout)
```

([`tools/environments/daytona.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/daytona.py),
versus `cmd.extend(["bash", "-c", shlex.quote(cmd_string)])` in
[`tools/environments/ssh.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/ssh.py)).

The lesson is important and slightly deflating: the transport becomes structured, but the *payload
stays a shell script*. `Hermes`'s `BaseEnvironment` is built around a single abstract `_run_bash`
hook and a `_wrap_command(command, cwd)` that composes a `bash` script carrying working-directory
markers and session-state snapshots
([`tools/environments/base.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/base.py)).
Any `Hermes` backend ARIES writes will receive a `bash` script string, whatever the wire looks like.
There is no `e2b` backend among `Hermes`'s built-ins.

## 6. `envd` against the ARIES preservation checklist

Rows are drawn from section 13 of
[the Hermes bridge functional inventory](../design/hermes-bridge-inventory.md). "Native" means the
capability is provided by `envd` as it exists, not by something ARIES would write around it.

| ARIES guarantee | Native in `envd`? | Gap / what ARIES must add |
| --- | --- | --- |
| **Positive revocation** — a `nil` `Stop` proves access is gone | **No** | `envd` has no revocation concept. Its access token can be rotated only through `/init`, which is not a revocation primitive, and dropping a stream does not stop work. ARIES must own revocation entirely: close its own listener, cancel in-flight executions, and confirm |
| **In-flight work aborted, not awaited** | **No — actively contradicted** | "We do not want the command to be killed if the request context is cancelled" (`start.go`). Killing needs an explicit `SendSignal`, and even then only `SIGTERM`/`SIGKILL` to a `pid`, with no process-group semantics. ARIES's `Docker` sandbox already targets process groups and confirms absence; that must stay ARIES-side |
| **Ambiguous post-revocation errors fail closed** | **No** | Connect surfaces `CodeCanceled`/`CodeDeadlineExceeded`, so a *classification* is available, but the `isPureCancellation` / `hasCancellationCause` distinction (`bridge.go:953-995`) is ARIES policy with no analogue. Port unchanged |
| **Audit completeness gates revocation** — any unrecordable call blocks `Stop` | **No** | `envd` logs through `zerolog` to `stdout` or an HTTP exporter, with a unary interceptor that deliberately omits stream events (`LogServerStreamWithoutEvents`). There is no durable per-call ledger, no sequencing, no seal, and no failure path that blocks anything. The entire `auditWriter` design (`bridge.go:167-182`, `bridge.go:415-423`) is ARIES's to keep |
| **Workdir authority** — the bridge, not the client, sets `Dir` | **No — inverted** | `ResolveDefaultWorkdir` gives the client's `cwd` priority and uses the daemon default only when the client sends none. A schema-only adoption must either ignore the `cwd` field or reject any request that sets it, and must keep `validWorkdir` (`workspace.go:66-85`), for which `envd` has no equivalent |
| **No client environment variables reach the sandbox** | **No — inverted** | `ProcessConfig.envs` is merged in last and wins. ARIES must reject a non-empty `envs` map, or drop it and record the refusal |
| **Exactly two command shapes reach the sandbox** | **No** | `cmd` and `args` are free-form, and `envd` additionally interposes its own `/bin/sh -c` wrapper. The grammar-equivalent check (accept only `/bin/bash [-l] -c <script>` and the bootstrap probe) stays ARIES-side, now over a typed `argv` instead of a quoted string |
| **Denial of file transfer into the evaluated container** | **No — the opposite is a feature** | `GET`/`POST /files`, `/files/compose`, `Filesystem.MakeDir`, `Move`, `Remove` and `WatchDir` are all first-class, with no flag to disable them. A schema-only adoption should simply not implement the `Filesystem` service or the `/files` routes, and must record each attempt as `denied` rather than as a protocol error |
| **Bounded `stdin` retention, no partial retention** | **No** | `SendInput`/`StreamInput` have no size bound. The 16 MiB `maxRecordedInputBytes` rule and the discard-not-truncate behavior (`bridge.go:212-229`) are ARIES's. Structurally easier here: `stdin` arrives as discrete framed messages rather than a byte stream |
| **Evidence ordering and sequencing** — monotonic sequence, shared `timestamp`, `FIFO` file order | **No** | `envd` assigns an incrementing `operation_id` per unary call (`logs.AssignOperationID`) but nothing durable or ordered across streams. Port `auditWriter` unchanged. Note that `StreamInput` exists precisely because Connect "ensures ordering of messages" on a client stream, which helps correlate `stdin` with its command |
| **No command output in the audit** | **Not applicable — must be preserved by construction** | `envd` never writes output to a file, so nothing conflicts; but nothing enforces it either. The `byteCounter` approach (`bridge.go:184-202`) ports directly, since `DataEvent` frames are already discrete and countable |
| **Credential handling** — one ephemeral credential, removed at revocation, never in the endpoint value | **Partly** | A bearer token in `X-Access-Token` is a smaller and simpler credential than an `Ed25519` key pair, and `core.ToolEndpoint` already carries only paths. But `envd`'s token is set once via `/init` and is first-caller-wins outside `Firecracker` (§3.7). Under an ARIES-written server the token is ARIES's to generate, write `O_EXCL` at `0600`, and delete on `Stop` — the existing machinery applies unchanged |
| **Exit-code clamp to `0..255`** | **No** | `EndEvent.exit_code` is `sint32`. ARIES must define its own domain explicitly, as the inventory already anticipates |
| **`NUL` and empty-payload rejection** | **No** | `proto3` `string` fields must be valid `UTF-8`, which rules out embedded `NUL` in `cmd`/`args`/`cwd` at the codec layer — a genuine improvement over ad-hoc parsing — but an empty `cmd` is still schema-valid and must be rejected in application code |
| **Wire payload reproducible for hashing** | **Improved** | A typed `argv` list has exactly one canonical serialization, so the `shlex` round-trip machinery (`grammar.go:106-160`) can be retired rather than ported. Hash the canonical `JSON` (or the `protobuf`) encoding of the request message |
| **Concurrent calls on one connection** | **Yes** | Connect over HTTP/2 multiplexes natively; `Process.Start` streams are independent. The twelve-parallel-command test (`bridge_test.go:380`) ports directly |
| **Streamed `stdin`, `stdout`, `stderr`, no buffering** | **Yes** | `DataEvent` keeps `stdout` and `stderr` in separate `oneof` arms; `stdin` is a separate call. This is a clean fit for `ExecStream` |
| **Refused sub-call must not tear down the transport** | **Yes** | A Connect error response ends one `RPC`, not the connection. The awkward "refuse the `env` request but keep the channel open" rule (`bridge.go:752-763`) disappears |
| **Sandbox usable for evaluation after revocation** | **Yes, if ARIES owns the server** | Trivially true when nothing was installed in the container. **False if `envd` runs inside it** — see §4.3 |

Score, counted honestly: of the nineteen rows, `envd` natively provides four and improves the shape of
two more. Every guarantee the inventory marks in bold as **Yes, must be provided** is either absent
from `envd` or inverted by it.

## 7. Assessment and recommendation

### Option (a) — adopt `envd` wholesale

Run the upstream `envd` binary inside the task container and have `Hermes` talk to it.

*For:* zero protocol work; a maintained daemon; `PTY` and filesystem support for free; an existing
`SDK` on the client side.

*Against:* it inverts the bridge's central property. The task container is the artifact the verifier
inspects, and this option installs a `root` daemon in it that creates `/run/e2b`, manages `cgroup`
subtrees, spawns `socat` processes, installs certificate-authority certificates, and rewrites `/etc/hosts` — the precise
list of mutations `.agents/BRIDGE-ALTERNATIVES.md:274` rejected `sshd` for, and longer. It removes
ARIES from the execution path, so `ExecStream`, process-group cancellation, the audit ledger, the
file-sync denial and workdir authority all have to be rebuilt as policy *around* a daemon that
contradicts several of them by design. And a benchmark image would have to be modified to carry the
daemon, breaking the "unmodified upstream image" property that
`integration_test.go:158` exists to prove.

**Reject.**

### Option (b) — adopt the protocol schema, write an ARIES server

Copy `process.proto` (and not `filesystem.proto`), generate Go with `connect-go`, and have
`pkg/bridge/hermesrpc` serve `/process.Process/Start`, `StreamInput`, `SendInput`, `SendSignal` and
`CloseStdin` on the host, translating each accepted call into a `core.Command` and an `ExecStream`.

*For:* the architecture is unchanged — a host-side listener that terminates harness calls and
re-issues them into the exact evaluated sandbox, which is what `pkg/bridge/hermesssh` already is.
Every guarantee in section 6 stays where it is today. The schema is small, stable, expressible in `JSON`,
and already implemented by two independent third parties (`Dormice`, `E2BGateway`), so it is not a
private invention. The quoting grammar, the `shlex` parity code and the "refused request must keep
the channel open" workaround all go away. And it is a genuinely externally-legible choice: a future
harness that already speaks E2B needs no adapter.

*Against:* the schema is shaped by requirements ARIES does not have and lacks ones it does. `cwd` and
`envs` are client-authoritative and must be rejected rather than honored, which means shipping a
schema whose fields are deliberately refused — a documentation burden and a standing source of
confusion. `Process.List`, `Process.Connect`, tags, `PTY`, and process reattachment are all
impossible for ARIES to permit and would either be absent (breaking a real E2B client) or
present-and-refusing. Two new direct dependencies (`connectrpc.com/connect`, and `protobuf` promoted
from indirect). And the compatibility benefit is largely theoretical unless something on the other
end actually is an E2B client — which, today, `Hermes` is not.

### Option (c) — an ARIES-native `RPC` schema informed by `envd`

Design a minimal schema that borrows `envd`'s proven shapes — a `oneof` event stream with
`start`/`data`/`end`/`keepalive`, separate `stdout` and `stderr` arms, `stdin` on its own ordered
call, an explicit `end` carrying an exit code — but carries only fields ARIES will honour: no `cwd`,
no `envs`, no `pty`, no filesystem service, no process reattachment.

*For:* every field means something and nothing is present-but-refused. The wire is exactly the four
Hermes payload shapes plus `stdin`, so the audit record is a faithful projection of the request
rather than a filtered one. It can still ride Connect over HTTP so all the transport reasoning above
holds, and it can be moved to the `envd` schema later if a second harness ever justifies it.

*Against:* no external client speaks it, so it is a private protocol — which is precisely the
"Generic remote-tool relay" concern already recorded as `Rejected until multiple real adapters reveal
a smaller shared seam` (`.agents/BRIDGE-ALTERNATIVES.md:276`). The counter-argument is that this is
not a generic relay but a second pair-specific bridge, which is the pattern ARIES already follows.

### Recommendation

**Option (c), with the `envd` schema as the explicit design reference and the Connect protocol as the
transport.**

The reasoning is that the value in `envd` is almost entirely in its *event schema*, not in its
service surface or its daemon. The `ProcessEvent` `oneof` — `start` with a `pid`, `data` with
separate `stdout`/`stderr` arms, `end` with an exit code and a status, `keepalive` for idle streams —
is a good design validated by four years of production use, and copying its shape costs nothing.
Its service surface, by contrast, is dominated by capabilities ARIES must refuse: client-authoritative
`cwd` and `envs`, a full filesystem `API`, file upload and download, process reattachment by `tag`,
and processes that deliberately outlive their streams. Adopting that surface in order to reject most
of it buys compatibility with clients ARIES does not have.

Option (b) becomes the right answer the moment a harness ARIES wants to support ships a working E2B
client — at which point the migration from (c) to (b) is a schema swap behind an unchanged server,
because both ride the same transport.

Whichever is chosen, the section 6 table is the real specification: `envd` supplies almost none of the
guarantees, and the bridge's `Start`/`Stop` lifecycle, revocation ledger, audit writer, grammar
validation, workdir authority and file-transfer denial all survive the migration essentially intact.
What the migration actually deletes is the SSH channel machinery and the `shlex` quoting round-trip —
roughly `grammar.go` in full and the channel-handling third of `bridge.go`.

### The single biggest obstacle

Not the protocol. **The harness side.**

`Hermes` reaches its terminal backend through `TerminalEnvironmentProvider`, registered in
`agent/terminal_env_registry.py`. That registry exists on `main`
([source](https://github.com/NousResearch/hermes-agent/blob/main/agent/terminal_env_registry.py))
and its built-in backend set is `{local, docker, singularity, modal, managed_modal, daytona,
vercel_sandbox, ssh}` — with no `e2b` entry, so a `Hermes` client would have to be written for any of
the three options.

But the pinned image `docker.io/nousresearch/hermes-agent:v2026.5.29.2`
(`configs/versions.json`) **does not contain that registry at all** — confirmed absent at that tag and
at `v2026.6.5` and `v2026.7.1`. Everything the current bridge relies on follows from that: ARIES
speaks SSH because SSH is the only remote backend the pinned image has, and
`TestUpstreamHermesDrivesTheBridgeWithoutPatches` (`integration_test.go:158`) exists to prove the
unmodified upstream image drives the bridge with no patch layer.

Any `RPC` migration therefore forces one of three choices, none of them cheap:

1. **Bump the pinned `Hermes` image** to one carrying the plugin registry, and write a provider
   plugin. This changes the evaluated agent, so every existing result becomes incomparable — and the
   registry's `BUILTIN_BACKEND_NAMES` frozen set means the plugin must register under a *new* name,
   not shadow `ssh`.
2. **Patch the pinned image**, which discards the "unmodified upstream, configured only by
   environment variables" property that the integration test was written to defend.
3. **Keep the SSH bridge for `Hermes`** and introduce the `RPC` bridge for a different harness first.

Note also §5.2: even with the registry available, `Hermes`'s `BaseEnvironment` hands every backend a
`bash` script string through `_run_bash`. The wire becomes typed; the payload does not. The migration
buys structural framing, ordered `stdin`, a clean cancellation channel and the removal of quoting
ambiguity — it does not buy a structured *command model*, because `Hermes` does not have one.

## 8. Sources

Primary — E2B:

- [`e2b-dev/infra`](https://github.com/e2b-dev/infra) and its [`LICENSE`](https://github.com/e2b-dev/infra/blob/main/LICENSE) (Apache-2.0)
- [`docs/ARCHITECTURE.md`](https://github.com/e2b-dev/infra/blob/main/docs/ARCHITECTURE.md)
- [`packages/envd/main.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/main.go), [`README.md`](https://github.com/e2b-dev/infra/blob/main/packages/envd/README.md), [`Makefile`](https://github.com/e2b-dev/infra/blob/main/packages/envd/Makefile), [`debug.Dockerfile`](https://github.com/e2b-dev/infra/blob/main/packages/envd/debug.Dockerfile)
- [`spec/process/process.proto`](https://github.com/e2b-dev/infra/blob/main/packages/envd/spec/process/process.proto), [`spec/filesystem/filesystem.proto`](https://github.com/e2b-dev/infra/blob/main/packages/envd/spec/filesystem/filesystem.proto), [`spec/envd.yaml`](https://github.com/e2b-dev/infra/blob/main/packages/envd/spec/envd.yaml)
- [`internal/api/auth.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/api/auth.go), [`internal/api/init.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/api/init.go), [`internal/permissions/authenticate.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/permissions/authenticate.go), [`internal/permissions/path.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/permissions/path.go), [`internal/execcontext/context.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/execcontext/context.go)
- [`internal/services/process/start.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/start.go), [`service.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/service.go), [`handler/handler.go`](https://github.com/e2b-dev/infra/blob/main/packages/envd/internal/services/process/handler/handler.go)
- [`packages/shared/pkg/grpc/envd/process/processconnect/process.connect.go`](https://github.com/e2b-dev/infra/blob/main/packages/shared/pkg/grpc/envd/process/processconnect/process.connect.go), [`packages/shared/go.mod`](https://github.com/e2b-dev/infra/blob/main/packages/shared/go.mod)
- [`e2b-dev/E2B`](https://github.com/e2b-dev/E2B), [`LICENSE`](https://github.com/e2b-dev/E2B/blob/main/LICENSE), [`js-sdk/src/sandbox/index.ts`](https://github.com/e2b-dev/E2B/blob/main/packages/js-sdk/src/sandbox/index.ts), [`python-sdk/e2b/envd/rpc.py`](https://github.com/e2b-dev/E2B/blob/main/packages/python-sdk/e2b/envd/rpc.py)
- [E2B integration index](https://docs.e2b.dev/quickstart/connect-llms)

Primary — protocol and third parties:

- [Connect protocol specification](https://connectrpc.com/docs/protocol/), [`connectrpc/connect-go`](https://github.com/connectrpc/connect-go), [Connect Go getting started](https://connectrpc.com/docs/go/getting-started/)
- [`harbor-framework/harbor`](https://github.com/harbor-framework/harbor), [`src/harbor/environments/e2b.py`](https://github.com/harbor-framework/harbor/blob/main/src/harbor/environments/e2b.py), [running Terminal-Bench on `Harbor`](https://www.harborframework.com/docs/tutorials/running-terminal-bench)
- [`BitMiracle-AI/Dormice`](https://github.com/BitMiracle-AI/Dormice), [`e2b/envd/index.ts`](https://github.com/BitMiracle-AI/Dormice/blob/main/packages/server/src/e2b/envd/index.ts), [`e2b/envd/process.ts`](https://github.com/BitMiracle-AI/Dormice/blob/main/packages/server/src/e2b/envd/process.ts)
- [`e2bgateway/e2bgateway`](https://github.com/e2bgateway/e2bgateway), [`kubernetes-sigs/agent-sandbox`](https://github.com/kubernetes-sigs/agent-sandbox), [`alibaba/OpenSandbox`](https://github.com/alibaba/OpenSandbox)
- [`huggingface/smolagents`](https://github.com/huggingface/smolagents/blob/main/src/smolagents/remote_executors.py), [`e2b.toml`](https://github.com/huggingface/smolagents/blob/main/e2b.toml), [secure code execution](https://huggingface.co/docs/smolagents/tutorials/secure_code_execution)
- [`NousResearch/hermes-agent`](https://github.com/NousResearch/hermes-agent/blob/main/agent/terminal_env_registry.py), [`terminal_env_provider.py`](https://github.com/NousResearch/hermes-agent/blob/main/agent/terminal_env_provider.py), [`environments/base.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/base.py), [`environments/daytona.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/daytona.py), [`environments/ssh.py`](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/ssh.py)
- [`Firecracker`](https://firecracker-microvm.github.io/)

Internal:

- [Hermes bridge functional inventory](../design/hermes-bridge-inventory.md)
- `pkg/runner/interfaces.go`, `pkg/core/types.go`, `pkg/bridge/hermesssh/{grammar,workspace}.go`
- `.agents/BRIDGE-ALTERNATIVES.md`, `configs/versions.json`
