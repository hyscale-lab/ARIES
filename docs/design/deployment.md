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
| `Start(ctx, SandboxRequest) (string, error)` | Allocate the task attachment and return its identifier. Call `Stop` even if startup fails after allocation. |
| `Validate(ctx)` | Confirm that the attachment still satisfies its ownership and isolation requirements. |
| `BridgeListen(ctx)` | Resolve the listener bind address and harness destination for this exact task occurrence. |
| `Stop(ctx)` | Remove the owned attachment and positively confirm absence. |

The sandbox receives this owner through `Options.NewEnvironment`. It passes the
attachment to its deployment request; Runner passes it separately to the harness
as `HarnessRequest.Network`. Bridge credentials do not select the attachment.
Composition supplies bridge `ResolveListen` callbacks without making bridge tool
execution responsible for network discovery.

The sandbox owns attachment cleanup after runtime removal. Harnesses own their
own runtimes and deployment transports. Tool bridges own temporary access, not
the task attachment. Docker-specific network operations stay outside
`Deployment`; see [Docker resource management](../implementation/docker.md).

## Why the contracts remain separate

`Deployment` operates a runtime; `TaskEnvironment` owns the attachment shared
by one task's runtimes and resolves bridge connectivity. Today these correspond
to a Docker container and its task network. Their cleanup is ordered separately:
remove runtimes before removing the attachment. Combining them would tie runtime
operations to a particular attachment lifecycle.

Keeping separate dependencies allows a future sandbox implementation to combine
an execution service with independently managed task connectivity. It does not
by itself make a Docker harness interoperable with a Kubernetes sandbox; that
combination still needs explicit routing, identity, access, and cleanup semantics.

## Current attachment handoff

The live [`Sandbox`](../../pkg/runner/interfaces.go) contract requires
`NetworkName() string`. [Runner](../../pkg/runner/runner.go) forwards it directly
as `HarnessRequest.Network`. The [concrete sandbox](../../pkg/sandbox/sandbox.go)
returns the attachment created by `TaskEnvironment.Start` and retains ownership.
Current harnesses require a nonempty shared deployment network name; this
explicit contract does not establish support for heterogeneous deployments.

## Substitution and current limits

A provider must preserve ownership, private access, archive permissions,
cancellation behavior, error distinctions, and confirmed cleanup. It must also
support the concrete capabilities required by its consumer; a logical Benchmark
or embedded ToolBridge need not be deployed as a service.

**Current gap against full deployment independence:** the shared request still
exposes network attachment strings, network aliases, image-declared volume
policy, and container-oriented resource settings. The
[sandbox adapter](../../pkg/sandbox/sandbox.go) assumes a Linux environment with
`/bin/sleep`, absolute paths, and numeric UID:GID execution. These are current
contract constraints, not proof of arbitrary backend portability. Another backend
requires explicit decisions about these semantics, staging, execution identity,
and cancellation rather than silently weakening them.

Docker is the implemented provider. Kubernetes is a planned, unsupported target;
see the [support and roadmap reference](../supported.md). Current CLI selection
rejects it before model services, image preparation, or resource allocation in
[`cmd/aries/wiring.go`](../../cmd/aries/wiring.go).
