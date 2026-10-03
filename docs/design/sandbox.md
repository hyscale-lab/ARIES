# ToolSandbox

`ToolSandbox` owns the task environment. It starts one isolated environment for
a task, returns the narrow live capability needed by the selected bridge and
benchmark, and stops the environment with positive confirmation that owned
resources are absent. It does not own harness policy, tool credentials, or
benchmark scoring.

## Operations and ownership

The [Runner interfaces](../../pkg/runner/interfaces.go) define:

| Operation | Contract |
| --- | --- |
| `Start(ctx, SandboxRequest) (Sandbox, error)` | Create and validate a live task environment; clean up partial allocation on failure. |
| `Stop(ctx, Sandbox) error` | Release the owned environment; success confirms absence. Repeated stops are safe. |
| `Sandbox.NetworkName() string` | Return the task attachment created at startup; current harnesses require a nonempty shared deployment network name. Ownership stays with the sandbox. |
| `Sandbox.Exec(ctx, Command)` | Execute exact argv with context cancellation; return a command result separately from transport errors. |
| `Sandbox.Upload(ctx, source, destination)` | Transfer a host file into the task environment. |
| `Sandbox.Download(ctx, source, destination)` | Retrieve a task file into private run output; distinguish missing files from transport or runtime loss. |

`Sandbox` is the live capability returned by `ToolSandbox`, not another Runner
role. Optional `StreamExecutor.ExecStream` and
`LimitedDownloader.DownloadLimit` capabilities support streamed command I/O and
bounded host downloads. A bridge or evaluator requiring an optional capability
must check it before use.

The current [sandbox implementation](../../pkg/sandbox/sandbox.go) separates task
policy from its injected [deployment and task environment](deployment.md).
Successful construction transfers deployment transport ownership to the manager;
Runner owns the sandbox lifecycle. `Manager.Close` retries failed startup
cleanup and closes the transport; it does not replace `Stop` for active tasks.

## Lifecycle, isolation, and failure

The sandbox begins before bridge or harness startup. The benchmark sanitizes it
before agent access and evaluates that same live environment after harness stop
and bridge revocation are positively confirmed. Harness failure does not decide
the evaluation outcome. See the [task lifecycle](../design.md#task-lifecycle-and-isolation-gates).

The current adapter validates execution defaults, starts a fresh task attachment,
creates and validates the runtime, starts it, and confirms it is running before
returning the capability. Partial startup uses a fresh bounded cleanup context;
unconfirmed cleanup is retained for retry. Stop rejects another manager's
sandbox, supports concurrent/repeated callers, and removes the runtime before
its task attachment. A cleanup error remains visible rather than being treated
as confirmed absence.

Commands retain absolute executable paths and exact argument boundaries. The
sandbox supplies default workdir and numeric execution identity; explicit
evaluator identity overrides remain possible. Cancellation must terminate the
command without stopping the sandbox needed for evaluation. Transfer limits,
private artifacts, and missing-file semantics are part of the isolation boundary.
[Docker mechanisms](../implementation/docker.md) explain the current realization.

## Substitution and validation

A replacement must preserve task identity, isolation, live evaluation, command
semantics, bounded cancellation cleanup, and positive absence checks. Implement
provider-specific operations beneath the component policy and select them through
explicit composition. Supporting the interface alone does not establish support
for every bridge or benchmark.

Existing [sandbox unit tests](../../pkg/sandbox/sandbox_test.go) cover startup
rollback, ownership, stop retries, command defaults, transfers, and missing-file
classification. [Integration cases](../../pkg/sandbox/sandbox_integration_test.go)
cover the real Docker lifecycle and targeted cancellation;
[isolation cases](../../pkg/sandbox/isolation_integration_test.go) cover concurrent
occurrences. These are verification entry points, not evidence of a new test run.
