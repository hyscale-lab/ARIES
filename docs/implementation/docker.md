# Docker resource management

The [deployment contracts](../design/deployment.md) separate component policy,
runtime operations and shared connectivity. [`pkg/deployment/docker`](../../pkg/deployment/docker/docker.go)
uses the Moby Go SDK for runtime creation, validation, archives, execution, logs,
network operations and confirmed removal.

## Composition and topology

Explicit command switches select Docker clients for harness, sandbox and bridge;
all use the same local Unix socket. Configuration translation and execution
access live in [deployment wiring](../../internal/app/wiring/deployment/docker.go)
and [bridge wiring](../../internal/app/wiring/deployment/bridge.go).

Each run owns one randomly named, labeled Docker bridge network with
`Internal:false`. All harnesses, live/evaluation sandboxes and the shared bridge
container join it. Task handles borrow the network. Unique runtime names identify
sandbox services, including separate sandboxes for repeated task IDs. Effective
`shared-egress` policy is recorded alongside preserved benchmark environment
metadata; `AllowNetwork` does not create per-task isolated networks.

One managed bridge container serves the run, with a dynamic SSH listener per live
sandbox. gRPC control is plaintext and unauthenticated, published on host loopback.
SSH clients also connect without authentication; one in-memory service host key
is retained for the protocol. Neither client keys nor known-hosts files are staged.

Composition supplies the bridge's Docker socket as an explicit mount. Deployment
validates the declared source, destination and permissions; component labels do
not grant mounts. Harness and sandbox runtimes do not receive the socket. See
[deployment configuration](../configuration.md#deployment-configuration).

## Image preparation

Hermes plugin images and bridge images share the Docker
[build implementation](../../pkg/deployment/docker/build.go): explicit build
contexts, client setup, build arguments, progress/error handling, and image
confirmation. Hermes supplies only its Dockerfile and reuses its derived image
tag, which includes the base, plugin pin, and recipe. The
[bridge builder](../../pkg/deployment/docker/build_bridge.go) also supplies the
bounded server executable and requires a matching content label before reusing
an image. Changes to the executable, recipe, or build arguments trigger a rebuild.
Neither build context includes other files from the working directory.

## Runtime creation and execution

The sandbox supplies ownership labels, unique runtime name, default workdir,
numeric execution user and private evidence paths. Docker validates those values
and resolves addresses. `Address` requires exactly one published loopback binding;
`TaskAddress` resolves the bridge's peer-reachable address using its actual dynamic
listener port. Protocol code does not probe interfaces or assume SSH port 2222.
Search URLs use unique sandbox runtime names. Provider-specific address inspection
stays in Docker, as described in [endpoint handoff](../design/deployment.md#endpoint-handoff).

[Streaming execution](../../pkg/deployment/docker/exec.go) preserves argv and
confirms targeted process-group termination on command cancellation without
stopping the sandbox. The sandbox applies agent defaults; explicit evaluator root
commands retain their requested execution identity.

Archive transport returns source metadata for bounded, regular-file checks.
The sandbox confines downloads to its output root, validates tar content, and
publishes private files atomically. Missing-source errors require an existing
runtime. Harnesses check effective staged file permissions during readiness.

## Removal and observation

Container cleanup inspects, stops or kills as needed, removes the runtime and
anonymous volumes, then confirms absence. Network removal also confirms absence.
Lost allocation responses are recovered through generated names and verified
ownership. Unresolved allocation returns `deployment.ErrAllocationUnconfirmed`;
cleanup retains that uncertainty instead of treating an empty ID as success.

Task revocation closes only its bridge listener and handlers and collects evidence.
Benchmark services and background work remain intact for evaluation. Task cleanup
removes harness/sandbox runtimes and releases logical attachment handles. Once all
tasks finish, run cleanup removes the shared bridge, finalizes observation, removes
the shared network and closes transports. Failures remain in the persisted run
infrastructure result.

The bridge resource source is selected from `bridge.deployment`, independently of
the sandbox source. Run-scoped discovery selects the shared bridge by run ownership
without inventing a task ID. Task sources select their harness and live/evaluation
sandboxes and exclude the bridge. Confirmed runtime disappearance during sampling
is skipped; genuine measurement failures remain visible. The common recorder
calculates rates and writes [resource evidence](../run-results.md).

[Provider tests](../../pkg/deployment/docker/docker_test.go),
[allocation recovery](../../pkg/deployment/docker/create_recovery_test.go),
[shared environment tests](../../pkg/deployment/docker/environment_test.go), and
[stream cancellation](../../pkg/deployment/docker/stream_test.go) cover these
boundaries. Additional providers and remote Docker remain unsupported.
