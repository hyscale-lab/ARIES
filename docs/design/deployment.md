# Deployment and task environment

Deployment infrastructure provides execution mechanisms beneath the four Runner
roles. Components retain their own behavior and policy. There is one shared
`Deployment` contract and a separate `TaskEnvironment` ownership contract in
[`pkg/deployment`](../../pkg/deployment/deployment.go); there is no deployment
interface per role.

## Deployment operations

| Operations | Semantics |
| --- | --- |
| `Create(ctx, Request) (string, error)` | Allocate a runtime and return its identity, including on partial failure when cleanup is required. |
| `Validate(ctx, id, Request, secrets)` | Confirm identity, ownership, isolation, and absence of supplied secrets from metadata before exposing the runtime; do not retain secrets. |
| `UploadArchive`, `DownloadArchive` | Transfer tar content with modes and ownership. Downloads return source size and mode for policy checks. |
| `Start`, `Running` | Start the allocated runtime and inspect whether it is running. Readiness beyond that is component policy. |
| `Exec`, `ExecStream` | Execute exact argv, with buffered or streamed I/O; cancellation must confirm targeted process termination. |
| `Logs`, `LogsStream` | Retrieve bounded or streamed runtime output. |
| `Address(ctx, id, port)` | Return a private service address reachable by the host, without a URL scheme. |
| `Stop(ctx, id)` | Remove owned resources; success requires positive confirmation of absence and repeated calls must be safe. |
| `Close()` | Close the deployment transport; this is not runtime removal. |

External operations accept `context.Context`. Owners must perform cancellation
cleanup with a fresh bounded context. A failed allocation can still own resources;
callers must retain returned identities and attempt cleanup. Failure to establish
absence is a cleanup failure, even when the transport has closed.

Archives carry permissions, but harness readiness must verify their effective
permissions inside the runtime. A download wraps `runner.ErrNotFound` only when
the source is absent in an existing runtime; loss of the runtime must remain a
different failure. Missing log runtimes also wrap `ErrNotFound`.

## TaskEnvironment operations and ownership

`TaskEnvironment` owns the attachment shared by task sandbox and harness. Each
occurrence, including retries or repeated task IDs, requires a fresh owner.

| Operation | Contract |
| --- | --- |
| `Start(ctx, SandboxRequest) (core.HarnessConnectivity, error)` | Validate service declarations before allocation, allocate the task attachment, and return placement with resolved service URLs. Call `Stop` even if startup fails after allocation. |
| `Validate(ctx)` | Confirm that the attachment still satisfies its ownership and isolation requirements. |
| `Stop(ctx)` | Remove the owned attachment and positively confirm absence. |

The sandbox receives this owner through `Options.NewEnvironment`. It passes the
attachment to its deployment request; Runner passes it separately to the harness
within `HarnessRequest.Connectivity`. Bridge credentials do not select the attachment.

The sandbox owns attachment cleanup after runtime removal. Harnesses own their
own runtimes and deployment transports. Tool bridges own temporary access, not
the task attachment. Docker-specific network operations stay outside
`Deployment`; see [Docker resource management](../implementation/docker.md).

## Why the contracts remain separate

`Deployment` operates a runtime; `TaskEnvironment` owns the attachment shared
by one task's runtimes. Docker uses containers
and a task network. Their cleanup is ordered separately:
remove runtimes before removing the attachment. Combining them would tie runtime
operations to a particular attachment lifecycle.

Keeping separate dependencies allows a future sandbox implementation to combine
an execution service with independently managed task connectivity. It does not
by itself establish heterogeneous deployment support; another composition still
needs explicit routing, identity, access, and cleanup semantics.

## Current attachment handoff

The live [`Sandbox`](../../pkg/runner/interfaces.go) contract requires
`Connectivity() core.HarnessConnectivity`. [Runner](../../pkg/runner/runner.go)
forwards that value as `HarnessRequest.Connectivity`.

The benchmark declares services in `Environment.Services` (currently a search
port). `TaskEnvironment.Start` returns the complete
`HarnessConnectivity{Placement, SearchURL}`. It rejects invalid service ports
before allocating the attachment. The sandbox still calls `Validate` to confirm
attachment ownership before creating its runtime, and retains environment
ownership through evaluation. Search-enabled harnesses reject missing or invalid
URLs; disabled search requires no endpoint.

`RuntimePlacement{DockerNetwork}` names the task-owned Docker network. Docker
validates and translates that field when creating runtimes. This preserves
current local Docker profiles without imposing Docker network syntax on harness
policy. Unsupported or missing placements fail explicitly. Separate profile
deployment blocks do not imply heterogeneous deployment support.

## Substitution and current limits

A provider must preserve ownership, private access, archive permissions,
cancellation behavior, error distinctions, and confirmed cleanup. It must also
support the concrete capabilities required by its consumer; a logical Benchmark
need not be deployed as a service. ToolBridge controllers own a separate native
bridge runtime through the lifecycle/transfer/addressing subset of Deployment.
Composition wiring supplies launch settings and runtime/measurement metadata;
controllers must not infer a deployment method from the sandbox backend or inject
Docker launch defaults. Docker is currently the only supported composition.
Backend-specific execution and process cleanup live in the provider package;
the shared deployment package holds contracts. Docker's sandbox process cleanup
uses Linux `/proc` identities to preserve the pre-assignment baseline and remove
detached agent commands before evaluation.

**Current gap against full deployment independence:** the shared request still
exposes a Docker network field, a trusted bridge daemon mount, network aliases,
image-declared volume policy, and container-oriented resource settings. The
[sandbox adapter](../../pkg/sandbox/sandbox.go) assumes a Linux environment with
`/bin/sleep`, absolute paths, and numeric UID:GID execution. These are current
contract constraints, not proof of arbitrary backend portability. Another backend
requires explicit decisions about these semantics, staging, execution identity,
and cancellation rather than silently weakening them.

Docker bridge runtimes are implemented. Runtime identity and ownership remain
bound through cleanup. Private staging preserves evidence after
the bridge application exits until the controller removes the runtime. See
[supported compositions](../supported.md).
