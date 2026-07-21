# ARIES Design

Status: **implemented through M2**. The RALPLAN-DR Architect and sequential
Critic approved the source draft without blockers. M0 then confirmed the
pinned runtime assumptions below; later milestones must return to planning if
one of these locked contracts changes.

## Goal and workspace boundaries

ARIES is a small Go benchmark runner. Its first path runs the real pinned
Terminal-Bench 2 `fix-git` task with an unmodified upstream OpenClaw container,
a remote OpenAI-compatible model, one local Docker sandbox, and an independent
verifier.

- `invitro` is read-only structural and style evidence for a direct `cmd` plus
  `pkg` Go layout.
- `agent_bench` is read-only workflow archaeology. No Python architecture or
  source is migrated.
- No file was written or changed outside `aries`. Before this boundary was
  locked, two early read-only `git status` checks ran once in `invitro` and
  once in `agent_bench`; they changed nothing. From M0 onward, all Git
  operations run in `aries`.
- `.omx/`, `.cache/`, `runs/`, generated credentials and config under
  `.aries/`, and `DEEPSEEK_API.key` are ignored and untracked.
- `AGENTS.md` and `PROMPT.md` are deliberately reviewed for secrets and tracked
  in M0 as the workspace contract and originating requirements, not silently
  ignored or left untracked.

The module is `github.com/hyscale-lab/aries`. The MVP has no Kubernetes,
plugins, registration, reflection, DI framework, generic factories, config
inheritance, future stubs, or Harbor runtime dependency.

## Pinned first path

| Component | Pin |
| --- | --- |
| Terminal-Bench 2 | commit `2fd12b88aafdd04a52c298e3940bcb189f9766d6` |
| `fix-git` task image | `alexgshaw/fix-git:20251031@sha256:61e431c00c58df652287aadce5457634d9f9330cfdd153ebdf2802df0d540119` |
| OpenClaw source | tag `v2026.5.26`, commit `10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7` |
| OpenClaw image | `ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3` |
| Initial live model | `https://api.deepseek.com`, `deepseek-v4-flash` |

The deterministic completion proof uses the pinned real task, image, private
verifier, and expected reward `1`. The optional DeepSeek run never substitutes
for that proof.

## Durable upstream evidence and compatibility lock

These commit-permalinked primary sources are the M0 evidence record:

- TB2 task and verifier:
  [task.toml](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/task.toml),
  [instruction](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/instruction.md), and
  [tests](https://github.com/harbor-framework/terminal-bench-2/blob/2fd12b88aafdd04a52c298e3940bcb189f9766d6/fix-git/tests/test.sh).
- OpenClaw SSH schema:
  [types.sandbox.ts lines 102-124](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/config/types.sandbox.ts#L102-L124).
- OpenClaw SSH config and argv:
  [ssh.ts lines 479-579](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh.ts#L479-L579).
- OpenClaw exec wrapping and workspace semantics:
  [ssh.ts lines 265-293](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh.ts#L265-L293) and
  [ssh-backend.ts lines 114-177 and 284-305](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/agents/sandbox/ssh-backend.ts#L114-L177).
- Published image tools and readiness:
  [Dockerfile packages lines 159-171](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/Dockerfile#L159-L171) and
  [runtime and readiness lines 303-325](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/Dockerfile#L303-L325).
- One-turn run API:
  [register.agent.ts lines 62-139](https://github.com/openclaw/openclaw/blob/10ad3aa16068baa84a1bd9ac4f7d42ae725cedb7/src/cli/program/register.agent.ts#L62-L139).

M0 executed the pinned image and confirmed its digest, non-root user,
`/readyz`, `node openclaw.mjs agent --message ... --json`, and absence of a
system `ssh`. Any later mismatch blocks implementation and returns to
RALPLAN-DR.

### M0 executable compatibility evidence

The 2026-07-21 M0 probes used the exact digest-qualified image references above
and left no probe container behind.

- Docker reported the OpenClaw OCI index digest exactly as pinned on
  `linux/amd64`, with user `node`, entrypoint `tini -s --`, and default gateway
  command. Inside the image OpenClaw reported version `2026.5.26`; `id` reported
  UID/GID 1000; `ssh` and `scp` were absent; `tar` and Python 3 were present.
- The shipped `agent --help` accepted `--agent`, `--session-key`, `--message`,
  `--json`, and `--timeout`. A temporary gateway started with the upstream
  diagnostic `--allow-unconfigured` flag and a probe-only token; an in-container
  request to `http://127.0.0.1:18789/readyz` returned HTTP 200 with
  `"ready": true`. Production runs render an explicit config and do not rely on
  that diagnostic flag.
- Inspection of the shipped distribution confirmed the exact non-TTY argv
  suffix `-F CONFIG -T -o RequestTTY=no HOST REMOTE_COMMAND`, the strict-host
  config directives, canonical single-quote escaping, direct construction of
  `/bin/sh -c` or `env ... /bin/sh -c` requests, and the runtime path formula.
  Applying that shipped formula to scope key `shared` produced
  `openclaw-ssh-shared-8198076c`; its workspace is
  `<workspaceRoot>/<runtimeID>/workspace`.
- Docker reported the `fix-git` image digest exactly as pinned on
  `linux/amd64`. Both its image metadata and a runtime probe reported
  `/app/personal-site`; `git rev-parse --show-toplevel` returned that same path.

The commit-pinned primary-source permalinks remain the human-readable evidence
index. The executable probes deliberately lock only the published artifacts
and shipped behavior needed by ARIES; they do not claim compatibility with
other OpenClaw or Terminal-Bench revisions.

The locked OpenClaw sandbox fragment is:

```json5
agents: {
  defaults: {
    sandbox: {
      mode: "all",
      scope: "session",
      backend: "ssh",
      workspaceAccess: "none",
      ssh: {
        target: "aries@task-sandbox:2222",
        command: "/opt/aries/bin/aries-ssh",
        workspaceRoot: "/aries/openclaw",
        strictHostKeyChecking: true,
        updateHostKeys: false,
        identityFile: "/run/aries/ssh/id_ed25519",
        knownHostsFile: "/run/aries/ssh/known_hosts"
      }
    }
  }
}
```

No inline identity or known-host data is rendered. The bridge pre-creates the
pinned upstream session runtime path and aliases its computed `workspace`
directory to `Task.Environment.Workdir`. OpenClaw therefore edits the same live
filesystem later inspected by the evaluator without clearing or copying it.
The harness uses a fixed task session key, and M0 locks the pinned runtime-ID
formula from `ssh-backend.ts`. A path mismatch blocks implementation.

The helper client accepts exactly this non-TTY argv shape:

```text
/opt/aries/bin/aries-ssh -F CONFIG -T -o RequestTTY=no openclaw-sandbox REMOTE_COMMAND
```

It rejects reordered, repeated, unknown, TTY, forwarding, subsystem, proxy,
agent, X11, and extra destination arguments. It parses only the directives
emitted by the pinned OpenClaw source: `Host`, `HostName`, `Port`, `BatchMode`,
`ConnectTimeout`, `ServerAliveInterval`, `ServerAliveCountMax`,
`StrictHostKeyChecking`, `UpdateHostKeys`, `User`, `UserKnownHostsFile`,
`IdentityFile`, and `IdentitiesOnly`, with the locked safe values above.

The server accepts only an SSH `exec` request from the per-task public key. The
request payload must decode under the pinned canonical single-quote grammar to:

```text
request := q("/bin/sh") q("-c") q(script) q(arg)*
request := q("env") q(NAME=VALUE)* q("/bin/sh") q("-c") q(script) q(arg)*
q(value) := single-quoted value using only the canonical embedded-quote escape
```

The server decodes tokens and invokes the decoded argv directly with a bounded
context; it never passes the raw SSH request to a permissive outer shell. The
inner `/bin/sh -c script` is the intentional OpenClaw tool execution inside the
task sandbox. NULs, malformed quoting, invalid or secret environment names,
noncanonical encodings, other command heads, and all non-exec SSH requests are
rejected. Password and keyboard-interactive authentication, PTY, forwarding,
agent and X11 forwarding, subsystems, and environment requests are disabled.
The listener is reachable only on the task-scoped Docker network.

## Architecture

The Runner substitutes exactly four main interfaces, defined in `pkg/runner`:

```go
type Benchmark interface {
    Tasks(context.Context) ([]core.Task, error)
    Evaluate(context.Context, core.Task, Sandbox) (core.Evaluation, error)
}

type AgentHarness interface {
    Start(context.Context, core.HarnessRequest) error
    Run(context.Context, string) (core.HarnessResult, error)
    Stop(context.Context) error
}

type ToolSandbox interface {
    Start(context.Context, core.Environment) (Sandbox, error)
}

type ToolBridge interface {
    Start(context.Context, Sandbox) (core.ToolEndpoint, error)
    Stop(context.Context) error
}
```

`Sandbox` is the returned live capability, not a fifth component role. Helpers
stay concrete and package-private. Each component rolls back resources acquired
by a failed `Start` before returning. Cleanup uses a bounded context derived
with `context.WithoutCancel`, not the cancelled run context, and joins primary
and cleanup errors with the primary failure still identifiable.

Responsibilities remain direct: `core` owns data; `runner` owns the four
interfaces and lifecycle; Terminal-Bench owns discovery and private evaluation;
Docker owns Docker; the bridge owns SSH; OpenClaw owns harness config and run;
`cmd/aries` owns explicit construction. Benchmark and harness do not import one
another.

Monitoring is a concrete observer, not a fifth Runner role. M6 runs with no
observer and records observer status `not_enabled`. In M7 the composition root
starts `monitor.Recorder` before a fresh `Runner.Run`, the recorder discovers
run-labeled containers without controlling them, and the root stops it after
Runner cleanup using the same bounded cleanup policy. Observer failure is a
separate result and never starts, stops, retries, or scores a task.

### M1 implementation refinements

M1 keeps `Sandbox` as the capability returned by `ToolSandbox`, so the Runner
still substitutes exactly four roles. `ToolEndpoint` carries protocol,
address, and credential-file paths but never credential bytes. The experiment
loader has one visible default, `output_dir = "runs"`, rejects unknown fields
at every struct level, and rejects trailing JSON values. Known component types
remain explicit switches in `cmd/aries`; the CLI fails clearly until their
concrete milestone packages exist.

The generic interfaces deliberately use a nil `Stop` error as the component's
positive termination or revocation confirmation. A concrete M3-M5 `Stop` may
return nil only after the stronger Docker and SSH checks locked above. If a
gate fails, the Runner records `blocked_isolation` and never calls `Evaluate`,
even when a later cleanup retry succeeds. `Start` failures are
self-rolled back by the component and receive no Runner `Stop` call.

Isolation calls and post-evaluation cleanup each receive a fresh bounded
context derived with `context.WithoutCancel`; evaluator runtime therefore does
not consume the sandbox cleanup budget. Stops within one phase share its
deadline, bounding the phase as a whole. `errors.Join` retains harness,
isolation, evaluation, and cleanup causes while the result keeps those outcomes
separate. The M1 observer result is explicitly `not_enabled`; monitoring still
does not participate in Runner lifecycle control. M1 proves that the Runner
retains no lifecycle state across repeated `Run` calls. It does not claim that
generic interfaces make `Stop` implementations race-safe or idempotent; those
are concrete, integration-tested obligations for the Docker sandbox, SSH
bridge, and OpenClaw harness in M3-M5.

### M2 Terminal-Bench adapter

`pkg/benchmark/terminalbench` supports only `fix-git` at the pinned TB2 commit.
The setup command creates a shallow detached checkout under the ignored
`.cache/terminal-bench-2` directory, verifies `HEAD` exactly, is idempotent for
the correct revision, and refuses to alter an existing wrong revision. The
runtime adapter repeats that revision check before discovery.

The adapter strictly decodes the real task TOML, rejects unknown keys and the
execution-critical capabilities not implemented by the MVP, verifies the final
Dockerfile workdir, and translates the declared image tag to the locked digest.
Only the stable ID, instruction, and generic environment leave the package.
Verifier paths, files, timeout, environment, SHA-256 digests, and stable file
metadata stay in a private table keyed by task ID; solution content is never
read or exposed. Evaluation reverifies the clean dataset revision and rejects a
verifier file that became a symlink, nonregular file, replacement, or mutation.

Evaluation uses only the live `runner.Sandbox` capability. It first removes the
fixed sandbox-owned `/tests` and `/logs/verifier` paths, recreates clean
directories, then uploads only the two privately enumerated regular files to
exact destinations. It runs the pinned `/tests/test.sh` command with its own
timeout and environment and downloads fresh CTRF and authoritative reward
alongside captured stdout and stderr. Host artifact paths are cleared before
collection, so stale or symlink-preseeded sandbox state cannot supply a result.
Reward `1` is success, reward `0` is a valid failed evaluation, and missing or
malformed reward is an evaluator error. Process exit alone is never treated as
the score.
The M2 tests use fakes, so unit checks require neither Docker nor a model API;
the integration-tagged checkout test reads the real ignored pin when present.

Go's standard library has no TOML decoder. M2 therefore adds only
`github.com/BurntSushi/toml` v1.4.0 (MIT) for strict typed decoding;
there is no Harbor runtime dependency.

## Exact lifecycle and evaluation isolation

For each task:

1. obtain the task;
2. start the sandbox;
3. start the bridge;
4. give endpoint and model configuration to the harness;
5. start the harness;
6. run one instruction;
7. wait for completion, failure, cancellation, or timeout;
8. stop the harness and positively confirm container exit and `State.Running`
   false;
9. stop the bridge and positively confirm listener and server-process absence,
   old-key reconnect rejection, and credential deletion;
10. only after both confirmations, inject and run the private evaluator in the
    still-live sandbox;
11. collect separate harness, isolation, evaluation, observer, and cleanup
    outcomes and artifacts;
12. stop and remove the sandbox.

An ordinary harness failure still permits evaluation when both isolation gates
pass and the sandbox remains valid. If either gate fails, no verifier material
is injected, evaluation is `blocked_isolation`, cleanup continues, and primary
plus cleanup errors remain visible. Every Stop is idempotent. Partial Start
rollback is owned by the component that partially acquired the resource.

## DeepSeek secret flow

The experiment stores only `api_key_env: "DEEPSEEK_API_KEY"`. For an explicitly
requested bounded live run, a host loader reads the user-provided, ignored
`DEEPSEEK_API.key` once, rejects empty, oversized, NUL-containing, or multiline
content, removes one terminal newline, and passes the value only as
`DEEPSEEK_API_KEY` in the OpenClaw harness container runtime environment via the
Docker API. It never appears in argv, rendered config, another host file, task
sandbox or evaluator environment, logs, results, telemetry, or tracked content.
The harness container is positively confirmed removed afterward.

A missing or invalid file, failed bounded provider preflight, or unavailable
provider records a finite skip reason and performs no unbounded retries. Canary,
redaction, exact-byte scans, and tracked-file checks are release gates.

## Evidence and repository completion contract

M6 writes an immutable **functional-oracle manifest** for its deterministic
reward-`1` run, with observer status `not_enabled` and no samples. M7 never
mutates it. After monitoring exists, M7 performs a fresh deterministic reward-`1`
run and writes a separate immutable **monitored release manifest** containing
observer samples and outcome.

Before the reviewed M7 commit, all build, test, integration, scan, manifest,
README, DESIGN, and TASKS writes finish. README documents setup, exact run
commands, artifact interpretation, architecture, and adding one concrete
component package plus one explicit constructor switch. All TASKS boxes close
and the complete diff is reviewed before that commit. After M7 commit, only
read-only Git and inventory verification runs; no document, checkbox, manifest,
scan output, or project artifact is changed.

## ADR-001 summary

Select the bridge-owned static Go client and server while retaining OpenClaw's
upstream SSH backend. This preserves the unmodified image and the four-role
Runner. M0 accepts this decision after the approved Architect/Critic review and
the executable probes above. The detailed alternatives, invalidation triggers,
and security pre-mortem remain in the local OMX planning record. The decision
is invalidated if the pinned argv, config, workspace, or exec grammar cannot be
supported without a permissive fallback; that event returns to planning rather
than weakening the boundary.
