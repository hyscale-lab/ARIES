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
supply `/bin/sh`. A path that this user cannot write, or an existing file that
it cannot write, gives `PERMISSION_DENIED`, and nothing changes. A write that
is canceled before its rename never lands. A rename that has started runs to
its end, and the call reports its real result.

## Limits

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

## Revocation

`Stop` first marks the session revoked. After that, every procedure is refused
with `UNAVAILABLE`, nothing reaches the executor, and the refusal is recorded
as `rejected`. `Stop` then cancels the running calls and stops the server.
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
exit code. When the output was cut, it adds one line on stderr. A usage error,
a transport failure, or a refusal by the bridge exits 255, as OpenSSH does.

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
removed.

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

- `operation_class` is an E2B operation: `exec` for `Exec`, `stat` for `Stat`,
  `read` for `ReadFile` and `ReadLines`, and `write` for `WriteFile`.
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

## Tests

[Server tests](../../pkg/bridge/grpc/bridge_test.go),
[file tests](../../pkg/bridge/grpc/files_test.go), and
[client tests](../../pkg/bridge/grpc/client_test.go) use a fake executor.
The [managed integration test](../../pkg/bridge/managed_integration_test.go)
runs `hermes-grpc` through the real bridge image with the built client.
The [Hermes tools test](../../pkg/harness/hermes/tools_integration_test.go)
runs real Hermes tools over the gRPC transport; see
[Hermes over gRPC](harnesses.md#hermes-over-grpc).
