# Deployment and task environment

Deployment supplies execution mechanisms beneath the four Runner roles.
Components retain behavior and ownership. The contracts in
[`pkg/deployment`](../../pkg/deployment/deployment.go) distinguish runtime
operations, run-owned connectivity, and logical task attachments.

## Deployment operations

| Operations | Semantics |
| --- | --- |
| `Create(ctx, Request)` | Allocate a runtime and retain a known owned identity on partial failure. `ErrAllocationUnconfirmed` means absence or ownership remains unresolved. |
| `Validate(ctx, id, Request, secrets)` | Verify identity, ownership, isolation and absence of supplied secrets from metadata; do not retain secrets. |
| `UploadArchive`, `DownloadArchive` | Preserve tar modes and ownership; return source metadata for download policy. |
| `Start`, `Running` | Start a runtime and inspect its running state; application readiness belongs to the component. |
| `Exec`, `ExecStream` | Preserve exact argv and buffered/streamed I/O; cancellation confirms targeted process termination. |
| `Logs`, `LogsStream` | Retrieve bounded or streamed output. |
| `Address(ctx, id, port)` | Resolve a host-reachable private service address, without a scheme. |
| `ServiceRuntime.TaskAddress(ctx, id, port)` | Resolve a service address reachable by the harness on the shared attachment. |
| `Stop(ctx, id)` | Remove owned resources and positively confirm absence; safe to repeat. |
| `Close()` | Close a transport; this is not runtime removal. |

External operations accept `context.Context`. Owners use fresh bounded cleanup
contexts after cancellation and retain ownership following partial failure.
An empty create response alone does not prove absence. A download returns
`runner.ErrNotFound` only for an absent source in an existing runtime; missing
runtime and transfer failures remain distinguishable. Harness readiness verifies
effective permissions after private archive staging.

## TaskEnvironment operations and ownership

`RunEnvironment.Start` prepares shared connectivity and returns an opaque
`RuntimePlacement`. Its `NewTaskEnvironment` returns a fresh logical handle for
each live or evaluation sandbox. A handle borrows the run attachment; it never
removes the shared network or closes the run's deployment transport.

| Task operation | Contract |
| --- | --- |
| `Start(ctx, TaskEnvironmentRequest)` | Validate run identity, service declarations and unique runtime name; return placement and resolved service URLs. |
| `Validate(ctx)` | Confirm the borrowed attachment remains active and owned. |
| `Stop(ctx)` | Release this logical handle without affecting peer tasks. |

`RunEnvironment.Stop` removes only connectivity owned by the run, after all
runtimes are removed, and confirms absence. Network identity and effective policy
are recorded at run scope. Docker implements one randomly named shared network
with outbound access. `Environment.AllowNetwork` remains benchmark input metadata;
the effective policy is `shared-egress`. Cross-task connectivity is accepted in
this trusted research setup.

The sandbox receives `Options.NewEnvironment`, attaches its runtime, and exposes
`Connectivity() core.HarnessConnectivity`. Runner forwards it to the harness.
Fresh evaluation sandboxes get independent handles and names on the same network.
Task cleanup releases handles after sandbox removal. The run owns shared teardown.

## Endpoint handoff

Consumers receive addresses reachable from their own location. They do not
infer Docker DNS, inspect host interfaces, or assume that Runner's loopback is
reachable inside a harness container.

| Consumer | Supplied value | Resolution owner |
| --- | --- | --- |
| Runner | Bridge gRPC control address | Bridge deployment |
| Harness | Sandbox-specific SSH endpoint | Bridge deployment, using the actual listener port |
| Harness | Task service URLs such as `SearchURL` | Sandbox task environment |
| Bridge | Immutable sandbox execution binding and configured backend access | Execution-provider wiring and registration |

Each local listener reports its bound port. Deployment resolves the advertised
host and port, which may differ from the bind address. Provider substitution tests
exercise a DNS host and remapped port. Docker publishes control on host loopback
and returns a peer-reachable bridge address for each dynamic SSH listener. It
builds search URLs using each sandbox's unique runtime service name, avoiding a
shared fixed alias.

`RuntimePlacement` is opaque provider placement data, distinct from an endpoint
or public sandbox identifier. Providers own attachment, routing and publication;
a future provider can use existing connectivity without allocating a network per
task or run. Passing endpoints keeps these details outside protocol code, without
introducing an endpoint registry or a generic network orchestration layer.

An endpoint alone does not make a sandbox remotely executable. Current bridges
use injected Docker API execution against exact container IDs. Another provider
must also implement the execution and lifecycle contracts. This separation helps
future E2B or cluster adapters; neither E2B wire compatibility nor additional
deployment support is implemented.

## Why the contracts remain separate

Runtime and network ownership have different lifetimes. Tasks remove their own
runtimes and release handles; only the run removes shared connectivity. The bridge
owns temporary access and its service, while Runner/ToolSandbox own sandbox
start, evaluation and stop. Independent dependencies preserve these boundaries.

## Substitution and current limits

The supported composition is local Docker on one daemon. Bridge runtime placement
and sandbox execution are separately injected, but production supports Docker
for both. Backend endpoints and socket mounts come from provider wiring.

Deployment requests still include container-oriented resource settings, host
mounts, network aliases and image-volume policy. The sandbox assumes Linux,
`/bin/sleep`, absolute paths and numeric UID:GID execution. These are explicit
constraints an alternate adapter must satisfy or deliberately revise, not proof
of arbitrary provider portability. See [supported compositions](../supported.md)
and [Docker implementation](../implementation/docker.md).
