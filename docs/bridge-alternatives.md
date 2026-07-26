# Current tool bridge architecture and alternatives

## Decision

ARIES keeps the OpenClaw-to-Docker bridge concrete and pair-specific:

```text
OpenClaw container -> pinned SSH client -> host ARIES listener
                   -> Moby ExecStream -> exact task container
```

This path uses OpenClaw's unmodified upstream SSH integration while keeping the
Docker socket out of the harness and keeping task-container ownership in ARIES.
It also lets the evaluator inspect the same still-running container that the
agent changed. ARIES shares only the Runner-facing `ToolBridge` role; it does
not impose one remote-tool protocol on future harnesses.

The implementation is more than a byte relay. It authenticates a temporary
client, validates the pinned OpenClaw command grammar, virtualizes OpenClaw's
protocol-only workspace namespace, streams commands into the task sandbox,
records private replay evidence, and positively revokes access before
evaluation.

## Four component roles

The Runner consumes exactly four substitutable interfaces from `pkg/runner`:

| Role | Responsibility | What it does not own |
| --- | --- | --- |
| `Benchmark` | Loads the task, sanitizes the live sandbox before agent access, and independently evaluates final sandbox state. | Harness execution, transport credentials, and sandbox lifecycle. |
| `AgentHarness` | Runs the agent instruction against the configured model and temporary tool endpoint. | The task container, evaluator files, and bridge listener. |
| `ToolSandbox` | Starts and positively stops the task environment and returns its typed exec and file-transfer capability. | Harness policy and benchmark scoring. |
| `ToolBridge` | Grants one harness temporary, authenticated access to one supplied sandbox, then revokes it. | Sandbox creation, harness lifecycle, and evaluation. |

The OpenClaw-specific bridge may consume the Docker sandbox's narrow streaming
capability because it adapts that exact pair. The returned `Sandbox` capability
is not a fifth substitutable component role. The benchmark and harness do not
import one another, and concrete component construction remains an explicit
switch in `cmd/aries`.

## Lifecycle and isolation gates

For each task, the Runner performs this order:

1. load the benchmark task;
2. start the task sandbox;
3. ask the benchmark to remove and prove absence of verifier paths;
4. start the bridge for that exact sandbox;
5. start and run the harness;
6. positively stop the harness;
7. positively stop and revoke the bridge;
8. evaluate the still-running sandbox;
9. stop the sandbox.

Cleanup follows the reverse ownership order with a fresh bounded context after
run cancellation. A partially successful harness or bridge start is still
followed by its idempotent stop operation. If harness absence or bridge
revocation cannot be confirmed, evaluation is blocked: ARIES does not upload
verifier files or expose solutions and tests to a possibly live agent path.
Harness success or failure remains separate from the evaluation outcome.

Bridge shutdown closes the listener and active SSH connections, waits for
handlers, flushes and closes evidence, and deletes staged credential sources.
It does not stop or recreate the task container. A session-termination,
handler-drain, evidence, or credential-cleanup failure makes bridge shutdown
fail closed.

## SSH and tool flow

`pkg/bridge/openclawssh` binds an ephemeral listener on the task network's host
gateway. For each task it creates an ephemeral Ed25519 host key and client key,
plus a port-qualified `known_hosts` entry. The harness copies these files and
the static `aries-ssh` client into its stopped container; no credential or
client file is bind-mounted.

The client accepts only OpenClaw's pinned non-TTY call shape:

```text
-F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND
```

The server requires the generated key and task username. It accepts session
exec channels and the OpenSSH keepalive request, and rejects TTYs, subsystems,
forwarding, unsupported global requests, noncanonical quoting, and malformed
commands. After canonical token decoding, ordinary accepted commands become
typed `core.Command` values and run through `Sandbox.ExecStream` on the exact
sandbox later passed to the evaluator. Argument boundaries are retained;
binary and late stdin are streamed; stdout and stderr remain separate; and the
sandbox exit status returns through SSH.

Each tool exec has its own process group. When its context or SSH connection is
cancelled, the Docker sandbox targets only that process group and positively
checks its absence. It does not stop the task container as a cancellation
shortcut.

## Workspace discovery

The Terminal-Bench adapter derives the task workdir from the selected task's
pinned-checkout Dockerfile:

```text
.cache/terminal-bench-2/<task-name>/environment/Dockerfile
```

The parser processes logical instructions in order, treats instruction names
case-insensitively, resolves relative `WORKDIR` values against the prior value,
and resets that state at each `FROM` so the final stage determines the result.
Only a shell-neutral absolute container path is accepted. Missing files and
absent, variable-based, JSON-form, ambiguous, malformed, or unsafe `WORKDIR`
instructions fall back to container root `/`. Non-not-found I/O errors still
propagate. This supports task-specific values such as `/workspace` and
`/app/personal-site` without assuming one dataset-wide directory.

The root fallback is deliberately narrow. Docker sandbox validation permits
`/` only where a workdir is valid: `core.Environment.Workdir` and
`core.Command.Dir`. Executable paths and upload/download paths retain their
stricter validation and cannot use root as a file-transfer target.

## Pair-specific virtual namespace mapping

OpenClaw's pinned SSH backend generates two protocol paths:

```text
virtual runtime root: /aries/openclaw/openclaw-ssh-shared-8198076c
virtual workspace:    /aries/openclaw/openclaw-ssh-shared-8198076c/workspace
```

In `pkg/bridge/openclawssh/workspace.go`, these are the package-private
`virtualRuntimeRoot` and `virtualWorkspace` constants. `prepareRemoteCommand`
owns control classification, HOME mapping, and outer-prefix removal;
`translateVirtualWorkspace` owns the bounded remainder scan.

These are virtual names only. The bridge does not create either path, a
symlink, an ownership marker, or any other filesystem alias in the task
container.

Mapping happens only after canonical SSH token decoding and follows this
fail-closed order:

| Control | Exact decoded form after `/bin/sh -c` | Bridge action |
| --- | --- | --- |
| Runtime-root probe | `if [ -d "$1" ]; then printf "1\n"; else printf "0\n"; fi`, sentinel `openclaw-sandbox-check`, then the virtual runtime root | Replace only the final path argument with the sandbox workdir and execute the unchanged probe. |
| Workspace clear | `mkdir -p -- "$1" && find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +`, sentinel `openclaw-sandbox-clear`, then the virtual workspace | Consume stdin and return success without executing against the sandbox. |
| Runtime-root removal | `rm -rf -- "$1"`, sentinel `openclaw-sandbox-remove`, then the virtual runtime root | Consume stdin and return success without executing against the sandbox. |

1. **Classify transport controls.** The bridge recognizes the pinned
   runtime-root probe, workspace-clear command, and runtime-root-removal command
   only by their complete decoded argument vector: shell path, exact script,
   sentinel, arity, ordering, and path must all match.
   For the benign probe, only its final runtime-root argument is replaced with
   the sandbox workdir. The two transport cleanup controls are handled
   harmlessly and never run against the benchmark workdir. Near-matches are not
   classified as controls.
2. **Validate the generated environment.** A generated tool command must
   use the pinned `env` order `PATH`, `HOME`, `LANG`,
   `OPENCLAW_SHELL=exec`, then `/bin/sh`. It must contain exactly one
   `HOME=<virtual workspace>` in that position. The bridge maps that value to
   the sandbox workdir. Missing, duplicate, reordered, wrong-case, or
   near-match values are rejected before sandbox execution. Ordinary canonical
   commands without the generated workspace prefix keep unrelated `HOME`
   values; an exact virtual HOME outside the pinned shape is rejected.
3. **Remove the exact outer workspace prefix.** Generated tool execution is
   recognized only by this exact decoded prefix:

   ```text
   cd '/aries/openclaw/openclaw-ssh-shared-8198076c/workspace' &&
   ```

   The prefix includes one ASCII space after `&&`. The bridge removes that
   protocol prefix and uses the sandbox workdir as `core.Command.Dir`; it does
   not construct a new user command by shell-string concatenation.
4. **Translate bounded workspace operands.** A byte scanner translates an
   exact virtual-workspace base only when both sides have approved shell-token
   boundaries. The left boundary must be start-of-string or one of space, tab,
   LF, CR, single quote, double quote, backtick, `(`, `)`, `{`, `}`, `[`, `]`,
   `;`, `|`, `&`, `<`, `>`, `=`, `:`, or `,`. The right boundary accepts the
   same set, end-of-string, or `/` for a descendant. The scanner copies every
   other byte and every descendant suffix unchanged, supports multiple eligible
   occurrences, and never performs a global substring replacement. This covers
   trace-observed absolute operands, redundant `cd` operations, redirections,
   heredoc bodies, and quoted literals while rejecting embedded or ambiguous
   near-matches.
5. **Fail closed on unresolved names.** An ordinary generated command may never
   contain the virtual runtime root. Any unresolved exact or ambiguous virtual
   namespace also causes rejection before `Sandbox.ExecStream`.

The join is root-safe. With workdir `/app/personal-site`, a virtual descendant
`/file` becomes `/app/personal-site/file`. With workdir `/`, the same descendant
becomes `/file`, never `//file`; a standalone virtual workspace becomes exactly
`/`. Ordinary agent-authored reads, writes, and removals within the bounded
workspace remain ordinary benchmark authority. Only the byte-exact transport
cleanup controls are suppressed.

This coupling is intentional and pinned: the virtual paths, generated command
prefix, environment order, probe, and cleanup controls belong to the selected
OpenClaw version. A future OpenClaw upgrade must update and re-test this adapter
rather than relaxing it into heuristic command rewriting.

## Why the shared symlink was removed

The former design created OpenClaw's fixed workspace path inside every task
container and pointed it at the benchmark workdir. That made a harness protocol
detail persistent evaluator-visible state and required ownership markers,
partial-start rollback, release handling, and symlink-specific cleanup. It also
made root fallback and heterogeneous task workdirs harder to reason about.

Virtual mapping removes that shared mutable alias. The task image keeps its own
Dockerfile-derived workdir, transport cleanup cannot follow an alias into that
directory, and bridge start no longer mutates the sandbox merely to establish a
path.

### No-alias end-to-end contract

Integration coverage checks the former fixed path with both `! -e` and `! -L`
immediately after bridge start and again after bridge revocation. It exercises
distinct non-root task workdirs and the `/` fallback, sends workspace operands
through the live bridge, and proves that mutations appear at the translated
target rather than at a protocol alias. The root case must produce
`/descendant`, never `//descendant`.

The qualifying OpenClaw path also runs the pinned runtime-root probe and an
exact generated command containing a redundant virtual-workspace `cd`, an
absolute descendant operand, and an absolute redirection target. After positive
harness stop and bridge revocation, the old endpoint must reject commands and
the independent evaluator must observe the same mutation in the still-running
sandbox. Cleanup then confirms that no managed container, network, process, or
credential remains.

## Evidence and privacy

Every task retains two private mode-0600 bridge artifacts:

```text
<task>/bridge/tool-calls.jsonl
<task>/bridge/ssh_raw.log
```

`tool-calls.jsonl` contains one valid JSON object per line. It describes the
translated command that was actually executed, including its argument vector,
workdir, environment names, nonsecret `workspace_home`, command hash, stream
byte counts, exit status, and outcome. Printable UTF-8 and HTML
characters remain literal; JSON-required quoting and control escaping remain
intact. Printable stdin remains inline. Binary or control-bearing stdin uses a
short `binary-omitted` marker with the authoritative byte count instead of
expanding opaque bytes into JSON escapes; the lossless bytes remain in
`ssh_raw.log`. Environment values and stdout/stderr content are not stored
there.

`ssh_raw.log` is a lossless, human-readable plain-text record of the wire side,
including the original decoded command, virtual HOME, payload, and stdin. It
uses fixed-order `key=value` fields between explicit call delimiters. Printable
UTF-8 remains literal and controls or invalid bytes use unambiguous escapes.
If a malformed request cannot yield a command, `wire_command` is empty but the
exact payload and byte count remain available.
This distinction is important during workspace translation: raw evidence shows
what OpenClaw sent, while structured evidence shows what the sandbox executed.

One bounded asynchronous writer owns both files. Handler paths enqueue complete
immutable records rather than performing storage I/O. Admission, write, sync,
drain, or close failure is latched and prevents positive bridge revocation.
Treat both artifacts as sensitive replay evidence because tool inputs may
contain private task data. Model/API credentials and SSH private-key bytes do
not enter either command record; model credentials are not sent to the task
sandbox.

## Alternatives and tradeoffs

| Design | Security and ownership | Workspace and image assumptions | Cancellation, revocation, and evidence | Evaluator visibility | Decision |
| --- | --- | --- | --- | --- | --- |
| **Current host SSH-to-Moby bridge** | ARIES owns the task container and ephemeral credentials; the harness receives neither the Docker socket nor verifier data. | Uses OpenClaw's pinned SSH grammar and virtual namespace; derives each task workdir from its Dockerfile. Task images need no SSH daemon or Python helper. | Targets one exec process group, positively revokes sessions, and retains paired wire/executed evidence. | Agent changes occur in the exact still-running container later evaluated; no bridge alias remains. | **Selected for OpenClaw.** |
| **Install or require `sshd` in every task image** | Moves daemon, user, key, port, and process ownership into the evaluator's sandbox. | Assumes each heterogeneous benchmark image can run and safely configure SSH. | Adds another service to stop and audit; transport logs depend on image configuration. | The evaluator sees daemon and credential setup mutations unrelated to the task. | Rejected. |
| **Give OpenClaw the Docker socket** | Exposes daemon authority far beyond one task container and lets the harness create, inspect, or remove unrelated resources. | Uses the harness's Docker behavior rather than the exact ARIES sandbox boundary. | ARIES cannot narrowly revoke per-task daemon authority or guarantee equivalent evidence. | The harness could replace or bypass the container intended for evaluation. | Rejected. |
| **Generic remote-tool relay** | A shared protocol would need to encode the union of harness authentication and command semantics before a second implementation exists. | Risks hiding pair-specific workspace, transfer, and image assumptions behind weak abstractions. | Cancellation, revocation, and replay guarantees would become lowest-common-denominator policy. | Translation mistakes could target state other than the evaluator's sandbox. | Rejected until multiple real adapters reveal a smaller shared seam. |
| **Hermes-specific SSH adapter** | Could preserve ARIES ownership, but must implement Hermes's own connection and authentication behavior. | Hermes SSH/SCP, ControlMaster, remote-home, and sync semantics differ from OpenClaw's virtual workspace. | Needs separate cancellation, transfer, revocation, and evidence tests. | Can target the ARIES sandbox only through a Hermes-specific adapter. | Candidate only when Hermes is implemented; do not reuse `openclawssh` by name alone. |
| **OpenHands workspace adapter or controlled agent-server sidecar** | Can preserve narrow authority if ARIES controls the workspace endpoint or sidecar lifecycle. | OpenHands uses `Workspace`/`RemoteWorkspace` over HTTP/WebSocket rather than OpenClaw's SSH grammar. | Must map its API operations, sessions, streaming, cancellation, and artifacts explicitly. | Can modify the evaluator's sandbox if the adapter delegates to its typed capabilities. | Candidate only when OpenHands is implemented; do not force it through SSH. |

## Extension boundary

Only `ToolBridge` is shared. A future harness adds a concrete adapter for its
real upstream boundary and an explicit constructor switch. Small encoding or
evidence helpers should move into shared code only after a second implementation
proves the common behavior. Authentication, workspace mapping, transfer,
cancellation, revocation, and privacy policy remain harness-specific unless
evidence shows otherwise.

## Upstream references

- OpenClaw [sandbox documentation](https://github.com/openclaw/openclaw/blob/main/docs/gateway/sandboxing.md),
  [SSH transport](https://github.com/openclaw/openclaw/blob/main/src/agents/sandbox/ssh.ts),
  and [SSH backend](https://github.com/openclaw/openclaw/blob/main/src/agents/sandbox/ssh-backend.ts).
- Hermes [configuration](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/user-guide/configuration.md),
  [SSH environment](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/ssh.py),
  and [Docker environment](https://github.com/NousResearch/hermes-agent/blob/main/tools/environments/docker.py).
- OpenHands [Workspace architecture](https://docs.openhands.dev/sdk/arch/workspace),
  [agent-server overview](https://docs.openhands.dev/sdk/guides/agent-server/overview),
  and [Docker sandbox](https://docs.openhands.dev/openhands/usage/sandboxes/docker).
