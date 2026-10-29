# gRPC bridge implementation

The gRPC transport implements the [ToolBridge contract](../design/bridge.md)
for runs with `bridge.type: "hermes-grpc"`. It is the second transport of the
shared tool bridge, beside [SSH](ssh-bridges.md). One transport serves a whole
run; see [transports](../design/bridge.md#transports).

The [gRPC server](../../pkg/bridge/grpc/bridge.go) is a
[NativeServer](../../pkg/bridge/bridge.go) in the shared `aries-bridge` child.
The [child wiring](../../internal/app/wiring/bridge/child.go) creates one server
for each registered sandbox. Each server binds the listener that the launch
configuration supplies, and serves one borrowed executor. Its endpoint has
protocol `grpc`, user `aries`, and the sandbox workdir. The server is
plaintext and authenticates no caller, as SSH and control do. It makes no
SSH host key. The task container serves nothing and runs no daemon: every call
reaches it through the execution backend of the run, the Docker Engine API or
`kubectl exec` on Kubernetes. Hermes is the harness of this transport: its
ARIES plugin runs the tool calls through the client; see
[Hermes over gRPC](harnesses.md#hermes-over-grpc).

## Origin

The typed operations of this transport are modeled on the process and
filesystem services of E2B. E2B runs a daemon, `envd`, inside each sandbox, and
its SDK gives an agent typed calls to run a command, stat a path, and read or
write a file. ARIES keeps the idea of typed sandbox calls. It serves them from
outside the sandbox, from the bridge. The messages are not the E2B messages;
see [Comparison with E2B](#comparison-with-e2b).

### E2B design

The E2B facts here are for SDK release 2.62.0
([`3a90417`](https://github.com/e2b-dev/E2B/tree/3a904178b2f1d1c40fd2ba1c2ad9a10f43896bf1))
and `envd` 0.8.0 in the infrastructure release 2026.30
([`f32ee8a`](https://github.com/e2b-dev/infra/tree/f32ee8a2a50052f32e3632ceb451111a98dd5104)).

- `envd` is one Go daemon. It runs as root in each sandbox, a Firecracker
  microVM, and listens on port 49983
  ([unit file](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/orchestrator/pkg/template/build/core/rootfs/files/envd.service.tpl#L26-L52)).
  An orchestrator on each node runs the microVMs, and sandbox traffic goes
  through E2B proxies
  ([architecture](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/docs/ARCHITECTURE.md#L15-L27)).
- The process service has `Start`, `Connect`, `List`, `SendInput`,
  `StreamInput`, `CloseStdin`, `SendSignal`, and `Update`
  ([process.proto](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/spec/process/process.proto#L5-L21)).
  `Start` streams events: start, output data, end, and keepalive. The
  filesystem service has `Stat`, `ListDir`, `MakeDir`, `Move`, `Remove`, and
  watch calls
  ([filesystem.proto](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/spec/filesystem/filesystem.proto#L7-L20)).
  It has no read or write call.
- File content moves over plain HTTP, not over RPC. `GET /files` returns a
  whole file, and it honours the HTTP `Range` header
  ([download](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/internal/api/download.go#L120-L172)).
  `POST /files` creates the parents and overwrites the file. It is not atomic:
  `envd` truncates the file in place as root, then writes
  ([upload](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/internal/api/upload.go#L54-L95)).
- Both SDKs use the Connect protocol with the JSON codec, not binary protobuf
  ([Python](https://github.com/e2b-dev/E2B/blob/3a904178b2f1d1c40fd2ba1c2ad9a10f43896bf1/packages/python-sdk/e2b/envd/client_shared.py#L29-L60)).
  `envd` itself serves plain HTTP/1.1
  ([server](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/main.go#L305-L319)).
- A caller proves access with an access token in the `X-Access-Token` header.
  The `/files` routes also accept a signed URL, with an optional expiry
  ([auth](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/internal/api/auth.go#L88-L160)).
- The SDK is a library in the process of the agent. `commands.run` starts
  `/bin/bash -l -c` with the command
  ([SDK](https://github.com/e2b-dev/E2B/blob/3a904178b2f1d1c40fd2ba1c2ad9a10f43896bf1/packages/python-sdk/e2b/sandbox_sync/commands/command.py#L448-L481)).
  The `files` object has `read`, `write`, `list`, `exists`, `get_info`,
  `remove`, `rename`, and `make_dir`
  ([SDK](https://github.com/e2b-dev/E2B/blob/3a904178b2f1d1c40fd2ba1c2ad9a10f43896bf1/packages/python-sdk/e2b/sandbox_sync/filesystem/filesystem.py#L118-L700)).

## Service

The wire contract is [sandbox.proto](../../pkg/bridge/grpc/v1/sandbox.proto),
package `aries.sandbox.v1`. `make proto` regenerates its checked-in bindings
with the control API. The service has five procedures:

- `Exec` takes `script`, `stdin`, and `login`. The server runs the exact argv
  `/bin/bash -c SCRIPT`, or `/bin/bash -l -c SCRIPT` when `login` is set, in the
  sandbox workdir. It uses the absolute `/bin/bash` path and the shell-neutral
  workdir rule that the Hermes SSH dialect also uses
  ([shared bridge kit](../../pkg/bridge/internal/bridgekit/workdir.go)).
  The request carries no environment and no directory. There is no dialect: the
  server does not parse the script. The reply holds the exit code, a `Reason`,
  separate stdout and stderr, and `truncated`. A non-zero exit code is a normal
  reply with status OK. A bridge failure gives exit code 255 and the reason
  `SANDBOX_ERROR`, `CANCELED`, or `TIMED_OUT`.
- `Stat` reports existence, type (regular, directory, other), size, and
  permission bits. A missing path gives `exists=false` with status OK.
- `ReadFile` streams a header (file size and `truncated`), then content chunks
  of the first `max_bytes` bytes. Zero `max_bytes` reads to the end of the
  file.
- `ReadLines` streams a window of lines from `first_line` (1-based) for
  `max_lines` lines, and cuts each line at `max_line_bytes`. A summary comes
  last: `total_lines`, `size`, `ends_with_newline`, and `more`.
- `WriteFile` receives a header (path and size), then chunks. The file changes
  only if exactly `size` bytes arrive. A reader sees the old file or the new
  one. Missing parent directories are created. Parent directories that a
  failed write created remain, as with the `mkdir -p` of stock Hermes. The
  reply holds `bytes_written` and `created`.

File calls follow a symlink, and need an absolute path. The container root `/`
is a directory, as for any shell: `Stat` reports a directory, and `ReadFile`,
`ReadLines` and `WriteFile` refuse it, as any directory, with
`FAILED_PRECONDITION`. No archive or `cat` of a directory starts. A path under
a file, and a symlink whose target is missing, do not exist. The server maps
errors to gRPC codes ([file procedures](../../pkg/bridge/grpc/files.go)):

| Condition | Code |
| --- | --- |
| Relative path, bad range or line window, or a write stream that does not match its header | `INVALID_ARGUMENT` |
| Missing path (except `Stat`) | `NOT_FOUND` |
| Permission denied | `PERMISSION_DENIED` |
| Directory, not a directory, or not a regular file | `FAILED_PRECONDITION` |
| Cancellation by the client or by revocation | `CANCELED` |
| Any other sandbox failure | `INTERNAL` |

The file procedures use the typed file operations of the borrowed target; see
[transports](../design/bridge.md#transports). Docker implements them on the
archive API ([Docker](docker.md)), and Kubernetes with short shell scripts over
`kubectl exec` ([Kubernetes](kubernetes.md)). Reads act as root on Docker, and
as the container user on Kubernetes, which is root on every current benchmark
image. A write acts as the user of the agent's shell (the exec user, or the
image `USER`). It checks write access and creates the missing parents as that
user, and renames the file into place as that user. So the task image must
supply `/bin/sh` and the commands that a write runs; see [Limits](#limits). A
path that this user cannot write, or an existing file that it cannot write,
gives `PERMISSION_DENIED`, and nothing changes. A write that is canceled before
its rename never lands. A rename that has started runs to its end, and the call
reports its real result.

## Limits

- `Exec` is unary and does not stream output, unlike SSH. The client gets no
  stdout or stderr until the command ends. A call that is canceled or times out
  gives the client no output. Thus an agent sees no output of a long foreground
  command until it ends. Hermes runs a background process with its output in a
  log file, and polls the log with short commands, so polling works. This is a
  known limitation that we accept for now.
- The agent sees output in a different order than on SSH. The plugin merges the
  client's stderr into its stdout, but the
  [client](../../pkg/bridge/grpc/client.go) writes all of stdout, then all of
  stderr. So error lines come at the end, not next to the line that caused
  them. The client's own lines, for example
  `aries: REASON_SANDBOX_ERROR`, also come in the agent's output. SSH streams
  both outputs, so their lines stay near the order the command wrote them.
- On Docker, an `Exec` ends only when the output of the command closes
  ([Docker exec](../../pkg/deployment/docker/exec.go)). A background child that
  keeps the output open, for example `cmd &` with no redirect, thus blocks the
  call until the child exits. SSH uses the same backend and does the same.
- On Docker, typed stat, read and line reads use the archive API of the daemon.
  This API cannot see `/proc`, `/sys`, `/dev` or tmpfs mounts (`/dev/shm`,
  `--tmpfs`); see [Docker](docker.md#runtime-creation-and-execution). A typed
  read reports a file there as missing, also through a symlink such as
  `/etc/mtab`, while `cat` in the shell reads it. A typed write into a tmpfs
  path fails with `INTERNAL` "sandbox file access failed": the upload goes into
  the directory under the mount, and the rename cannot find it.
- On Kubernetes, `stat` reports size 0 for a `/proc` file. So `read_file` of
  such a file returns "File is empty (0 bytes)", while `cat` in the shell reads
  it ([Kubernetes files](../../pkg/deployment/kubernetes/files.go)).
- Typed calls and the shell see the filesystem differently. Typed stat and read
  act as root (on Docker, through the daemon). Search, patch reads, delete and
  move run in the shell as the agent's user. So `read_file` can report a file
  as missing that `search_files` finds, and can read a file that the shell
  cannot. See [transports](../design/bridge.md#transports) and
  [Hermes over gRPC](harnesses.md#hermes-over-grpc).
- On Docker, an agent that owns a parent directory can swap it for a symlink
  between the access check and the upload of a write. The daemon then puts the
  temporary file, as root, in the symlink target; see
  [Docker](docker.md#runtime-creation-and-execution).
- On Docker, a typed write needs `/bin/sh`, `mkdir`, `mv` and `/bin/rm` in the
  task image ([Docker files](../../pkg/deployment/docker/files.go)). On
  Kubernetes, the file scripts also need `stat`, `readlink`, `cat`, `chmod` and
  `wc`.
- The service has no PTY, signal, reattach, list, move or remove call; see
  [Comparison with E2B](#comparison-with-e2b). Hermes does list, move and
  remove with shell commands through `Exec`.
- Each operation starts a new client process and a new HTTP/2 connection, and
  the client has no retry policy. One `read_file` of a text file makes three
  calls: a stat, a read of the first 1000 bytes, and the line read. Hermes over
  SSH reuses one ControlMaster connection.
- `Exec` stdin above 16 MiB is refused with `RESOURCE_EXHAUSTED` before
  anything runs. A workdir that is not shell-neutral is refused with
  `FAILED_PRECONDITION`. Both refusals are recorded with status `rejected` and
  exit code -1.
- The server keeps at most 16 MiB of stdout and 16 MiB of stderr for each
  `Exec`. Above that, it drops the remainder and sets `truncated`, and the
  record keeps the true byte counts. This bound protects the shared bridge
  process, because a unary reply is held in memory. With the Docker and
  Kubernetes backends, this bound does not fire: the backend fails an exec
  whose stdout or stderr passes 16 MiB
  ([`maxExecOutput`](../../pkg/deployment/docker/exec.go)), the same as for
  SSH. The call then ends with `REASON_SANDBOX_ERROR` and exit code 255.
- Each gRPC message is at most 64 MiB, on the server and on the client. The
  output bound always fires first.
- File content has no size bound. It moves in 64 KiB chunks. The server holds
  one chunk at a time, plus written content up to 16 MiB for the audit.
- A `WriteFile` above 16 MiB runs. The audit keeps no content for it: the
  record holds a size note with `stdin_encoding` `size-omitted`, and the true
  `stdin_bytes` and `sha256`. SSH is different: stdin above 16 MiB fails the
  SSH command and latches an audit error, so that session's evidence fails.
- The [audit](../../pkg/bridge/internal/bridgekit/audit.go) of one sandbox
  session is at most 256 MiB, for `tool-calls.jsonl` and the raw log together.
  Printable written content up to 16 MiB and printable `Exec` stdin are inline
  and count toward this cap. After the cap, every later call gets `UNAVAILABLE`,
  `Stop` returns the latched error, release fails, and the task is not
  evaluated; see [Revocation](#revocation).

## Revocation

`Stop` first marks the session revoked. After that, every procedure is refused
with `UNAVAILABLE`, nothing reaches the executor, and the refusal is recorded
as `rejected`. After the audit latches an error, for example at its size cap,
every procedure is also refused with `UNAVAILABLE`, because the bridge can no
longer record a call.
`Stop` then cancels the running calls and stops the server.
The server waits for its handlers (`WaitForHandlers`), and `Stop` seals the
audit only after every handler returned. Thus no record arrives after the
seal. If the context of `Stop` expires first, the session stays pending, and a
later `Stop` can try again. `Stop` is idempotent.

A canceled call closes its backend execution and may leave its command running
in the sandbox, as for SSH. A canceled call, also one that passes its
deadline, is recorded as `canceled`. A tool error, also one that a call returns
during cancellation, stays in the record and does not fail revocation.
Only a handler join that does not end and a failure to seal the audit prevent
confirmed release; see [lifecycle](../design/bridge.md#lifecycle-cancellation-and-failure).

## Client

ARIES stages the static `aries-grpc-client` into the Hermes container at
`/opt/aries/bin/aries-grpc-client`. `make build` builds it with
`CGO_ENABLED=0`. Keep it beside `bin/aries`. The
[client](../../pkg/bridge/grpc/client.go) runs one operation for each process:

```text
aries-grpc-client exec [--login] [--] SCRIPT
aries-grpc-client file stat PATH
aries-grpc-client file read [--max-bytes N] PATH
aries-grpc-client file lines --first N --max N [--max-line-bytes N] PATH
aries-grpc-client file write --size N PATH < content
```

The client reads the bridge address from `ARIES_GRPC_TARGET`. It dials in
plaintext and ignores the `*_PROXY` variables, because a proxy outside the run
network cannot reach the bridge. `exec` reads all of stdin before it dials. It
writes the command's stdout and stderr unchanged and exits with the command's
exit code. When the output was cut, it adds one line on stderr. When the
bridge reports that the command did not complete, the client exits 255 and
adds the reason on stderr, for example `aries: REASON_SANDBOX_ERROR`. A usage
error, a transport failure, or a refusal by the bridge exits 255, as OpenSSH
does.

For `file`, stdout holds only the payload. `stat` writes one JSON line on
stdout. `read`, `lines`, and `write` write one JSON metadata line on stderr on
success, and a message on failure. Content streams through and is never held
whole. If a read fails after some content, that content stays on stdout, so
check the exit code before you use stdout. The `file` exit codes are:

| Exit code | Status |
| --- | --- |
| 0 | OK |
| 1 | `NOT_FOUND` |
| 2 | `PERMISSION_DENIED` |
| 3 | `INVALID_ARGUMENT` |
| 4 | `RESOURCE_EXHAUSTED` |
| 5 | `FAILED_PRECONDITION` |
| 255 | Any other status, a usage error, or a transport failure |

The client has no timeout flag. Hermes stops the client process when a command
times out, and the closed connection cancels the call on the bridge. The
command may keep running in the sandbox until it exits or the sandbox is
removed. The Hermes plugin stops a `file` client after three times the Hermes
terminal timeout (540 s with the default 180 s) and reports a tool error. One
file call can run several operations in the sandbox, but Hermes gives each
single shell command only the terminal timeout.

When `ARIES_TOOL_CALL_ID` is set and not empty, the client sends its value as
the gRPC metadata `aries-tool-call-id` on each call. The client does not check
the value. The bridge accepts one value of at most 128 bytes, made of the
characters `A-Z`, `a-z`, `0-9`, `.`, `_`, `:`, and `-`. It refuses any other
value with `INVALID_ARGUMENT` before the call reaches the sandbox.

## Evidence

The gRPC transport writes `tool-calls.jsonl` with the same rules as SSH,
through the shared [audit writer](../../pkg/bridge/internal/bridgekit/audit.go);
see [bridge evidence](../run-results.md#bridge-evidence). The
[gRPC records](../../pkg/bridge/grpc/audit.go) have these differences:

- `operation_class` is an [E2B operation class](#e2b-operation-classes): `exec`,
  `stat`, `read`, or `write`.
- An `Exec` record has `path` `/bin/bash`, the exact `argv`, the script as
  `command`, and the SHA-256 of the script as `command_hash`.
- A file call has no exit code, so its record has exit code -1 and an empty
  `command_hash`. `status` is `completed`, `failed`, `canceled`, or `rejected`.
- `WriteFile` content follows the stdin rules: printable UTF-8 is inline as
  `stdin`, and binary content is a `binary-omitted` marker.
- `ReadFile` and `ReadLines` record the path and the bytes sent as
  `stdout_bytes`, never the content. Both are a `read`; the read window tells
  them apart. A `ReadFile` with a limit records `max_bytes`, and a whole-file
  read records no window. A `ReadLines` records `first_line`, `max_lines`, and
  `max_line_bytes` when it is not zero. It records `total_lines` (also 0 for an
  empty file) only when it returned a summary.
- `sha256` is the hex digest of the content that a completed file call moved.
- `truncated` is set when `Exec` output was cut (never with the Docker backend,
  see above), when a `ReadFile` range ends before the end of the file, or when
  `ReadLines` has more lines.
- `tool_call_id` is the `aries-tool-call-id` metadata of the call, also on a
  refused call. A call without the metadata has no `tool_call_id`. A call
  refused for a bad id has none either.
- gRPC records have no `request_type` or `want_reply`. Those fields are SSH only.

`grpc_raw.log` is opt-in through `bridge.retain_raw_log` and has mode 0600. Each
call is one record between `--- ARIES GRPC CALL BEGIN ---` and
`--- ARIES GRPC CALL END ---`. The fixed-order `key=value` lines are
`sequence`, `sandbox_id`, `timestamp`, `operation_class`, `status`, `run_id`,
`task_id`, `container_id`, `path`, `login`, `script`, `stdin_bytes`, `stdin`,
`read_bytes`, and `sha256`, with the escapes of `ssh_raw.log`. The log keeps
the `Exec` script and stdin and the `WriteFile` content (or the size note above
16 MiB). A read keeps only its path, byte count, and digest. The log never
holds output or read content.

### E2B operation classes

Each gRPC record names the E2B operation of its call in `operation_class`.
The mapping is one-to-one:

| `operation_class` | ARIES procedure | E2B `envd` call |
| --- | --- | --- |
| `exec` | `Exec` | `process.Process/Start` |
| `stat` | `Stat` | `filesystem.Filesystem/Stat` |
| `read` | `ReadFile` and `ReadLines` | `GET /files` |
| `write` | `WriteFile` | `POST /files` |

E2B has no call that reads lines. `ReadLines` is a `read` too, and the read
window fields tell the two ARIES procedures apart.

This is a label mapping. It is not E2B wire compatibility and not E2B log
compatibility. E2B publishes no format for a tool-call log, so there is no
format to match. The `envd` log is a free-form operational log for E2B. It
does have a `process_end` line with `stdout_bytes` and `stderr_bytes` for a
process
([source](https://github.com/e2b-dev/infra/blob/f32ee8a2a50052f32e3632ceb451111a98dd5104/packages/envd/internal/services/process/handler/handler.go#L560-L618)).

These fields of a gRPC record have no E2B counterpart:

- `sha256`, the digest of the content that a file call moved.
- `stdin_bytes`, `stdout_bytes`, and `stderr_bytes`, the byte counts. Only the
  `process_end` log line above has a similar count.
- The read window: `max_bytes`, `first_line`, `max_lines`, `max_line_bytes`,
  and `total_lines`.
- `truncated`.
- `tool_call_id`, the id of the harness tool call.
- `started_at`, the time the bridge started the call.

## Comparison with E2B

The E2B column is for the revisions in [E2B design](#e2b-design). The ARIES
column is this transport.

| Axis | E2B | ARIES gRPC transport |
| --- | --- | --- |
| Server place | `envd` in each microVM, as root | The shared bridge child, outside the sandbox; the task container runs nothing |
| Wire | Connect protocol, JSON codec; HTTP/2 to the E2B edge, HTTP/1.1 into the microVM | gRPC, protobuf binary, plaintext |
| Commands | Long-lived processes with streamed events, PTY, signals, and reattach | One unary `Exec` with `script`, `stdin`, and `login`; no PTY, no signal, no reattach |
| Caller controls | Command, arguments, environment, directory, user | Script, stdin, and login only; the bridge sets the workdir and passes no environment |
| File calls | `Stat`, `ListDir`, `MakeDir`, `Move`, `Remove`, watch, and `/files` | `Stat`, `ReadFile`, `ReadLines`, and `WriteFile`; list, move, and delete stay on the shell |
| File content | Separate HTTP route, whole file, `Range` supported | The same gRPC service in 64 KiB chunks, with a byte limit or a line window |
| Write | Truncates in place as root; no size check | Exact size, temporary file and rename as the agent's user |
| Missing path | An error | `exists=false` for `Stat`, with status OK |
| Authentication | Access token or signed URL | None; plaintext on the run network |
| Revocation | None for one session; access ends with the sandbox | Every call is refused with `UNAVAILABLE`, handlers are joined, and the audit is sealed before evaluation |
| Timeouts | A request header can limit a process; closing the stream does not stop it | No server deadline; the harness stops the client and the closed connection cancels the call |
| Output limits | No byte limit found | 16 MiB for each of stdout and stderr; 64 MiB for a message |
| Evidence | Operational log for E2B; no tool-call format | `tool-calls.jsonl` for each sandbox call, and an optional raw log |
| Sandbox runtime | Firecracker microVM | Docker or Kubernetes container |

### Common to both

- A typed call for each file operation. Stat, read, and write are separate from
  command execution.
- The command runs as `/bin/bash -l -c SCRIPT`. ARIES uses `-l` only when
  `login` is set.
- Stdout and stderr are separate. A non-zero exit code is a result, not a
  transport error.
- A write creates the missing parent directories.
- A file read acts as root on Docker.
- A canceled call can leave its command running in the sandbox.
- A protobuf schema defines the calls. The codec is a free choice.

### Different on purpose, and why

- **No daemon in the task container.** The bridge is outside the sandbox. It
  owns the access to the sandbox, so it can revoke the access before
  evaluation; see
  [lifecycle](../design/bridge.md#lifecycle-cancellation-and-failure).
- **ARIES messages, not E2B messages.** No E2B wire compatibility is
  implemented or verified; see [supported](../supported.md) and
  [deployment](../design/deployment.md).
- **A narrow request.** `Exec` carries only the fields that ARIES honours. It
  has no directory, environment, PTY, or reattach.
- **No authentication, but revocation for each session.** The research network
  is trusted. Isolation comes from the exact sandbox binding and from
  revocation before evaluation; see
  [control and native protocols](../design/bridge.md#control-and-native-protocols).
- **Atomic write as the agent's user.** A reader sees the old file or the new
  one. A typed write reaches only what the agent's own shell can write; see
  [transports](../design/bridge.md#transports).
- **`ReadLines` and `max_bytes`.** Only the requested window crosses the wire.
- **Unary `Exec` with bounded output.** The bridge process is shared, so a
  unary reply must fit in memory; see [Limits](#limits).
- **No server deadline.** The harness owns the timeout; see [Client](#client).
- **Evidence for each call.** Each sandbox call has a record with `sandbox_id`
  and a sequence number; see
  [evidence](../design/bridge.md#evidence-and-substitution).

## Tests

[Server tests](../../pkg/bridge/grpc/bridge_test.go),
[file tests](../../pkg/bridge/grpc/files_test.go), and
[client tests](../../pkg/bridge/grpc/client_test.go) use a fake executor.
The [managed integration test](../../pkg/bridge/managed_integration_test.go)
runs `hermes-grpc` through the real bridge image with the built client.
The [Hermes tools test](../../pkg/harness/hermes/tools_integration_test.go)
runs real Hermes tools over the gRPC transport; see
[Hermes over gRPC](harnesses.md#hermes-over-grpc).
