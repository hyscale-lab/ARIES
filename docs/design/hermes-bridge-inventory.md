# Hermes bridge functional inventory

## 1. Purpose and how to use this inventory

This document is a complete behavioural inventory of `pkg/bridge/hermesssh`, written for a reader
who intends to replace its SSH wire protocol with an `RPC` schema. It records what the current
implementation does and what it guarantees, so each item can be ticked off against a replacement
rather than rediscovered from the diff.

Every claim carries a `file:line` citation. Guarantees are stated transport-independently wherever
the guarantee does not depend on SSH; where it does, the entry is marked and section 13 classifies
it as a transport artifact that a replacement may discard, or a real guarantee it must reproduce.

Absences are recorded as deliberately as presences. A migration that adds retries, buffering,
rate limiting, or session reuse changes the security argument, so the places where this package
has none are listed explicitly.

Scope: `bridge.go` (1156 lines), `grammar.go` (170), `workspace.go` (85), and the four test files.
The Hermes harness itself, the Docker sandbox, and the Runner are referenced only where they
constrain the bridge.

## 2. The contract it implements

The package exports one type, `Manager`, and asserts the role contract at compile time
(`pkg/bridge/hermesssh/bridge.go:530`):

```go
var _ runner.ToolBridge = (*Manager)(nil)
```

`runner.ToolBridge` has two methods (`pkg/runner/interfaces.go:51-56`):

```go
// ToolBridge grants and then positively revokes harness access to a sandbox.
// A nil Stop error is the positive revocation confirmation.
type ToolBridge interface {
	Start(context.Context, Sandbox) (core.ToolEndpoint, error)
	Stop(context.Context) error
}
```

### Endpoint fields populated

`Start` returns this value (`pkg/bridge/hermesssh/bridge.go:649-653`):

```go
return core.ToolEndpoint{
	Protocol: "ssh", Address: address, Username: lockedUsername, Network: network,
	IdentityFile: identityContainerPath, IdentitySourceFile: session.identitySource,
	LogPaths: session.logPaths(),
}, nil
```

| `core.ToolEndpoint` field | Value | Source |
| --- | --- | --- |
| `Protocol` | `"ssh"` | `bridge.go:650` |
| `Address` | `host:port` of the listener, ephemeral port | `bridge.go:611`, `bridge.go:646` |
| `Username` | `"aries"` (`lockedUsername`) | `bridge.go:38`, `bridge.go:650` |
| `Network` | `sandbox.NetworkName()` | `bridge.go:647` |
| `IdentityFile` | `/run/aries/ssh/id_ed25519`, the in-container path | `bridge.go:37`, `bridge.go:651` |
| `IdentitySourceFile` | host path of the generated private key | `bridge.go:597`, `bridge.go:651` |
| `LogPaths` | `tool-calls.jsonl`, plus `ssh_raw.log` when retained | `bridge.go:658-663` |

Deliberately left empty:

- `ClientCommand` and `ClientSourceFile`. Hermes runs its own OpenSSH, so no client helper is
  staged or advertised. Pinned by `pkg/bridge/hermesssh/bridge_test.go:181-183`.
- `KnownHostsFile` and `KnownHostsSourceFile`. Hermes forces `StrictHostKeyChecking=accept-new`
  and offers no way to preload a known-hosts file, so handing one over would imply a guarantee the
  bridge cannot enforce. The generated line is written to the artifact directory as evidence only
  (`bridge.go:615-621`).

`core.ToolEndpoint` carries no credential bytes; only paths (`pkg/core/types.go:90-105`).

### What the Runner requires

- `Start` is called after the benchmark has sanitized the live sandbox and before harness start
  (`pkg/runner/runner.go:217`).
- `bridgeActive` is set to `true` immediately after the call, before the error is examined, so a
  failed `Start` is still followed by a `Stop` (`pkg/runner/runner.go:218-225`). `Start` therefore
  must tolerate being partially completed and `Stop` must be `idempotent`.
- `endpoint.LogPaths` is copied into the task result as `ToolLogPaths`
  (`pkg/runner/runner.go:226`), so advertised paths must exist.
- `Stop` runs inside a fresh bounded context derived with `context.WithoutCancel`
  (`pkg/runner/runner.go:261`), after harness stop, and a `nil` return is the positive revocation
  confirmation (`pkg/runner/runner.go:268-273`).
- A non-`nil` `Stop` sets `Isolation.Status` and `Evaluation.Status` to
  `core.StatusBlockedIsolation` and returns before `Evaluate` (`pkg/runner/runner.go:276-283`).
  Evaluation and verifier upload never happen if revocation was not confirmed.

Concurrency contract: one active session per `Manager`. A second `Start` while a session is active
or while `Stop` is in flight is rejected (`bridge.go:558-560`).

## 3. Lifecycle: `Start`

`Start` (`bridge.go:555-654`) runs entirely under `manager.mu`, so it is serialized against `Stop`
and against another `Start`.

Steps in order:

1. **Single-session guard.** `manager.active != nil || manager.stopping` returns
   `"Hermes SSH bridge is already active"` (`bridge.go:558-560`). No session object exists yet, so
   nothing is left behind.

2. **Sandbox capability assertion.** `generic.(bridgeSandbox)` (`bridge.go:561-564`). The interface
   (`bridge.go:73-83`) is structural; the package does not import the Docker sandbox package.

   ```go
   type bridgeSandbox interface {
   	runner.Sandbox
   	ContainerID() string
   	ContainerName() string
   	NetworkName() string
   	NetworkGateway(context.Context) (string, error)
   	RunID() string
   	TaskID() string
   	Workdir() string
   	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
   }
   ```

   | Method | Why it is required |
   | --- | --- |
   | `runner.Sandbox` | the base role contract (`Exec`, `Upload`, `Download`); not otherwise used by the bridge |
   | `ContainerID()` | evidence field on every structured and raw record (`bridge.go:842`, `bridge.go:889`) |
   | `ContainerName()` | evidence field and start log field (`bridge.go:842`, `bridge.go:648`) |
   | `NetworkName()` | `ToolEndpoint.Network` and the start log field (`bridge.go:647`) |
   | `NetworkGateway(ctx)` | the bind address; the listener sits on the task network gateway (`bridge.go:565`, `bridge.go:606`) |
   | `RunID()` | evidence correlation on every record (`bridge.go:885`, `bridge.go:887`) |
   | `TaskID()` | evidence correlation and the artifact directory name (`bridge.go:573`, `bridge.go:886`) |
   | `Workdir()` | the authoritative working directory for every command (`bridge.go:785`) |
   | `ExecStream(...)` | streaming execution, so command output never lands in the agent-writable container filesystem (`bridge.go:812`) |

   The failure message names Docker (`"requires the local Docker sandbox capability"`), but the
   assertion is on the method set, not on a concrete type.

3. **Gateway resolution.** `sandbox.NetworkGateway(ctx)` (`bridge.go:565-568`). Failure returns
   before a session exists; nothing is created, and no `fail` rollback runs.

4. **Session construction.** `bridgeSession` with an empty connection set and the default
   `replyRequest` closure (`bridge.go:569-572`). `artifactDir` is
   `<outputDir>/<taskID>/bridge` (`bridge.go:573`).

   From this point every failure goes through `fail` (`bridge.go:574-589`), described below.

5. **Artifact directory.** `ensurePrivateDirectory(session.artifactDir)` (`bridge.go:590-592`)
   creates the path `0700`, resolves symbolic links, rejects the path if
   `filepath.EvalSymlinks` differs from `filepath.Abs`, and re-applies `0700`
   (`bridge.go:1133-1149`). The same function guards `Options.OutputDir` in `New`
   (`bridge.go:540-542`).

6. **Session key generation.** `generateSessionKeys` (`bridge.go:1036-1058`) creates two
   independent `Ed25519` key pairs per session: a host key wrapped in an `ssh.Signer`, and a
   client key marshalled to `PKCS8` `PEM` (`bridge.go:1060-1066`). Only the client private key is
   written to disk; the host private key exists only in memory. Nothing is reused across sessions
   or tasks.

7. **Artifact paths.** `id_ed25519`, `known_hosts`, `tool-calls.jsonl`, and — only when
   `omitRawLog` is false — `ssh_raw.log` (`bridge.go:597-602`).

8. **Identity file write.** `writeExclusivePrivate(session.identitySource, clientPEM)`
   (`bridge.go:603-605`) uses `writeExclusiveWithOperations` (`bridge.go:1093-1131`):
   `O_WRONLY|O_CREATE|O_EXCL` at `0600`, write, `Sync`, `Chmod`, `Sync`, `Close`. Any step failing
   closes and removes the partial file and joins all errors. An existing file at that path is a
   hard failure, never an overwrite.

9. **Listener bind.** `net.Listen("tcp4", net.JoinHostPort(gateway, "0"))` (`bridge.go:606-610`).
   `IPv4` only, ephemeral port, bound to the task network gateway address in the ARIES process —
   ARIES is the server, Hermes is the client. `net.SplitHostPort` on the resolved address supplies
   the advertised host and port (`bridge.go:611-614`).

10. **Known-hosts evidence line.** (`bridge.go:615-621`)

    ```go
    knownLine := fmt.Sprintf("[%s]:%s %s", host, port, ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
    ```

    Written with the same exclusive `0600` routine. It is evidence of the key Hermes pins on first
    use, not an input to the harness.

11. **Audit files.** `manager.openAudit(session.toolLogPath)` (`bridge.go:622-625`), then the raw
    log when the path is set (`bridge.go:626-632`). `openAuditFile` uses
    `O_WRONLY|O_CREATE|O_EXCL` at `0600` (`bridge.go:261-267`), so a pre-existing artifact fails
    the start. If the raw log cannot be opened, the already-open structured file is closed and the
    close error is joined into the failure (`bridge.go:630`).

12. **Audit writer.** `newAuditWriter` starts the single writer `goroutine` (`bridge.go:633`,
    `bridge.go:269-277`).

13. **Serve context and server configuration.** A `context.Background()`-rooted cancellable
    context is created — the serve loop deliberately does not inherit the caller's context, so
    revocation is driven only by `revoke` (`bridge.go:634-635`). `newServerConfig` builds the
    `ssh.ServerConfig` (`bridge.go:636`, `bridge.go:665-677`).

14. **Serve `goroutine`.** `session.wait.Add(1)` then `go session.serve(...)`
    (`bridge.go:637-638`).

15. **Test hook.** `manager.afterStart` is called when non-`nil` (`bridge.go:639-643`). No code in
    this package or repository assigns it for the Hermes bridge; it is always `nil` here. Only the
    OpenClaw bridge's own test uses its separate copy (`pkg/bridge/openclawssh/bridge_test.go:914`).

16. **Publication.** `manager.active = session`, `manager.stopErr = nil`, one structured start log
    with `address`, `network`, and `container` fields (`bridge.go:644-648`), then the endpoint.

### The `fail` rollback path

```go
fail := func(primary error) (core.ToolEndpoint, error) {
	session.partialStart = true
	session.revoke()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	defer cancel()
	waitErr := session.waitFor(cleanupCtx)
	if waitErr != nil {
		manager.active = session
		return core.ToolEndpoint{}, errors.Join(primary, waitErr)
	}
	cleanupErr := session.finalize(cleanupCtx)
	if cleanupErr != nil {
		manager.active = session
	}
	return core.ToolEndpoint{}, errors.Join(primary, cleanupErr)
}
```

(`bridge.go:574-589`.) Properties:

- The cleanup context is detached from the caller's context and bounded by
  `manager.cleanupTimeout` (default 20 seconds, `bridge.go:33`, `bridge.go:543-545`), so rollback
  still runs when `Start` was cancelled.
- If the rollback itself does not complete, the session is published as `manager.active` so the
  Runner's mandatory later `Stop` can retry it. This is why `bridgeActive` is set before the error
  check in `pkg/runner/runner.go:218-221`.
- `partialStart` makes `finalize` remove the whole artifact directory once every other cleanup step
  succeeded (`bridge.go:947-949`). A partial start therefore leaves no evidence directory behind.

What is left behind per failure point, assuming rollback succeeds:

| Failure point | State at failure | After rollback |
| --- | --- | --- |
| capability assertion (`bridge.go:561`) | nothing created | nothing |
| gateway resolution (`bridge.go:565`) | nothing created | nothing |
| artifact directory (`bridge.go:590`) | directory may be partly created | directory removed |
| key generation (`bridge.go:593`) | empty artifact directory | directory removed |
| identity write (`bridge.go:603`) | partial file already removed by `writeExclusive` | directory removed |
| listener bind (`bridge.go:606`) | identity file on disk | identity removed, directory removed |
| address parse (`bridge.go:611`) | listener open, identity on disk | listener closed, directory removed |
| known-hosts write (`bridge.go:619`) | listener open, identity on disk | listener closed, directory removed |
| structured log open (`bridge.go:622`) | listener open, identity and known-hosts on disk | listener closed, directory removed |
| raw log open (`bridge.go:628`) | structured file explicitly closed first | directory removed |
| `afterStart` (`bridge.go:640`) | full session running, `goroutine` serving | context cancelled, listener and connections closed, audit sealed and drained, directory removed |

If rollback does not succeed, everything named in the "state at failure" column stays on disk and
the session remains active for the Runner's `Stop` to retry.

## 4. Lifecycle: `Stop` and revocation

`Stop` (`bridge.go:893-933`).

**Nothing to do.** With no active session and no `Stop` in flight, `Stop` returns the stored
`manager.stopErr` (`bridge.go:895-899`). Before any `Start` that is `nil`; after a successful
`Stop` it is `nil`; after a failed `Stop` it is the previous failure, returned again.

**Concurrent caller.** A second caller while `stopping` is true waits on the `stopDone` channel and
then returns the shared result, or returns `ctx.Err()` if its own context expires first
(`bridge.go:900-912`). It does not start a second revocation.

**The revocation sequence** (`bridge.go:919-923`):

```go
session.revoke()
err := session.waitFor(ctx)
if err == nil {
	err = session.finalize(ctx)
}
```

`revoke` (`bridge.go:997-1011`) is guarded by `sync.Once` and does three things: cancels the serve
context, closes the listener, and closes every live connection under `session.mu`. Cancelling the
serve context is what terminates an in-flight `ExecStream`, because `handleSession` passes the
per-connection context down into it (`bridge.go:715`, `bridge.go:812`). Revocation does not wait
for in-flight work to finish; it aborts it.

`waitFor` (`bridge.go:1013-1022`) waits on `session.wait`, which counts the serve `goroutine`, each
connection handler, and each channel handler, against the caller's context. A timeout returns
`ctx.Err()` and `finalize` is skipped, so the identity file is not removed and `Stop` fails.

`finalize` (`bridge.go:935-951`):

```go
auditErr := session.closeAudit(ctx)
if session.audit != nil && !session.audit.finished() {
	return auditErr
}
cleanupErr := errors.Join(
	session.revocationError(), auditErr,
	removeIfPresent(session.identitySource),
)
if session.partialStart && cleanupErr == nil {
	cleanupErr = os.RemoveAll(session.artifactDir)
}
return cleanupErr
```

`closeAudit` calls `sealAndWait` (`bridge.go:1024-1029`, `bridge.go:500-516`). If the writer
`goroutine` has not finished — the drain timed out — `finalize` returns early and does **not**
remove the private identity, because the writer may still touch its files. `Stop` then fails and
evaluation is blocked.

**Deleted on `Stop`:** only `id_ed25519` (`bridge.go:945`). That deletion is the credential half of
revocation, alongside the closed listener and connections.

**Retained on `Stop`:** `known_hosts`, `tool-calls.jsonl`, and `ssh_raw.log` when written. The
retention rationale is stated inline at `bridge.go:940-942`, pinned by
`bridge_test.go:345-376`, and the artifact permissions (`0600`) are pinned by
`integration_test.go:401-406`.

**Retained on a partial start:** nothing; the whole artifact directory is removed
(`bridge.go:947-949`).

**`Stop` returns non-`nil` exactly when:**

1. The caller's context expires while waiting for handlers (`bridge.go:920`, `bridge.go:1019`).
2. The caller's context expires while draining the audit (`bridge.go:513-515`), or the audit writer
   latched any error — marshal, render, budget overflow, write, short write, `sync`, or close
   (`bridge.go:299-316`, `bridge.go:456-497`).
3. The audit writer did not finish (`bridge.go:937-939`).
4. `session.revocationError()` is non-`nil` — a recorded execution error that is not pure
   cancellation (`bridge.go:962-969`).
5. `os.Remove` of the identity fails for a reason other than "does not exist"
   (`bridge.go:1151-1156`).
6. On a partial start only, `os.RemoveAll` of the artifact directory fails (`bridge.go:948`).

On failure, `manager.active` stays set and `stopping` returns to false (`bridge.go:924-931`), so a
later `Stop` retries `waitFor` and `finalize`. `revoke` will not run twice, which is correct: it is
already complete.

### `hasCancellationCause` and `isPureCancellation`

```go
func hasCancellationCause(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
```

(`bridge.go:971-973`.) It answers "is this error attributable to revocation?". `execute` uses it
twice: to decide whether a sandbox error already carries the cancellation cause
(`bridge.go:813`), and `recordRevocationError` uses it to decide whether the error belongs in the
revocation ledger at all (`bridge.go:953-960`). A plain sandbox failure during normal operation is
recorded in the tool log as `failed` and does not block revocation.

```go
func isPureCancellation(err error) bool { ... }
```

(`bridge.go:975-995`.) It walks joined errors (`Unwrap() []error`) and wrapped errors
(`Unwrap() error`) and requires every leaf to be exactly `context.Canceled` or
`context.DeadlineExceeded`. A joined error with zero children returns false, as does `nil`.
`revocationError` uses it to suppress the expected case — commands aborted *by* revocation are not
a revocation failure — while any error carrying additional non-cancellation content survives and
fails `Stop`.

The two exist to separate "the tool call died because we revoked" from "the tool call died and we
cannot prove it stopped". The second must block evaluation; the first must not.

## 5. Connection and channel handling

This section is the most SSH-specific in the document. The framing is replaceable; the policies
expressed through it are not.

**Accept loop** (`bridge.go:679-695`). Unbounded: every accepted connection is registered in
`session.connections` and handed to a new `goroutine`. There is no connection limit, no accept
backoff, and no per-peer check beyond authentication. Accept errors are logged as warnings unless
the context is cancelled or the listener is closed.

**Handshake deadline.** `lockedConnectTimeout` is 5 seconds (`bridge.go:39`) and is applied to the
raw connection before `ssh.NewServerConn` (`bridge.go:705`). It is cleared immediately after the
handshake (`bridge.go:714`) because Hermes holds one ControlMaster connection open for the whole
run. There is consequently **no idle timeout** on an established connection.

**Authentication** (`bridge.go:665-677`):

```go
configuration := &ssh.ServerConfig{
	MaxAuthTries: 3,
	PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if metadata.User() != lockedUsername || !bytes.Equal(key.Marshal(), authorized.Marshal()) {
			return nil, errors.New("public key rejected")
		}
		return &ssh.Permissions{}, nil
	},
}
configuration.AddHostKey(hostSigner)
```

- Public key only. No password, keyboard-interactive, `none`, or certificate callback is set, so
  the library offers no other method.
- The username is locked to `aries` (`bridge.go:38`); any other user is rejected regardless of key.
- Key comparison is an exact byte comparison of the marshalled wire form against the single
  session key. There is no authorized-keys list, no algorithm negotiation policy beyond the
  library default, and no key expiry other than session lifetime.
- `MaxAuthTries: 3`.
- Empty `ssh.Permissions`: no extensions, no critical options, nothing carried into the session.
- Authentication attempts and failures are **not** written to either audit file.

**Host key.** One ephemeral `Ed25519` host key per session (`bridge.go:1037-1043`), advertised only
through the retained `known_hosts` evidence line.

**Channels** (`bridge.go:722-737`). Only `session` channels with zero-length `ExtraData` are
accepted; everything else is rejected with `ssh.UnknownChannelType`. There is no `direct-tcpip`,
`forwarded-tcpip`, or `x11` support, so no port forwarding is possible over the grant.

**Global requests** (`bridge.go:740-747`):

```go
func serveGlobalRequests(requests <-chan *ssh.Request) {
	for request := range requests {
		accepted := request.Type == "keepalive@openssh.com" && len(request.Payload) == 0
		if request.WantReply {
			_ = request.Reply(accepted, nil)
		}
	}
}
```

Only `keepalive@openssh.com` with an empty payload is accepted. `tcpip-forward` and every other
global request is refused. Global requests are **not** recorded in either audit file — the audit
covers channel requests only.

**Non-`exec` channel requests** (`bridge.go:752-763`). The request is refused with a negative reply
when a reply is wanted, recorded with status `unsupported` and class `unknown`, and the loop
**continues** — the channel stays open. The inline comment states the reason:

```go
// OpenSSH sends an `env` request on every channel before the exec.
// Refusing it is correct and expected, but the channel must stay
// open or Hermes loses every command it ever issues.
```

Closing the channel on the refused `env` request would break every Hermes command. This covers
`env`, `pty-req`, `shell`, `subsystem`, `signal`, and `window-change` alike: refused, recorded,
channel preserved. Pinned by `bridge_test.go:109-128` and `bridge_test.go:240-249`.

**One `exec` per channel** (`bridge.go:749-799`). After an `exec` request is handled the loop
returns and the deferred `channel.Close()` fires (`bridge.go:734`). There is no multiplexing of
several `exec` requests onto one channel, and no reuse of a channel after its command completes.
An `exec` request without `WantReply`, with an unmarshallable payload, with a denied or malformed
command, or whose accept reply fails, is recorded and then also ends the channel
(`bridge.go:764-794`).

**Exit status** (`bridge.go:796`): the sandbox exit code is sent back as an `exit-status` channel
request with `wantReply` false, then the channel closes. There is no `exit-signal` path.

**Concurrent channels.** Many channels are served concurrently on one connection, each in its own
`goroutine` (`bridge.go:731-736`), which is what makes ControlMaster multiplexing work. Pinned by
`bridge_test.go:380-417` with twelve parallel commands.

**Connection teardown.** A `goroutine` waits on `server.Wait()` and cancels the per-connection
context (`bridge.go:717-720`), so a dropped client aborts its own in-flight commands but not the
session.

**Absent:** no rate limiting on connections, channels, or requests; no retry of anything; no
back-pressure; no keepalive initiated by the server; no per-connection or per-session command
count limit.

## 6. Wire grammar

The grammar is defined in `grammar.go` and derives from Hermes's `tools/environments/ssh.py`,
which joins its `argv` with single spaces (`grammar.go:8-21`). Four payload shapes are ever
produced.

| Input shape | Kind | Outcome |
| --- | --- | --- |
| `echo 'SSH connection established'` | `bootstrap` | accepted, replayed literally (`grammar.go:69-70`) |
| `echo $HOME` | `bootstrap` | accepted, replayed literally (`grammar.go:71-72`) |
| `bash -c <canonically quoted script>` | `agent` | accepted, decoded (`grammar.go:78-98`) |
| `bash -l -c <canonically quoted script>` | `agent`, `login` | accepted, decoded (`grammar.go:78-98`) |
| prefix `mkdir -p ` | `sync` | denied by policy, `errSyncDenied` (`grammar.go:50`, `grammar.go:74-76`) |
| prefix `tar xf ` | `sync` | denied by policy |
| prefix `tar cf ` | `sync` | denied by policy |
| prefix `rm -f ` | `sync` | denied by policy |
| prefix `scp ` | `sync` | denied by policy |
| empty string, or any string containing `NUL` | `unknown` | rejected (`grammar.go:65-67`) |
| any other shell (`/bin/sh -c ...`) | `unknown` | rejected (`grammar.go:83`) |
| `bash` without `-c` | `unknown` | rejected (`grammar.go:83`) |
| `bash -c ''` (empty script) | `unknown` | rejected (`grammar.go:90-92`) |
| more than one token after `-c` | `unknown` | rejected (`grammar.go:138-140`) |
| unterminated quote | `unknown` | rejected (`grammar.go:135-137`) |
| double-quoted token | `unknown` | rejected by the canonical check (`grammar.go:143-145`) |
| redundantly quoted safe token, e.g. `bash -c 'ls'` | `unknown` | rejected by the canonical check |
| bare token containing unsafe characters, e.g. `bash -c ls\|whoami` | `unknown` | rejected (`grammar.go:112-114`) |
| the OpenClaw envelope `'env' 'HOME=/x' '/bin/sh' '-c' 'ls'` | `unknown` | rejected |

The classification matters beyond accept and reject: `sync` produces a `denied` record and
`unknown` produces a `rejected` record, so evidence separates ARIES policy from a protocol
violation (`bridge.go:778-783`, `grammar.go:30-38`).

Denial is refused *before* anything reaches the sandbox. The rationale is recorded at
`grammar.go:17-21`: the sync set is built from Hermes's `iter_sync_files`, which includes
credential files, and the remote is the exact container the verifier later inspects. Hermes catches
and rolls back every sync failure, so refusal is safe.

### Canonical quoting

`decodeShellToken` (`grammar.go:106-147`) reverses one `shlex.quote` token and then requires the
encoding to have been canonical:

```go
if shlexQuote(decoded) != encoded {
	return "", errors.New("SSH exec script token is not canonically quoted")
}
```

A bare token is legal only when it contains no character outside the safe set; a quoted token must
use the exact `'"'"'` escape for an embedded single quote and must terminate at the end of the
payload. The round-trip check means each accepted payload has exactly one reading, so no second
parse of the same bytes is possible.

`shlexQuote` (`grammar.go:152-160`) mirrors Python's `shlex.quote`, whose safe set is the ASCII
regular expression `[^\w@%+=:,./-]`; `shlexUnsafe` (`grammar.go:162-170`) enumerates that set as
ASCII letters, digits, and `_@%+=:,./-`. Parity matters for two reasons: it is what makes the
round-trip check exact, and it is what lets `encodeRemoteCommand` reproduce the original wire bytes
for hashing (`workspace.go:59-62`). A divergence would either reject legitimate Hermes payloads or
admit two encodings of one script.

Note the ASCII-only safe set: any non-ASCII rune is "unsafe" and therefore forces quoting, which
matches CPython's behaviour for the `str` regular expression only in the ASCII range. Scripts with
non-ASCII content still round-trip, because they are always quoted on both sides.

## 7. Command preparation

`prepareRemoteCommand` (`workspace.go:29-55`) turns a decoded `remoteCommand` into a
`preparedRemoteCommand` holding a `core.Command`, the canonical encoded payload, and the kind.

Hermes has no virtual workspace namespace; it addresses the sandbox with ordinary absolute paths
(`workspace.go:11-13`). The transform is therefore minimal, and exactly two things happen:

1. The bare `bash` token becomes the absolute `/bin/bash` (`workspace.go:20`, `workspace.go:48`),
   because the sandbox requires an absolute command path and performs no `PATH` lookup. This is
   why a Hermes task image must provide `/bin/bash`.
2. `Dir` is set to the sandbox workdir (`workspace.go:48`), unconditionally. The bridge is
   authoritative for the working directory regardless of what Hermes believes its `cwd` to be.

What becomes a `core.Command` (`pkg/core/types.go:35-45`):

| Kind | `Path` | `Args` | `Dir` |
| --- | --- | --- | --- |
| `bootstrap` | `/bin/sh` (`workspace.go:14`) | `["-c", <the literal probe payload>]` | sandbox workdir |
| `agent` | `/bin/bash` | `["-c", script]` or `["-l", "-c", script]` | sandbox workdir |

`Env`, `Stdin`, `Timeout`, `User`, and `OutputLimitBytes` are never set. No environment variable
from the wire reaches the sandbox; `env` requests are refused. Pinned by
`workspace_test.go:26-28`. `Stdin` stays empty because input is streamed, not buffered into the
command value.

**`validWorkdir`** (`workspace.go:66-85`) rejects anything that would change meaning as a process
working directory or in evidence:

- `/` is accepted as a special case.
- Otherwise: must start with `/`, must be at least two characters, must not end with `/`.
- No empty component (so no `//`), no `.`, no `..`.
- Each component may contain only ASCII letters, digits, `.`, `_`, and `-`.

Everything else, including spaces and quotes, is rejected and the request never runs
(`bridge.go:785-790`, pinned by `workspace_test.go:47-62`).

**Bootstrap probe replay** (`workspace.go:34-45`). The probe is not synthesized from a decoded
`argv`; the literal wire payload is replayed through `/bin/sh -c`, selected by inspecting
`remote.argv[1]`. The comment states the reason: `echo $HOME` must report the sandbox's own home
rather than a value ARIES invents. Pinned by `workspace_test.go:84-101` and
`bridge_test.go:261-294`.

**`encodeRemoteCommand`** (`workspace.go:59-62`):

```go
parts := append([]string(nil), remote.argv[:len(remote.argv)-1]...)
return strings.Join(parts, " ") + " " + shlexQuote(remote.script)
```

It reconstructs the wire payload from the decoded command so the recorded `command` field and the
`command_hash` are taken over a canonical, replayable form rather than over whatever bytes arrived.
Because decoding already required canonical quoting, the reconstruction is byte-identical to the
original payload — pinned against the two captured Hermes payloads at
`workspace_test.go:64-82`. For bootstrap probes `encoded` is the literal probe string
(`workspace.go:37-40`).

An unknown kind is a hard error (`workspace.go:52-54`), so a future grammar addition cannot reach
the sandbox unclassified.

## 8. Execution

`execute` (`bridge.go:807-853`).

**Stream wiring** (`bridge.go:809-812`):

```go
stdin := &recordedInput{reader: channel}
stdout := &byteCounter{writer: channel}
stderr := &byteCounter{writer: channel.Stderr()}
result, err := session.sandbox.ExecStream(ctx, prepared.command, stdin, stdout, stderr)
```

`stdin` is read directly from the channel and streamed into the sandbox; `stdout` and `stderr` are
written straight back to the channel and its extended-data stream. Output is never buffered by the
bridge and never lands in a file — only its size is observed. There is no output size limit.

**`byteCounter`** (`bridge.go:184-202`) wraps a reader or writer and accumulates a count in an
`atomic.Int64`, exposed by `count()`. It retains no content.

**`recordedInput`** (`bridge.go:204-247`) both forwards and retains `stdin`, guarded by its own
mutex. The overflow rule (`bridge.go:216-223`): if a read would push the retained buffer past
`maxRecordedInputBytes` (16 MiB, `bridge.go:34`), the byte count is still incremented, the retained
buffer is **discarded**, `overflow` is latched, and the `Read` returns an error to the sandbox —
which aborts the command. The bridge does not truncate and keep a prefix: partial input is not
retained at all, because a partial record would be misleading evidence.

`record` (`bridge.go:231-247`) returns the count, the text to place in the structured record, the
encoding label, the raw bytes for the raw log, and the overflow flag. `safeStructuredText`
(`bridge.go:249-259`) requires valid `UTF-8` with no control characters other than tab, newline,
and carriage return. Failing that, the structured record carries `binary-omitted` and a note whose
wording depends on whether the raw log is retained this run:

```go
note := fmt.Sprintf("[binary input omitted; %d bytes not retained]", count)
if retainedRaw {
	note = fmt.Sprintf("[binary input omitted; %d bytes retained in ssh_raw.log]", count)
}
```

The note must not point at an artifact this run did not write (`bridge.go:240-241`, pinned by
`bridge_test.go:571-593`).

**Post-cancellation error preservation** (`bridge.go:813-822`):

```go
if contextErr := ctx.Err(); contextErr != nil && !hasCancellationCause(err) {
	// A sandbox error returned after revocation is ambiguous unless it carries
	// the cancellation cause. Preserve both so Stop fails closed rather than
	// silently treating an unconfirmed tool termination as an earlier error.
	if err == nil {
		err = contextErr
	} else {
		err = errors.Join(contextErr, err)
	}
}
```

If the context is already done and the sandbox error does not itself carry a cancellation cause,
the context error is joined in. This guarantees that a command in flight at revocation always
produces an error carrying a cancellation cause, which is what makes it visible to
`recordRevocationError`.

**Classification** (`bridge.go:823-835`):

| Condition | `status` | `error` | `exit_code` |
| --- | --- | --- | --- |
| `err == nil` | `completed` | omitted | sandbox exit code, clamped |
| `err != nil`, not cancellation | `failed` | `sandbox execution failed` | `255` |
| `err != nil`, `errors.Is` cancellation or deadline | `canceled` | `session canceled` | `255` |

Exit codes outside `0..255` are clamped to `255` (`bridge.go:833-835`), which also normalizes the
`-1` a sandbox returns on a failed start. The clamped value is both recorded and sent back as the
channel `exit-status` (`bridge.go:796`). The recorded error message is a fixed string; the
underlying sandbox error text is never written to the artifacts.

**Revocation ledger.** Every non-`nil` error goes to `recordRevocationError`
(`bridge.go:827`), which keeps it only if it carries a cancellation cause
(`bridge.go:953-960`).

**Overflow short-circuit** (`bridge.go:836-840`): on `stdin` overflow the audit is latched with
`"retain Hermes SSH stdin: input exceeds ..."` and `execute` returns **without writing a record**.
The call is therefore absent from the tool log and `Stop` will fail. This is deliberate: an
unrecordable call blocks revocation rather than being silently logged with missing input.

## 9. Evidence and audit

### `toolCallRecord`

`bridge.go:107-131`. One `jsonl` line per channel request, written to `tool-calls.jsonl`.

| Field | `json` name | Meaning and source |
| --- | --- | --- |
| `Sequence` | `sequence` | monotonic, assigned in `enqueue` (`bridge.go:295`, `bridge.go:297`) |
| `Timestamp` | `timestamp` | `RFC3339Nano` in `UTC`, assigned in `enqueue` (`bridge.go:296`) |
| `ContainerID` | `container_id` | `sandbox.ContainerID()` |
| `ContainerName` | `container_name` | `sandbox.ContainerName()` |
| `OperationClass` | `operation_class` | `agent`, `bootstrap`, `sync`, or `unknown` (`grammar.go:33-38`) |
| `Path` | `path` | `core.Command.Path`; omitted for requests that never ran |
| `Workdir` | `workdir` | `core.Command.Dir`; omitted for requests that never ran |
| `CommandHash` | `command_hash` | `sha256` hex of the canonical payload (`bridge.go:1031-1034`) |
| `Command` | `command` | the canonical wire payload; omitted when empty |
| `Argv` | `argv` | `[Path]` followed by `Args` (`bridge.go:846`); omitted when empty |
| `Stdin` | `stdin` | retained input text, or the binary-omission note |
| `StdinEncoding` | `stdin_encoding` | `utf-8` or `binary-omitted` |
| `StdinBytes` | `stdin_bytes` | total bytes read from the channel, including discarded overflow |
| `StdoutBytes` | `stdout_bytes` | counted, not retained |
| `StderrBytes` | `stderr_bytes` | counted, not retained |
| `ExitCode` | `exit_code` | `0..255`, or `-1` for a request that never ran (`bridge.go:868-870`) |
| `DurationMS` | `duration_ms` | wall time around `ExecStream` (`bridge.go:808`, `bridge.go:849`) |
| `Status` | `status` | `completed`, `failed`, `canceled`, `rejected`, `denied`, `unsupported` |
| `Error` | `error` | fixed classification message; omitted when empty |
| `RunID` | `run_id` | set in `writeRecord` (`bridge.go:885`) |
| `TaskID` | `task_id` | set in `writeRecord` (`bridge.go:886`) |
| `RequestType` | `request_type` | the raw channel request type, `exec` or otherwise |
| `WantReply` | `want_reply` | the request's reply flag |

There is no `stdout` or `stderr` content field, by design; the integration test asserts the strings
`"stdout":` and `"stderr":` never appear in the file (`integration_test.go:445-454`).

### `rawSSHRecord`

`bridge.go:133-147`. One block per request in `ssh_raw.log`, rendered by `renderRawSSHRecord`
(`bridge.go:333-351`) between `--- ARIES SSH CALL BEGIN ---` and `--- ARIES SSH CALL END ---`
markers, as `key=value` lines in this order: `sequence`, `timestamp`, `request_type`, `want_reply`,
`status`, `run_id`, `task_id`, `container_id`, `wire_command`, `payload_bytes`, `payload`,
`stdin_bytes`, `stdin`.

| Field | Meaning |
| --- | --- |
| `Sequence`, `Timestamp` | identical values to the structured record with the same sequence; this is the correlation key |
| `RequestType`, `WantReply` | as received |
| `Status` | the same status string as the structured record |
| `RunID`, `TaskID`, `ContainerID` | set in `writeRecord` (`bridge.go:887-889`) |
| `WireCommand` | the verbatim `exec` payload string, empty for non-`exec` requests |
| `Payload`, `PayloadBytes` | the raw request payload bytes as received, cloned (`bridge.go:751`, `bridge.go:879`) |
| `Stdin`, `StdinBytes` | the retained raw input bytes, including bytes the structured record omitted as binary |

Values are escaped by `writeEscapedRaw` (`bridge.go:364-397`): `\\`, `\n`, `\r`, `\t` as two-character
escapes, printable runes verbatim, everything else as uppercase `\xHH` per byte
(`bridge.go:399-406`). The record is line-oriented and lossless with respect to the original bytes.

The raw log holds no command output either (`integration_test.go:468-472`).

### The two files and `OmitRawLog`

- `tool-calls.jsonl` is always written (`bridge.go:599`, `bridge.go:622`).
- `ssh_raw.log` is written only when `Options.OmitRawLog` is false (`bridge.go:600-602`,
  `bridge.go:626-632`).
- `Options.OmitRawLog` documents the trade-off at `bridge.go:47-53`: the raw log is the only
  artifact holding the raw wire command, the request payload, and the binary `stdin` the structured
  log omits. It is a forensic record, not a duplicate.
- The zero value of `Options` retains the raw log, but the profile default does not: the wiring
  passes `OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()` (`cmd/aries/wiring.go:333`) and
  `retain_raw_log` defaults to false (`pkg/config/config.go:230-241`). Production runs drop the raw
  log unless the profile opts in.
- `logPaths` advertises only the files that exist (`bridge.go:656-663`), so the endpoint never
  names a missing artifact. Pinned by `bridge_test.go:540-544`.

Both files are opened `O_EXCL` at `0600` (`bridge.go:261-267`) and retained at `0600`
(`integration_test.go:401-406`).

### `auditWriter` design

`bridge.go:167-182`. One writer `goroutine` per session, started by `newAuditWriter`
(`bridge.go:269-277`) and terminated only by sealing.

- **Producers** call `enqueue` (`bridge.go:285-321`) under `writer.mu`. Producers never touch the
  files, so channel handlers never block on disk `I/O` beyond appending to a slice.
- **Ordering.** `sequence` and `timestamp` are assigned under the same lock that appends to
  `pending`, and the writer drains `pending` in `FIFO` order (`bridge.go:429-445`). File order
  therefore equals sequence order, and the structured and raw records for one request share a
  sequence and a `timestamp`.
- **Durability.** Each entry is written to both files and then `sync`-ed on both
  (`bridge.go:449-454`). A short write is treated as an error (`bridge.go:461-463`).
- **Wake-up.** A buffered channel of size one with a non-blocking send (`bridge.go:408-413`).
- **Sealing.** `sealAndWait` (`bridge.go:500-516`) sets `sealed`, wakes the writer, and waits for
  `done` or the caller's context. The writer drains everything pending, then calls `finish`
  (`bridge.go:482-498`), which performs a final `sync` on both files and closes them, latching any
  error. `enqueue` after seal is itself a latched error (`bridge.go:291-294`).
- **Nil-safety.** `retainsRaw`, `sealAndWait`, and `finished` all tolerate a `nil` writer
  (`bridge.go:281-283`, `bridge.go:501-503`, `bridge.go:519-521`), which is the partial-start case.

### Byte budget

`maxToolLogBytes` is 256 MiB (`bridge.go:35`) and is charged against the **combined** length of the
structured line plus the raw line (`bridge.go:312-316`):

```go
charge := int64(len(structuredLine) + len(rawLine))
if charge > maxToolLogBytes-writer.bytes {
	writer.latchLocked(fmt.Errorf("Hermes SSH combined audit exceeds %d bytes", maxToolLogBytes))
	return
}
```

Exceeding the budget drops the record and latches the error. There is no rotation, truncation, or
compression.

### Latching, and which failures block revocation

`latchLocked` joins errors into `writer.err` (`bridge.go:415-417`); once `err` is non-`nil`,
`enqueue` returns immediately and records nothing more (`bridge.go:288-290`). The latched error is
returned by `sealAndWait` and flows through `closeAudit` into `finalize`, so **every latched audit
failure fails `Stop` and blocks evaluation**. The complete latching set:

| Failure | Location |
| --- | --- |
| `enqueue` after seal | `bridge.go:291-294` |
| `json` marshal of a structured record | `bridge.go:299-303` |
| render of a raw record | `bridge.go:304-311` |
| combined byte budget exceeded | `bridge.go:312-316` |
| write error or short write on either file | `bridge.go:456-469` |
| `sync` error on either file, per entry | `bridge.go:471-480` |
| final `sync` on either file | `bridge.go:483-484` |
| close error on either file | `bridge.go:485-497` |
| `stdin` retention overflow | `bridge.go:836-839` |

Additionally, a drain that does not finish within the `Stop` context returns
`"drain Hermes SSH audit: ..."` (`bridge.go:513-515`) and leaves `finished()` false, which stops
`finalize` before the identity is removed (`bridge.go:937-939`).

### `commandHash`

`bridge.go:1031-1034`: `sha256` of the command string, hex encoded. For executed commands the input
is `prepared.encoded`, the canonical wire payload (`bridge.go:844`). For requests that never ran it
is `audit.remoteCommand` (`bridge.go:866`), which is the empty string for non-`exec` requests and
for `exec` requests whose payload failed to unmarshal — so those records all carry the hash of the
empty string. The hash is a correlation and integrity aid over a replayable form, not a redaction:
the plaintext command is recorded alongside it.

### Requests that never ran

`logRequestFailure` (`bridge.go:863-874`) writes a record with the kind established so far, no
`Path`, no `Workdir`, no `Command`, no `Argv`, `stdin_encoding: utf-8`, zero byte counts, and
`exit_code: -1`. The comment is explicit that a refused sync must not borrow a successful command's
exit code, and the classification is pinned by `integration_test.go:412-435`.

`logRejected` (`bridge.go:855-857`) is the `rejected` / `"invalid remote command"` specialization.

## 10. Limits, bounds, and validation constants

| Constant or bound | Value | Location | Protects |
| --- | --- | --- | --- |
| `defaultBridgeCleanup` | 20s | `bridge.go:33` | bounds the internal rollback in `fail`; used only when `Options.CleanupTimeout <= 0` (`bridge.go:543-545`) |
| `maxRecordedInputBytes` | 16 MiB (`16 << 20`) | `bridge.go:34` | bounds host memory held for `stdin` evidence per call; exceeding it aborts the call and latches |
| `maxToolLogBytes` | 256 MiB (`256 << 20`) | `bridge.go:35` | bounds combined audit disk usage per session |
| `identityContainerPath` | `/run/aries/ssh/id_ed25519` | `bridge.go:37` | the fixed in-container path the harness mounts the identity at |
| `lockedUsername` | `aries` | `bridge.go:38` | authentication: no other username may authenticate |
| `lockedConnectTimeout` | 5s | `bridge.go:39` | bounds an unauthenticated connection's handshake; cleared afterwards (`bridge.go:714`) |
| `MaxAuthTries` | 3 | `bridge.go:667` | bounds authentication attempts per connection |
| artifact directory mode | `0700` | `bridge.go:1134`, `bridge.go:1148` | private evidence and credential directory |
| artifact file mode | `0600` | `bridge.go:262`, `bridge.go:1069`, `bridge.go:1094` | private key, known-hosts, and audit files |
| exclusive creation | `O_CREATE\|O_EXCL` | `bridge.go:262`, `bridge.go:1094` | never overwrite or adopt a pre-existing artifact |
| symbolic-link rejection | `EvalSymlinks != Abs` | `bridge.go:1137-1147` | the artifact directory may not be reached through a link |
| exit-code clamp | `0..255` | `bridge.go:833-835` | a sandbox exit code cannot escape the protocol range |
| channel type | `session` with empty `ExtraData` | `bridge.go:723` | no forwarding or unexpected channel types |
| global request | `keepalive@openssh.com`, empty payload | `bridge.go:742` | no `tcpip-forward` or other global capability |
| `exec` per channel | exactly one | `bridge.go:795-797` | one command per channel, no reuse |
| `validWorkdir` character set | `[A-Za-z0-9._-]` per component | `workspace.go:66-85` | the working directory cannot change meaning in a shell or in evidence |
| `NUL` and empty payload | rejected | `grammar.go:65-67` | no truncation ambiguity in the wire command |
| canonical quoting | `shlexQuote(decoded) == encoded` | `grammar.go:143-145` | exactly one reading per accepted payload |
| listener network | `tcp4`, port `0` | `bridge.go:606` | `IPv4` on the task gateway, ephemeral port, never a fixed well-known port |

Absent bounds, stated explicitly: no limit on the number of connections, channels, or commands; no
limit on `stdout` or `stderr` bytes; no per-command timeout set by the bridge (`core.Command.Timeout`
is left zero); no idle timeout after the handshake; no rate limit anywhere.

## 11. Privacy and redaction guarantees

Must never appear in the artifacts:

- **Private key bytes.** The client private key exists only in `id_ed25519`, which is removed at
  revocation (`bridge.go:945`). The host private key is never written to disk at all
  (`bridge.go:1037-1043`). Neither is logged, and `core.ToolEndpoint` carries paths only
  (`pkg/core/types.go:90-105`).
- **Command output.** No `stdout` or `stderr` content is recorded in either file — only byte counts
  (`bridge.go:121-122`). Enforced by test at `integration_test.go:445-454` and
  `integration_test.go:468-472`. Output belongs to the sandbox transcript.
- **Underlying sandbox error text.** The `error` field is one of three fixed strings
  (`bridge.go:828-831`, `bridge.go:856`, `bridge.go:761`), so a sandbox error carrying environment
  detail is not copied into the record.
- **Model credentials.** The bridge never sees them; nothing in `Options` or the session carries a
  model configuration.

Deliberately does appear:

- The full script Hermes sent, verbatim, in `command` and `argv` (`bridge.go:844-846`). There is no
  redaction pass over script content.
- The raw request payload and the raw `stdin` bytes, in `ssh_raw.log` when retained
  (`bridge.go:879-880`).
- The container identity, run identity, and task identity on every record
  (`bridge.go:884-890`).
- The host public key, in `known_hosts`, retained after revocation as evidence of the key Hermes
  pinned (`bridge.go:940-942`).
- The bind address, network name, and container name in one structured start log line
  (`bridge.go:648`).

**Tool inputs are private task data.** `stdin` and the command text are agent-supplied and
benchmark-derived; the artifacts are written `0600` inside a `0700` directory precisely because
they may contain task content, credentials the agent happened to echo, or wire-supplied data. They
are private run artifacts, not shareable logs — the same rule stated for the run directory in
`CLAUDE.md` and `docs/design/bridge.md`.

## 12. Concurrency and locking

| Primitive | Location | Protects |
| --- | --- | --- |
| `Manager.mu` (`sync.Mutex`) | `bridge.go:66` | `active`, `stopping`, `stopDone`, `stopErr`; serializes the whole of `Start` against `Stop` |
| `Manager.stopDone` (channel) | `bridge.go:69` | broadcast to concurrent `Stop` callers; closed once per `Stop` (`bridge.go:930`) |
| `bridgeSession.mu` (`sync.Mutex`) | `bridge.go:99` | the `connections` set: insert on accept (`bridge.go:689-691`), delete on close (`bridge.go:701-703`), iterate on revoke (`bridge.go:1005-1009`) |
| `bridgeSession.revocationMu` (`sync.Mutex`) | `bridge.go:101` | `revocationErr`, written from any channel `goroutine` (`bridge.go:957-959`) and read in `finalize` (`bridge.go:962-969`) |
| `bridgeSession.wait` (`sync.WaitGroup`) | `bridge.go:103` | counts the serve loop (`bridge.go:637`), each connection handler (`bridge.go:692`), and each channel handler (`bridge.go:731`); `waitFor` is the drain gate before finalization |
| `bridgeSession.revokeOnce` (`sync.Once`) | `bridge.go:104` | one cancel-and-close pass, so a retried `Stop` does not close closed things twice |
| `auditWriter.mu` (`sync.Mutex`) | `bridge.go:171` | `pending`, `sequence`, `bytes`, `sealed`, `err`; assigns sequence and `timestamp` atomically with the append |
| `auditWriter.wake` (buffered channel, size 1) | `bridge.go:177` | non-blocking producer wake-up (`bridge.go:408-413`) |
| `auditWriter.done` (channel) | `bridge.go:178` | writer termination signal, read by `sealAndWait` and `finished` |
| `byteCounter.n` (`atomic.Int64`) | `bridge.go:187` | byte counts read from another `goroutine` after `ExecStream` returns |
| `recordedInput.mu` (`sync.Mutex`) | `bridge.go:206` | `n`, `data`, `overflow`; the reader runs on the sandbox's `goroutine`, `record` on the handler's |
| per-connection `context.CancelFunc` | `bridge.go:715` | aborts that connection's in-flight commands when the client goes away (`bridge.go:717-720`) |
| session `cancel` | `bridge.go:635`, `bridge.go:999` | aborts every in-flight command at revocation |

Lock ordering is shallow: no function holds two of these at once, except that `enqueue` holds
`auditWriter.mu` while calling the injected `marshal` and `renderRaw` closures.

## 13. Preservation checklist for an `RPC` migration

| Behavior | Where it lives now | SSH-specific? | Must the replacement provide it? |
| --- | --- | --- | --- |
| Positive revocation: `nil` `Stop` means access is gone | `bridge.go:893-951` | no | **Yes.** This is the role contract (`pkg/runner/interfaces.go:51-56`) and the gate on evaluation. |
| `Stop` `idempotent`, safe after a failed `Start` | `bridge.go:893-912`, `bridge.go:574-589` | no | **Yes.** The Runner always calls `Stop` (`pkg/runner/runner.go:218-221`). |
| Concurrent `Stop` callers share one result | `bridge.go:900-912` | no | Yes. |
| Failed `Stop` leaves the session retryable | `bridge.go:924-931` | no | Yes. |
| In-flight work aborted, not awaited, at revocation | `bridge.go:997-1011`, `bridge.go:812` | no | **Yes.** |
| Ambiguous post-revocation errors fail closed | `bridge.go:813-822`, `bridge.go:953-995` | no | **Yes.** The distinction between "died because revoked" and "cannot prove it stopped" must survive. |
| Expected cancellation does not fail `Stop` | `bridge.go:962-969`, `bridge.go:975-995` | no | Yes, or revocation becomes unusable. |
| One active session per `Manager` | `bridge.go:558-560` | no | Yes. |
| Bounded, detached cleanup context | `bridge.go:577-578` | no | Yes. |
| Partial start leaves no artifact directory | `bridge.go:947-949` | no | Yes. |
| Credential material removed at revocation | `bridge.go:943-946` | no | **Yes**, in whatever form the replacement's credential takes. |
| Credential never in the endpoint value, profile, logs, or results | `bridge.go:649-653`, `pkg/core/types.go:90-93` | no | Yes. |
| Credential file written `O_EXCL` `0600`, `sync`-ed, in a `0700` non-symlink directory | `bridge.go:1068-1149` | no | Yes. |
| Per-session ephemeral credentials, no reuse across tasks | `bridge.go:1036-1058` | partly (`Ed25519` keys are SSH-shaped) | Yes as a property; the key type is an artifact. |
| Sandbox capability assertion before any resource is created | `bridge.go:561-564`, `bridge.go:73-83` | no | Yes. The method set may shrink if the replacement needs less. |
| Workdir authority: the bridge, not the client, sets the working directory | `workspace.go:48` | no | **Yes.** |
| `validWorkdir` character and structure rules | `workspace.go:66-85` | no | Yes. |
| Absolute command path, no `PATH` lookup | `workspace.go:20`, `workspace.go:48` | no | Yes, while the sandbox contract is unchanged. |
| No environment variables from the client reach the sandbox | `bridge.go:752-763`, `workspace.go:46-51` | the `env` request is SSH-specific; the policy is not | **Yes.** |
| Exactly two command shapes reach the sandbox: `/bin/bash` and the bootstrap `/bin/sh` probe | `workspace.go:29-55`, `integration_test.go:232-250` | no | **Yes.** This is the leak check. |
| File-sync denial by policy, before the sandbox | `grammar.go:47-76` | the payload prefixes are SSH-shaped; the policy is not | **Yes.** The replacement must have an equivalent "no file transfer into the evaluated container" rule. |
| Denial distinguished from protocol rejection in evidence | `bridge.go:778-783`, `grammar.go:33-38` | no | Yes. |
| Bootstrap probes answered from the sandbox, not synthesized | `workspace.go:34-45` | probe payloads are Hermes-over-SSH artifacts | Only if the replacement keeps a bootstrap handshake. |
| Canonical `shlex` quoting and the round-trip check | `grammar.go:106-160` | **yes** — a pure transport artifact | No. A typed `RPC` schema carries `argv` as a list; the single-reading property it buys must be preserved structurally instead. |
| `shlexQuote` parity with Python | `grammar.go:149-170` | **yes** | No, once quoting is gone. |
| `NUL` and empty-payload rejection | `grammar.go:65-67` | partly | Yes as input validation on whatever carries the command. |
| Wire payload reproducible for hashing | `workspace.go:59-62` | quoting is an artifact; a canonical replayable form is not | **Yes.** The recorded command must be replayable and hashable. |
| SSH channel semantics: session-only channels, one `exec` per channel, refused-but-open non-`exec` requests, `exit-status` | `bridge.go:722-799` | **yes** | No. But the *reason* the channel stays open — a refused sub-request must not break the client — generalizes to "an unsupported call must be refused without tearing down the transport". |
| Host key, known-hosts evidence, first-use pinning note | `bridge.go:615-621`, `bridge.go:1037-1043` | **yes** | No, unless the replacement has server identity to pin; if it does, retain the same evidence-not-guarantee framing. |
| Public-key-only authentication, locked username, exact key comparison, `MaxAuthTries` | `bridge.go:665-677` | mechanism is SSH-specific | **Yes** as a property: exactly one credential, exactly one identity, bounded attempts. |
| Handshake deadline that does not survive into the session | `bridge.go:705`, `bridge.go:714` | **yes**, tied to ControlMaster | Only if the replacement has a long-lived connection. |
| Concurrent calls on one connection | `bridge.go:731-736` | transport-shaped | Yes if the client multiplexes. |
| Streamed `stdin`, `stdout`, `stderr`; no buffering of output | `bridge.go:809-812` | no | **Yes.** Output must not be buffered to a file or into the container filesystem. |
| Exit-code clamp to `0..255` | `bridge.go:833-835` | partly | Yes, or define the replacement's own exit-code domain explicitly. |
| Bounded retained input, no partial retention | `bridge.go:212-229` | no | **Yes.** |
| Unrecordable call blocks revocation rather than logging a lie | `bridge.go:836-840` | no | **Yes.** |
| Evidence ordering: monotonic sequence, shared `timestamp`, `FIFO` file order | `bridge.go:285-321`, `bridge.go:425-447` | no | **Yes.** |
| Structured and raw records correlated by sequence | `bridge.go:297-298`, `integration_test.go:473-475` | no | Yes, if a second forensic stream is kept. |
| Every request recorded, including refused ones | `bridge.go:752-763`, `bridge_test.go:240-249` | no | **Yes.** Audit completeness is the property, not the `env` request. |
| Never-ran records carry `exit_code: -1` | `bridge.go:868-870` | no | Yes. |
| No command output in the audit | `bridge.go:107-131`, `integration_test.go:445-454` | no | **Yes.** |
| Fixed error strings, no sandbox error text in artifacts | `bridge.go:828-831` | no | Yes. |
| Binary input omitted from the structured record with an honest note | `bridge.go:231-259` | no | Yes. |
| Combined audit byte budget with latching | `bridge.go:312-316` | no | Yes. |
| All audit failures latch and block revocation | `bridge.go:415-423`, `bridge.go:937-939` | no | **Yes.** |
| Asynchronous single-writer audit with per-entry `sync` | `bridge.go:167-182`, `bridge.go:425-480` | no | Property yes (durable, ordered, non-blocking); the design is an implementation choice. |
| Artifacts opened `O_EXCL` at `0600` and retained at `0600` | `bridge.go:261-267` | no | Yes. |
| `LogPaths` names only files that exist | `bridge.go:656-663` | no | Yes. |
| `OmitRawLog` opt-out drops only the forensic stream | `bridge.go:47-53`, `bridge.go:600-602` | the file is SSH-named | Keep the option shape; rename the artifact. |
| No client helper staged or advertised | `bridge.go:649-653` | Hermes-specific | Depends on whether the replacement client needs staging. |
| No rate limiting, retry, or back-pressure | absent throughout | no | Not required, but the absence is deliberate and should be a conscious decision, not an oversight. |
| Sandbox usable for evaluation after revocation | `integration_test.go:391-395` | no | **Yes.** Revocation must not disturb the container. |

## 14. Test inventory

Four test files cover the package: three build without tags and run under `make test`; the fourth
carries `//go:build integration` (`integration_test.go:1`). Each file's tables below carry a
**Scenario** column describing the situation the test constructs: who the client is, what precedes
the action, what is sent, and what is observed. Line numbers are those of the `func Test...`
declaration. The four levels used in the coverage map at the end are:

| Level | Client | Sandbox | Listener |
| --- | --- | --- | --- |
| unit | none | none | none — a pure function or value is called directly |
| fake-client | a Go `golang.org/x/crypto/ssh` client | in-memory `testSandbox` (`bridge_test.go:23`) | a real bridge listener on `127.0.0.1` |
| integration-docker | a Go `ssh` client with a pinned host key | a real `pkg/sandbox/docker` container | a real bridge listener |
| integration-upstream | the unmodified upstream Hermes image running its own OpenSSH | `integrationSandbox` (`integration_test.go:90`), which executes on the host | a real bridge listener |

### `pkg/bridge/hermesssh/grammar_test.go`

#### Fixtures and helpers

| Helper | Where | What it provides | What it deliberately does not do |
| --- | --- | --- | --- |
| `capturedLoginPayload` | `grammar_test.go:15` | the verbatim `bash -l -c '...'` payload Hermes v2026.5.29.2 (`tools/environments/ssh.py`) sent to a logging SSH server for its session snapshot: eight newline-separated lines including `export -p`, `declare -f \| grep -vE '^_[^_]'`, `shopt -s expand_aliases`, and the `__HERMES_CWD_...__` marker, with every embedded quote in the `'"'"'` form (`grammar_test.go:9-13`) | nothing is synthesized; it is the real wire shape, so it also pins the incompatibility with OpenClaw's single-token grammar |
| `capturedAgentPayload` | `grammar_test.go:16` | the verbatim `bash -c '...'` payload for an agent command: sources the snapshot, `builtin cd -- /tmp`, `eval 'echo hello-from-agent && pwd'`, re-snapshots, prints the marker, `exit $__hermes_ec` | same |

No fake stands in for anything here; every test calls `decodeRemoteCommand` (`grammar.go:64`) or
`shlexQuote` (`grammar.go:152`) directly.

| Test | Line | Scenario | Invariant pinned | Port or retire |
| --- | --- | --- | --- | --- |
| `TestDecodeAcceptsCapturedHermesPayloads` | 19 | `decodeRemoteCommand` is called on `capturedLoginPayload`, then on `capturedAgentPayload`. For the login payload the test reads back `login`, `kind`, and searches the decoded `script` for `shopt -s expand_aliases`, a literal newline, and the unescaped `grep -vE '^_[^_]'` (`grammar_test.go:24-32`); for the agent payload it checks `login` is false and the script holds the unescaped `eval 'echo hello-from-agent && pwd'` (`grammar_test.go:38-43`). | the two verbatim payloads captured from Hermes v2026.5.29.2 decode to class `agent`; `login` is set only for the `-l` form; embedded newlines survive and every `'"'"'` escape collapses to one `'` ([section 6](#6-wire-grammar)) | Retire as-is; replace with captured payloads of the new schema. The principle — pin against real recorded client output, not synthesized input — must be ported. |
| `TestDecodeAcceptsBootstrapProbes` | 46 | `decodeRemoteCommand` is called on `connectionProbePayload` (`echo 'SSH connection established'`) and `remoteHomePayload` (`echo $HOME`), the two constants at `grammar.go:26-27`; only the returned `kind` is inspected. | both probes decode without error and classify as `bootstrap` (`grammar.go:68-73`) | Port if a bootstrap handshake survives. |
| `TestDecodeDeniesCapturedFileSyncPayloads` | 61 | six payloads captured from Hermes's `~/.hermes` sync are decoded in turn: two `mkdir -p` forms (one creating `skills`, `credentials`, and `cache` at once), `tar xf - --no-overwrite-dir -C ...`, `tar cf - -C / home/colin/.hermes`, `rm -f .../skill.md`, and `scp -t .../skill.md` (`grammar_test.go:62-69`); each error is tested with `errors.Is` against `errSyncDenied`. | every one of the five `syncPayloadPrefixes` (`grammar.go:50`) is hit by at least one captured payload and returns exactly `errSyncDenied` (`grammar.go:53`, `grammar.go:74-76`), the sentinel the bridge later maps to status `denied` rather than `rejected` ([section 9](#requests-that-never-ran)) | **Port.** The denial policy and its distinct error must survive. |
| `TestDecodeRejectsEverythingElse` | 78 | eleven named payloads are decoded and each must return a non-`nil` error; which error is not checked (`grammar_test.go:92-96`). The cases are: empty string; `bash -c 'ls\x00'` (`NUL`); `/bin/sh -c 'ls'` (other shell); `'env' 'HOME=/x' '/bin/sh' '-c' 'ls'` (the OpenClaw envelope); `bash 'ls'` (no `-c`); `bash -c ''` (empty script); `bash -c 'ls' extra` (trailing token); `bash -c 'ls` (unterminated); `bash -c "ls"` (double quotes); `bash -c ls\|whoami` (bare token that `shlex.quote` would have quoted); `bash -l 'ls'` (login without `-c`). | the rejection categories are: empty or `NUL` (`grammar.go:65-67`, two cases); wrong prefix — other shell, OpenClaw envelope, missing `-c`, `-l` without `-c` (`grammar.go:78-85`, four cases); empty decoded script (`grammar.go:90-92`, one case); unterminated quote (`grammar.go:135-137`, one case); more than one token (`grammar.go:138-140`, one case); non-canonical quoting — double quotes fail the bare-token character check (`grammar.go:112-114`), and `ls\|whoami` fails the round-trip (`grammar.go:143-145`) (two cases) | Port the concept; most individual cases are quoting artifacts. The empty, `NUL`, envelope-shape, and wrong-executable cases generalize to input validation on the replacement schema. |
| `TestDecodeRequiresCanonicalQuoting` | 101 | `bash -c ls` is decoded and its `script` must equal `ls`; `bash -c 'ls'` is decoded and must be refused (`grammar_test.go:102-107`). | a bare token is accepted only where `shlex.quote` would itself have left it bare; the redundantly quoted form of a safe token fails the `shlexQuote(decoded) != encoded` check (`grammar.go:143-145`), so each payload has exactly one reading ([canonical quoting](#canonical-quoting)) | Retire; the single-reading property becomes structural in a typed schema. |
| `TestShlexQuoteMatchesPython` | 110 | `shlexQuote` is called on seven inputs — the empty string, `ls`, the all-safe-characters token `a/b_c-d.e:f,g@h%i`, `ls -l`, `it's`, a string with an embedded newline, and `$HOME` — and compared with the output Python's `shlex.quote` produces for each (`grammar_test.go:111-119`). | `shlexQuote` (`grammar.go:152-170`) reproduces Python's safe set `[\w@%+=:,./-]`, returns `''` for empty input, and escapes an embedded quote as `'"'"'` | Retire with the quoting. |
| `TestDecodeRoundTripsArbitraryScripts` | 127 | four scripts — `echo hello`, `grep -vE '^_[^_]' file`, a two-line `printf 'a\nb'` followed by `exit $?`, and `echo "double" && echo 'single'` — are each encoded with `shlexQuote` behind both `bash -c ` and `bash -l -c `, giving eight payloads; each is decoded and the recovered `script` compared byte-for-byte (`grammar_test.go:128-145`). | encode-then-decode is lossless for scripts containing single quotes, double quotes, newlines, `$`, and `&&`, under both the plain and login prefixes | Port as a schema round-trip test. |

### `pkg/bridge/hermesssh/workspace_test.go`

#### Fixtures and helpers

No helpers are defined. Every test builds its input through `decodeRemoteCommand` and calls
`prepareRemoteCommand` (`workspace.go:29`) directly, except `TestPrepareRejectsUnknownKind`, which
constructs a `remoteCommand` literal (`workspace_test.go:104`). The constants under test are
`remoteShellPath` (`/bin/bash`, `workspace.go:20`) and `bootstrapShell` (`/bin/sh`,
`workspace.go:14`).

| Test | Line | Scenario | Invariant pinned | Port or retire |
| --- | --- | --- | --- | --- |
| `TestPrepareMapsAgentCommandsToTheSandboxWorkdir` | 8 | `bash -c 'echo hi'` is decoded and prepared for workdir `/testbed`; the resulting `core.Command` is inspected field by field (`workspace_test.go:17-30`). | `Path` is `/bin/bash`, `Dir` is the caller-supplied workdir, `Args` is exactly `["-c", "echo hi"]`, `kind` is `agent`, and `Env` is `nil` — no environment ever originates from the client ([section 7](#7-command-preparation), `workspace.go:46-51`) | **Port.** Workdir authority and the no-environment rule. |
| `TestPreparePreservesLoginShell` | 33 | `bash -l -c 'export -p'` is decoded and prepared for `/testbed`; only `Args` is inspected. | `Args` has three entries with `-l` ahead of `-c` (`workspace_test.go:42`), so the login-shell snapshot Hermes runs at connection time keeps its semantics | Port if the login shell shape survives. |
| `TestPrepareRejectsUnsafeWorkdir` | 47 | one decoded agent command is prepared against eight workdir strings — empty, `relative/path`, `/has space`, `/quote'd`, `/trailing/`, `/a//b`, `/a/../b`, `/a/./b` — each of which must be refused, and then against `/`, which must be accepted (`workspace_test.go:52-59`). | `validWorkdir` (`workspace.go:66-85`) rejects relative paths, whitespace, quotes, trailing slashes, empty components, and `.`/`..` components, and accepts the root as the one single-character path | **Port** unchanged; `validWorkdir` is transport-independent. |
| `TestEncodeRoundTripsToTheOriginalPayload` | 64 | three payloads — `capturedAgentPayload`, `capturedLoginPayload`, and `bash -c 'echo hi'` — are decoded, prepared for `/testbed`, and the `encoded` field of each result compared with the original wire string (`workspace_test.go:65-81`). | `encodeRemoteCommand` (`workspace.go:59-62`) reproduces the exact bytes Hermes sent, so `commandHash` (`bridge.go:1031-1034`) is taken over a replayable canonical form ([section 9](#commandhash)) | **Port** as "the recorded command is replayable". |
| `TestPrepareBootstrapProbesReplayLiterally` | 84 | both probe payloads are decoded and prepared for `/testbed`; `Path`, `Args[1]`, `kind`, and `encoded` are inspected (`workspace_test.go:94-99`). | probes run as `/bin/sh -c <literal payload>` in the workdir (`workspace.go:34-45`), class `bootstrap`, and the recorded command is the payload itself — `echo $HOME` is answered by the sandbox's shell, never by a value ARIES invents | Port if probes survive. |
| `TestPrepareRejectsUnknownKind` | 103 | a hand-built `remoteCommand{argv: ["bash"], kind: "other"}` is prepared for `/testbed`; the error must be non-`nil` and contain `unsupported` (`workspace_test.go:104-106`). | the `default` arm of the kind switch (`workspace.go:52-54`) refuses anything the grammar did not classify, so no unclassified command can reach the sandbox | **Port.** |

### `pkg/bridge/hermesssh/bridge_test.go`

#### Fixtures and helpers

| Helper | Where | What it fakes or reproduces | What it deliberately does not fake | Production behavior it stands in for |
| --- | --- | --- | --- | --- |
| `testSandbox` | `bridge_test.go:23-77` | the whole `bridgeSandbox` method set (`bridge.go:73-83`) over an in-memory recorder: `Exec` (`bridge_test.go:31-38`) clones `Args` and `Env`, appends the command under `mu`, and always returns the preconfigured `result` with a `nil` error. `ExecStream` (`bridge_test.go:40-61`) first drains all of `stdin` with `io.ReadAll` — this read is not cancellable and completes only when the client sends `EOF`; then, if `block` is non-`nil`, it waits on `block` or `ctx.Done()` and on cancellation returns `ExitCode: -1` with `ctx.Err()` (`bridge_test.go:45-51`); otherwise it appends the drained bytes to `stdins`, delegates to `Exec`, and copies `result.Stdout`/`result.Stderr` to the writers (`bridge_test.go:52-60`). `snapshot` (`bridge_test.go:73-77`) copies the recorded commands under the lock. | `Upload`/`Download` are no-ops (`bridge_test.go:63-64`); identity is constant — container `sandbox-container-id`/`sandbox-container-name`, network `sandbox-network-name`, gateway `127.0.0.1`, workdir `/app`, run `test-run`, task `test-task` (`bridge_test.go:65-71`). It never returns a non-cancellation error, never produces a non-zero exit code unless configured, and does not honor cancellation while draining `stdin`. | `pkg/sandbox/docker.Sandbox` and its `ExecStream` ([section 8](#8-execution), `bridge.go:812`) |
| `newTestManager` | `bridge_test.go:79-86` | `New(Options{OutputDir, CleanupTimeout: 5s})`; no `Logger`, so the package-level `logrus` standard logger is used (`bridge.go:546-548`); `OmitRawLog` false, so both audit files are written | nothing | the constructor path `cmd/aries/wiring.go` takes |
| `clientConfig` | `bridge_test.go:91-107` | reads the private identity from `endpoint.IdentitySourceFile`, parses it with `ssh.ParsePrivateKey`, authenticates as `endpoint.Username` with that key only, restricts host-key algorithms to `Ed25519` (`bridge_test.go:103`), and sets a 5 s dial timeout | host-key verification: `HostKeyCallback` is `ssh.InsecureIgnoreHostKey()` (`bridge_test.go:104`), the closest stand-in for Hermes's forced `StrictHostKeyChecking=accept-new` first-use acceptance (`bridge.go:615-617`); the retained `known_hosts` is not consulted (that is `pinnedClientConfig` in the integration file) | Hermes's OpenSSH client configuration ([section 3](#3-lifecycle-start)) |
| `runExec` | `bridge_test.go:112-128` | opens one `session` channel on the shared client, calls `session.Setenv("LANG", "C.UTF-8")` and discards its error (`bridge_test.go:119`) — this is the `env` channel request OpenSSH sends before every `exec`, which the bridge refuses with `Reply(false)` while keeping the channel open (`bridge.go:752-763`); sets `Stdin` only when non-empty, captures `stdout` and `stderr` into builders, and calls `session.Run(payload)`, which sends the `exec` request with `WantReply` and waits for `exit-status`. Returns the two outputs and the error: `*ssh.ExitError` for a non-zero exit, a non-`nil` error when the `exec` request itself is refused. | ControlMaster multiplexing is not reproduced explicitly; each call opens a new channel on the one connection, which is the same server-side shape. Calls `t.Fatal` if `NewSession` fails, including from the non-test `goroutines` of the concurrency test (`bridge_test.go:114-117`, `bridge_test.go:401-407`). | the per-command channel sequence in [section 5](#5-connection-and-channel-handling) |
| `recordsOfType` | `bridge_test.go:133-141` | filters parsed audit records by `request_type` so tests that count `exec` records are not thrown off by the `env` record that precedes each one | nothing | none (reader of the [structured audit](#9-evidence-and-audit)) |
| `readToolCalls` | `bridge_test.go:143-167` | parses `tool-calls.jsonl` line by line with a 1 MiB scanner buffer (`bridge_test.go:152`), skipping blank lines and failing the test on any non-JSON line | nothing | none |

The seams `Manager.openAudit`, `Manager.afterStart`, and `bridgeSession.replyRequest`
(`bridge.go:62-63`, `bridge.go:97`) and the `exclusiveWriteOperations` injection point
(`bridge.go:1079-1082`) exist in this package but are not used by any test here; the equivalent
tests live in `pkg/bridge/openclawssh/bridge_test.go` only.

| Test | Line | Scenario | Invariant pinned | Port or retire |
| --- | --- | --- | --- | --- |
| `TestBridgeProxiesHermesCommandsAndRetainsEvidence` | 169 | `testSandbox` is configured to return exit 7, `stdout` `tool-output`, `stderr` `tool-diagnostic`. `Start` runs under a 20 s context and the endpoint is inspected. A Go `ssh` client dials with `clientConfig`; `runExec` sends the `env` request then an `exec` of `capturedAgentPayload` with no `stdin`. After the run the recorded command is inspected, `Stop` is called, and both `tool-calls.jsonl` and `ssh_raw.log` under `<outputDir>/test-task/bridge/` are read back (`bridge_test.go:228-256`). | endpoint: no `ClientCommand` or `ClientSourceFile` (`bridge_test.go:181-183`), protocol `ssh`, user `aries`, non-empty `IdentitySourceFile`, address on `127.0.0.1` with a non-empty port that is not `22` (`bridge_test.go:184-190`); run: `*ssh.ExitError` with status 7, `stdout`/`stderr` byte-exact; sandbox: exactly one command, `/bin/bash -c <script>` in `/app`, script containing the unescaped `eval` (`bridge_test.go:210-223`); audit: exactly one `exec` record with `status` `completed`, `operation_class` `agent`, `exit_code` 7, `container_id`/`run_id`/`task_id` from the sandbox; at least one `env` record, every one `unsupported`/`unknown` (`bridge_test.go:241-249`, [section 9](#requests-that-never-ran)); raw log contains `wire_command=` and `ARIES SSH CALL BEGIN` | **Port** nearly all of it; the `env` clause becomes "refused calls are recorded". |
| `TestBridgeAnswersBootstrapProbes` | 261 | `testSandbox` returns `stdout` `/root\n` for everything. After `Start` and dial, `runExec` sends `connectionProbePayload` then `remoteHomePayload`; both must return a `nil` error (exit 0 is the fake's default). The two recorded commands are inspected (`bridge_test.go:282-293`). Probe `stdout` is not asserted at this level. | both probes succeed; each reaches the sandbox as `/bin/sh -c <payload>`; the second carries the literal `echo $HOME`, proving the bridge did not substitute a home of its own ([section 7](#7-command-preparation)) | Port if probes survive. |
| `TestBridgeDeniesFileSyncAndRecordsItAsPolicy` | 298 | an empty `testSandbox`. After `Start` and dial, `runExec` sends `mkdir -p /root/.hermes /root/.hermes/credentials` with no `stdin`, then `tar xf - --no-overwrite-dir -C /root/.hermes` with `stdin` `archive-bytes`; both `exec` requests must be refused. The client is closed, `Stop` is called, and the `exec` records are read back (`bridge_test.go:327-340`). | zero commands reach the sandbox; exactly two `exec` records, each `status` `denied`, `error` containing `file sync is denied`, `operation_class` `sync` — never `agent` (`bridge.go:778-779`, [section 6](#6-wire-grammar)). Not asserted: `exit_code` `-1` on the denied records (pinned in the integration file), and that the `archive-bytes` `stdin` of a denied request is neither read nor retained (`bridge.go:873`). | **Port.** |
| `TestStopRevokesIdentityAndRetainsKnownHosts` | 345 | `Start` with a bare `testSandbox`; no client ever connects. `known_hosts` under `<outputDir>/test-task/bridge/` is read and must be non-empty; `Stop` is called; the file is read again and the private identity is `Stat`-ed (`bridge_test.go:356-375`). | `known_hosts` is byte-identical across `Stop` and the identity file is `ErrNotExist` afterwards — `finalize` removes only `identitySource` (`bridge.go:943-946`, [section 4](#4-lifecycle-stop-and-revocation)) | Port the credential half; the `known_hosts` half is SSH-specific. |
| `TestBridgeServesConcurrentChannelsOnOneConnection` | 380 | `testSandbox` returns `stdout` `ok`. One Go `ssh` client is dialed; twelve `goroutines` each call `runExec` on that client with `bash -c 'echo command-<a..l>'` (`bridge_test.go:396-408`); errors are collected on a channel and any one fails the test; the sandbox must have recorded twelve commands. | twelve session channels open concurrently on one connection each carry an `env` refusal and an `exec` to completion; `handleConnection` serves channels in their own `goroutines` (`bridge.go:731-736`, [section 12](#12-concurrency-and-locking)). Ordering and the audit are not inspected. | Port if the replacement multiplexes. |
| `TestBridgePassesStdinThrough` | 419 | `testSandbox` returns `stdout` `read`. After `Start` and dial, `runExec` sends `bash -c 'cat > out'` with `stdin` `piped-input`; the fake's `stdins` slice is inspected under its lock (`bridge_test.go:437-441`). | exactly one `stdin` capture equal to `piped-input` reaches `ExecStream` through the `recordedInput` wrapper (`bridge.go:809`, [section 8](#8-execution)). The audit's `stdin` field is not read here (pinned at `integration_test.go:436`). | **Port.** |
| `TestStopRevokesListenerAndIsIdempotent` | 444 | `Start` with an empty `testSandbox`; `clientConfig` is built *before* `Stop` because revocation deletes the identity file (`bridge_test.go:453-454`). `Stop` is called three times sequentially, each must return `nil`; a dial with the saved configuration must fail; the identity must be `ErrNotExist` (`bridge_test.go:455-466`). No client connected before `Stop`. | the first `Stop` revokes; the second and third take the `active == nil && !stopping` branch and return the stored `nil` (`bridge.go:895-899`); the listener is closed (`bridge.go:1002-1004`); credential material is gone ([section 4](#4-lifecycle-stop-and-revocation)) | **Port.** |
| `TestStopCancelsInFlightCommand` | 470 | `testSandbox` is built with a `block` channel that is never closed. After `Start` and dial, a `goroutine` calls `runExec` with `bash -c 'sleep forever'` and no `stdin`; the fake drains the immediate `EOF`, then parks on `block`/`ctx.Done()`. The test waits 200 ms, calls `Stop` under a 30 s context, and requires it to return `nil`; the `runExec` `goroutine` must finish within 10 s (`bridge_test.go:485-501`). | `revoke` cancels the serve context (`bridge.go:997-1001`), the fake returns `ctx.Err()`, `execute` sees `hasCancellationCause` true, records status `canceled` with exit 255 (`bridge.go:813-832`), `recordRevocationError` stores the pure cancellation, and `revocationError` reports `nil` (`bridge.go:953-969`, [`isPureCancellation`](#hascancellationcause-and-ispurecancellation)), so `Stop` succeeds and the channel closes. The `canceled` audit record itself is not read back. | **Port.** A ported version should also assert the `canceled` record. |
| `TestStartRejectsSecondSessionAndNonDockerSandbox` | 504 | `Start` succeeds with one `testSandbox`; a second `Start` on the same `Manager` with another `testSandbox` must return an error (`bridge_test.go:508-514`). | a second `Start` while active is rejected (`bridge.go:558-560`, [section 3](#3-lifecycle-start)) | Port. **Gap:** the name promises a non-Docker-sandbox check the body never makes — every sandbox passed satisfies `bridgeSandbox`. The capability assertion at `bridge.go:561-564` and the `stopping` half of the guard at `bridge.go:558` have no coverage. |
| `TestNewRequiresOutputDirectory` | 517 | `New(Options{OutputDir: "  "})` is called with no sandbox, listener, or client. | a whitespace-only output directory is rejected before anything is created (`bridge.go:533-535`) | Port. |
| `TestBridgeOmitsRawLogWhenConfigured` | 527 | `New` is called with `OmitRawLog: true`; `testSandbox` returns exit 0, `stdout` `ok`. After `Start`, `endpoint.LogPaths` is scanned; a client dials and `runExec` sends `capturedAgentPayload`; the client is closed and `Stop` called; `tool-calls.jsonl` is read as bytes and `ssh_raw.log` is `Stat`-ed (`bridge_test.go:540-566`). | no `LogPaths` entry ends in `ssh_raw.log` (`bridge.go:658-663`); the structured log exists and is non-empty; the raw file does not exist ([the two files and `OmitRawLog`](#the-two-files-and-omitrawlog)). Records are not parsed and `LogPaths` is not compared for exact equality. | **Port.** |
| `TestBinaryStdinNoteMatchesRawRetention` | 571 | two sub-tests build a `recordedInput` directly with an empty reader, write the bytes `0x00 0x01 0x02` into its buffer, set `n = 3`, and call `record(true)` then `record(false)` (`bridge_test.go:581-584`); the returned count, raw bytes, and overflow flag are discarded. | `safeStructuredText` returns false for control bytes (`bridge.go:249-259`), so the encoding is `binary-omitted` and the note says `retained in ssh_raw.log` only when the raw log is retained, `not retained` otherwise (`bridge.go:240-246`, [section 11](#11-privacy-and-redaction-guarantees)). The invalid-`UTF-8` branch at `bridge.go:250` is not exercised. | **Port.** |

### `pkg/bridge/hermesssh/integration_test.go`

#### Fixtures and helpers

| Helper | Where | What it fakes or reproduces | What it deliberately does not fake | Production behavior it stands in for |
| --- | --- | --- | --- | --- |
| `bridgeFixtureImage` | `integration_test.go:33` | `debian:12-slim` pinned by digest, the task image for the deterministic test; Debian because the bridge resolves the bare `bash` token to `/bin/bash` (`workspace.go:20`) and a `busybox` image could not run an agent command (`integration_test.go:29-32`) | nothing from Hermes is in this image | a benchmark task image |
| `hermesIntegrationImage` | `integration_test.go:41-51` | reads the Hermes image pin from `configs/versions.json` through `config.LoadVersions` and fails if the pin is empty, so the upstream test always runs against the image the experiments use | nothing | the `configs/versions.json` pin |
| `repositoryRoot` | `integration_test.go:53-69` | walks up from the working directory to the first `go.mod` | nothing | none |
| `driverScript` | `integration_test.go:73-88` | a Python program run *inside* the upstream Hermes image with its own interpreter: imports `tools.terminal_tool`, asserts `_get_env_config()["env_type"] == "ssh"`, prints the resolved host/port/user/cwd, and calls `terminal_tool(command="echo aries-integration-ok && pwd")`, printing the JSON result verbatim | the model: no LLM is in the loop; the tool is invoked directly, which is the same code path an agent turn takes | Hermes's terminal tool driving its OpenSSH client |
| `integrationSandbox` | `integration_test.go:90-139` | `ExecStream` (`integration_test.go:102-123`) records the command, then runs `command.Path` with `command.Args` on the *host* through `os/exec` (`exec.CommandContext`), with `Dir` set to the fixture's `workdir` (a `t.TempDir()`), `stdin`/`stdout`/`stderr` wired straight through, mapping `*exec.ExitError` to the exit code and any other failure to `-1` plus the error. Identity: container and name `integration-container`, network `host`, gateway `127.0.0.1`, run `integration-run`, task `integration-task` (`integration_test.go:127-133`). | Docker: no container exists; `/bin/bash` and `/bin/sh` are the host's. `Exec` always returns `errors.New("unused")` (`integration_test.go:96-98`), so any bridge path that called it would fail loudly. | `pkg/sandbox/docker.Sandbox` ([section 8](#8-execution)) |
| `requireDocker` | `integration_test.go:141-151` | skips when no `docker` binary is on `PATH` or `docker image inspect <image>` fails within 30 s | the check shells out to the `docker` CLI; it is test-only | none |
| `pinnedClientConfig` | `integration_test.go:480-489` | `clientConfig` (`bridge_test.go:91`) with `HostKeyCallback` replaced by `knownhosts.New(<artifactDir>/known_hosts)`, so the dial verifies the bridge's ephemeral host key against the file the bridge retains as evidence (`bridge.go:615-621`) | first-use acceptance; this is stricter than Hermes | a client that already pinned the host key |

Both tests depend on Docker but only the first self-skips: `requireDocker` at
`integration_test.go:160`; `TestBridgeExecMutatesTheEvaluatorSandbox` calls
`dockersandbox.PullImages` and fails rather than skips when the daemon is absent
(`integration_test.go:309-311`).

| Test | Line | Scenario | Invariant pinned | Port or retire |
| --- | --- | --- | --- | --- |
| `TestUpstreamHermesDrivesTheBridgeWithoutPatches` | 158 | the pinned upstream Hermes image is required locally. `Start` runs against an `integrationSandbox` whose workdir is a host temporary directory, under a 4 min context. The generated identity is copied to a `0600` file, `driverScript` is written out, and a `skill.md` containing the canary `aries-file-sync-canary` is planted in a temporary skills directory (`integration_test.go:173-196`). `docker run --rm --network host` starts the image with the driver, identity, and skills directory bind-mounted, `HERMES_HOME=/run/aries/hermes`, `TERMINAL_ENV=ssh`, `TERMINAL_SSH_HOST/PORT/USER/KEY` from the endpoint, `TERMINAL_CWD` set to the sandbox root, `TERMINAL_TIMEOUT=60`, and `--entrypoint /opt/hermes/.venv/bin/python /driver.py` (`integration_test.go:198-213`). Hermes's own OpenSSH client then connects (accepting the host key on first use), runs its bootstrap probes, attempts its `~/.hermes` file sync, and runs the tool command. Afterwards the driver output, the recorded commands, `Stop`, a walk of the sandbox root, and the structured audit are inspected. | the driver output contains `"env_type": "ssh"`, `aries-integration-ok`, and `"exit_code": 0` (`integration_test.go:221-226`); at least one command reached the sandbox and every one is either `/bin/bash ...` or `/bin/sh -c <one of the two probe payloads>` — any other path, including a leaked `tar`, `scp`, or `mkdir`, fails (`integration_test.go:232-250`); `Stop` returns `nil`; no file under the sandbox root contains the canary and `<root>/.hermes` does not exist (`integration_test.go:257-275`); the audit holds at least one `completed` and at least one `denied` record (`integration_test.go:278-293`). This is the only test in which the real OpenSSH client with ControlMaster drives the bridge ([section 5](#5-connection-and-channel-handling)). The raw log is not inspected. | **Port**, rewritten for the new client configuration. This is the only test proving the real harness drives the bridge with no patch layer. |
| `TestBridgeExecMutatesTheEvaluatorSandbox` | 303 | `bridgeFixtureImage` is pulled through the Moby SDK and a real `pkg/sandbox/docker` container is started with run `hermes-bridge-integration`, task `same-state`, workdir `/work`, 128 MB memory (`integration_test.go:309-330`); the bridge `Start`s against it. A Go `ssh` client dials with `pinnedClientConfig` over the retained `known_hosts`. Four `runExec` calls follow: `connectionProbePayload`; `remoteHomePayload`; the agent payload `bash -c 'cat > /work/bridge-state; cat /work/bridge-state; printf tool-stderr >&2; exit 7'` with `stdin` `streamed-input`; and the sync payload `mkdir -p /root/.hermes/skills` (`integration_test.go:346-374`). Between and after them the test runs its own commands directly through `sandbox.Exec` as the evaluator would. `Stop` is called, the listener and identity are probed, the container is used once more, and both audit files are parsed (`integration_test.go:381-475`). | probes: `stdout` exactly `SSH connection established\n`; the home probe is non-empty and does not contain the literal `$HOME`; agent: `*ssh.ExitError` 7, `stdout` `streamed-input`, `stderr` `tool-stderr`, and `/bin/cat /work/bridge-state` read directly from the container returns the streamed bytes; sync: refused, and `test ! -e /root/.hermes` passes inside the container; after `Stop`: identity `ErrNotExist`, a `TCP` dial to the endpoint fails within 200 ms, and `printf evaluator > /work/after-bridge` still runs in the container; `LogPaths` is exactly `[tool-calls.jsonl, ssh_raw.log]` under `<outputDir>/same-state/bridge/`, both `0600`; exactly four `exec` records in order — `bootstrap`/`completed`/0/probe, `bootstrap`/`completed`/0/home, `agent`/`completed`/7/agent payload, `sync`/`denied`/`-1`/no command — with the run, task, and container identity on each; the agent record carries `stdin` `streamed-input`, `stdin_encoding` `utf-8`, `workdir` `/work`, `path` `/bin/bash`; one `env` record per `exec`; neither artifact contains a `stdout`/`stderr` field; the raw log holds `wire_command=<sync payload>`, `status=denied`, `stdin=streamed-input`, `run_id=...`; the count of `ARIES SSH CALL BEGIN` equals the number of structured records ([section 9](#9-evidence-and-audit), [section 11](#11-privacy-and-redaction-guarantees)) | **Port** in full. This is the fixture that pins the end-to-end guarantees the migration is most likely to lose. |

### Scenario coverage map

Rows are the situations the bridge must handle, drawn from the branches of `bridge.go`; a scenario
with no covering test carries `none`. "Level" uses the four levels defined at the top of this
section.

| Scenario | Covered by | Level | Gap notes |
| --- | --- | --- | --- |
| Happy-path agent command, no `stdin`, non-zero exit propagated with `stdout`/`stderr` | `TestBridgeProxiesHermesCommandsAndRetainsEvidence` | fake-client | — |
| Happy-path agent command with `stdin` reaching the sandbox | `TestBridgePassesStdinThrough`, `TestBridgeExecMutatesTheEvaluatorSandbox` | fake-client, integration-docker | the fake-level test checks only the sandbox side; the audit `stdin` field is pinned only at integration level |
| Agent command mutates the container the evaluator later inspects | `TestBridgeExecMutatesTheEvaluatorSandbox` | integration-docker | — |
| Login-shell (`bash -l -c`) snapshot command | `TestDecodeAcceptsCapturedHermesPayloads`, `TestPreparePreservesLoginShell`, `TestEncodeRoundTripsToTheOriginalPayload`, `TestUpstreamHermesDrivesTheBridgeWithoutPatches` | unit, integration-upstream | no fake-client test sends `capturedLoginPayload` over SSH; at upstream level it is exercised but the resulting record is not singled out |
| Bootstrap probes answered from the sandbox | `TestDecodeAcceptsBootstrapProbes`, `TestPrepareBootstrapProbesReplayLiterally`, `TestBridgeAnswersBootstrapProbes`, `TestBridgeExecMutatesTheEvaluatorSandbox`, `TestUpstreamHermesDrivesTheBridgeWithoutPatches` | unit, fake-client, integration-docker, integration-upstream | — |
| File-sync denial: refused before the sandbox, recorded as `denied`/`sync`, exit `-1` | `TestDecodeDeniesCapturedFileSyncPayloads`, `TestBridgeDeniesFileSyncAndRecordsItAsPolicy`, `TestBridgeExecMutatesTheEvaluatorSandbox`, `TestUpstreamHermesDrivesTheBridgeWithoutPatches` | unit, fake-client, integration-docker, integration-upstream | that the `stdin` of a denied request is neither consumed nor retained (`bridge.go:873`) is not asserted |
| Malformed payload over SSH recorded as `rejected`/`unknown` (`bridge.go:780-782`, `bridge.go:855-857`) | `TestDecodeRejectsEverythingElse` (grammar only) | unit | none at bridge level: no test sends a malformed payload through a channel and reads back a `rejected` record |
| `NUL` or empty payload | `TestDecodeRejectsEverythingElse` | unit | none through SSH |
| Non-canonical quoting refused | `TestDecodeRequiresCanonicalQuoting`, `TestDecodeRejectsEverythingElse` | unit | — |
| `shlex` encode/decode round trip | `TestShlexQuoteMatchesPython`, `TestDecodeRoundTripsArbitraryScripts`, `TestEncodeRoundTripsToTheOriginalPayload` | unit | — |
| Workdir authority and `validWorkdir` rules | `TestPrepareMapsAgentCommandsToTheSandboxWorkdir`, `TestPrepareRejectsUnsafeWorkdir` | unit | `prepareRemoteCommand` failing *inside* `handleSession` (`bridge.go:785-790`) has no bridge-level test; every fake sandbox reports a valid workdir |
| Unknown command kind refused | `TestPrepareRejectsUnknownKind` | unit | — |
| No client environment reaches the sandbox | `TestPrepareMapsAgentCommandsToTheSandboxWorkdir`, `TestBridgeProxiesHermesCommandsAndRetainsEvidence` (`env` recorded as `unsupported`) | unit, fake-client | — |
| Non-`exec` channel request refused, channel kept open, refusal recorded | `TestBridgeProxiesHermesCommandsAndRetainsEvidence`, `TestBridgeExecMutatesTheEvaluatorSandbox` (one `env` per `exec`) | fake-client, integration-docker | only the `env` request type is exercised; `pty-req`, `shell`, `subsystem`, and `signal` are not |
| `exec` request without `WantReply` (`bridge.go:764-767`) | none | — | a Go client cannot send it through `Session.Run`; requires a raw channel |
| `exec` payload that fails `ssh.Unmarshal` (`bridge.go:768-773`) | none | — | requires a raw channel request |
| Non-`session` channel type or channel with extra data rejected (`bridge.go:723-726`) | none | — | e.g. `direct-tcpip` port forwarding; no test attempts one |
| Global requests: `keepalive@openssh.com` accepted, others refused (`bridge.go:740-747`) | none | — | Hermes's OpenSSH may send keepalives at upstream level, but nothing asserts the reply |
| Authentication: wrong user or key refused, `MaxAuthTries` 3 (`bridge.go:665-677`) | none | — | every test authenticates with the correct identity and `aries` |
| Handshake deadline cleared before the session outlives 5 s (`bridge.go:705`, `bridge.go:714`) | `TestUpstreamHermesDrivesTheBridgeWithoutPatches` (incidentally) | integration-upstream | no test deliberately holds a connection idle past `lockedConnectTimeout` and then issues a command |
| Concurrent channels on one connection | `TestBridgeServesConcurrentChannelsOnOneConnection` | fake-client | twelve channels; sequence numbers and record count are not asserted |
| Second `Start` while active | `TestStartRejectsSecondSessionAndNonDockerSandbox` | fake-client | — |
| `Start` while `Stop` is in flight (`bridge.go:558`, `stopping`) | none | — | — |
| Capability assertion failure: sandbox lacks `bridgeSandbox` (`bridge.go:561-564`) | none | — | named by the test above but never performed; `openclawssh` has an analogue |
| `NetworkGateway` failure before any resource is created (`bridge.go:565-568`) | none | — | — |
| Partial `Start` rollback through `fail`: artifact directory, key generation, identity write, listen, `known_hosts` write, audit open, `afterStart` (`bridge.go:574-589`, `bridge.go:590-643`); artifact directory removed (`bridge.go:947-949`) | none | — | the `openAudit`/`afterStart` seams are unused here; see [the `fail` rollback path](#the-fail-rollback-path) |
| `known_hosts` written as `[host]:port <key>` and usable for pinning | `TestBridgeExecMutatesTheEvaluatorSandbox` (via `pinnedClientConfig`), `TestStopRevokesIdentityAndRetainsKnownHosts` | integration-docker, fake-client | the fake-client test checks only non-empty and unchanged |
| Endpoint shape: no client helper, locked user, ephemeral port, `LogPaths` | `TestBridgeProxiesHermesCommandsAndRetainsEvidence`, `TestBridgeExecMutatesTheEvaluatorSandbox` | fake-client, integration-docker | — |
| `Stop` with an in-flight command: aborted, not awaited; `Stop` returns `nil` | `TestStopCancelsInFlightCommand` | fake-client | the `canceled`/255 audit record is not read back |
| `Stop` `idempotent` (sequential repeats) | `TestStopRevokesListenerAndIsIdempotent` | fake-client | — |
| Concurrent `Stop` callers share one result (`bridge.go:900-912`) | none | — | — |
| `Stop` that fails and is retried; session retained (`bridge.go:924-931`) | none | — | requires an injected audit or removal failure |
| `Stop` before any client connected, and after a client disconnected | `TestStopRevokesIdentityAndRetainsKnownHosts`, `TestStopRevokesListenerAndIsIdempotent`, `TestBridgeDeniesFileSyncAndRecordsItAsPolicy` | fake-client | — |
| Sandbox usable for evaluation after revocation | `TestBridgeExecMutatesTheEvaluatorSandbox` | integration-docker | — |
| Identity removed, listener closed, `known_hosts` kept at `Stop` | `TestStopRevokesIdentityAndRetainsKnownHosts`, `TestStopRevokesListenerAndIsIdempotent`, `TestBridgeExecMutatesTheEvaluatorSandbox` | fake-client, integration-docker | — |
| Sandbox returns a non-cancellation error: status `failed`, exit 255 (`bridge.go:825-832`) | none | — | `testSandbox.Exec` never returns an error; `integrationSandbox` returns one only if the host cannot start the process |
| Sandbox returns `nil` error after cancellation (`bridge.go:817-818`) or a foreign error joined with the cancellation (`bridge.go:819-821`) | none | — | the fail-closed half of [`hasCancellationCause`](#hascancellationcause-and-ispurecancellation) |
| `isPureCancellation` on a mixed joined error blocks `Stop` (`bridge.go:975-995`) | none | — | — |
| Exit-code clamp to `0..255` (`bridge.go:833-835`) | none | — | the fake returns `-1` only alongside an error, where 255 is already forced |
| `stdin` overflow past 16 MiB: buffer dropped, audit latched, no record written (`bridge.go:212-229`, `bridge.go:836-840`) | none | — | — |
| Audit byte budget exceeded and latched (`bridge.go:312-316`) | none | — | — |
| Audit enqueue after seal, marshal or render failure, write/`sync`/close failure, drain timeout, unfinished audit at `finalize` (`bridge.go:291-311`, `bridge.go:456-498`, `bridge.go:513-515`, `bridge.go:937-939`) | none | — | [latching](#latching-and-which-failures-block-revocation) has no test in this package |
| Binary `stdin` omitted from the structured record with an honest note | `TestBinaryStdinNoteMatchesRawRetention` | unit | invalid `UTF-8` (as opposed to control bytes) is not exercised; no SSH-level test sends binary `stdin` |
| Raw-log opt-out drops only `ssh_raw.log` | `TestBridgeOmitsRawLogWhenConfigured` | fake-client | records are not parsed under the opt-out |
| Raw and structured audits correlated one-to-one; no command output in either | `TestBridgeExecMutatesTheEvaluatorSandbox` | integration-docker | `command_hash`, `argv`, `stdout_bytes`/`stderr_bytes`, `duration_ms`, and `sequence` monotonicity are never asserted directly |
| Artifacts `0600`, directory `0700`, symlink refused (`bridge.go:261-267`, `bridge.go:1133-1149`) | `TestBridgeExecMutatesTheEvaluatorSandbox` (file mode only) | integration-docker | directory mode and the symlink rejection have no test in this package |
| Unmodified upstream Hermes end-to-end, no patch layer | `TestUpstreamHermesDrivesTheBridgeWithoutPatches` | integration-upstream | the raw log is not inspected; the sandbox is the host, not a container |
| Real Docker sandbox end-to-end with all four payload shapes | `TestBridgeExecMutatesTheEvaluatorSandbox` | integration-docker | does not self-skip without Docker |

**Coverage gaps a migration should not inherit** (the five named previously, confirmed, plus those
found above): the capability-assertion failure; the audit byte budget; the `stdin` overflow path;
`isPureCancellation` on a mixed joined error; a `Stop` that fails and is then retried; every
`fail(...)` site in `Start` and the partial-start directory removal; `Start` during `stopping`;
concurrent `Stop`; authentication refusal; non-`session` channels; `exec` without `WantReply`;
an exec payload that fails to `unmarshal`; a malformed payload recorded as `rejected` through a
real channel; a sandbox error without cancellation (`failed`/255); the two ambiguous
post-revocation error shapes; the exit-code clamp; every audit-writer failure and the seal timeout;
and the `canceled` audit record.

**Sources:** `pkg/bridge/hermesssh/grammar_test.go`, `pkg/bridge/hermesssh/workspace_test.go`,
`pkg/bridge/hermesssh/bridge_test.go`, `pkg/bridge/hermesssh/integration_test.go`,
`pkg/bridge/hermesssh/bridge.go`, `pkg/bridge/hermesssh/grammar.go`,
`pkg/bridge/hermesssh/workspace.go`, `pkg/bridge/openclawssh/bridge_test.go` (seam usage only).
