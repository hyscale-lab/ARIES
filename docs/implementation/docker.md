# Docker resource management

The [deployment contracts](../design/deployment.md) separate component policy from
execution and task attachment ownership. Their current Docker provider is
[`pkg/deployment/docker`](../../pkg/deployment/docker/docker.go), which uses the
Moby Go SDK for creation, validation, archive transport, streaming execution,
logs, and positive removal. ARIES does not shell out to Docker for these operations.

## Composition and topology

Explicit command switches select separate Docker clients for
`harness.deployment` and `sandbox.deployment`; both must name the same local Unix
socket. Image preparation and resource sampling use that selected daemon too.
Configuration translation lives
in [deployment wiring](../../internal/app/wiring/deployment/docker.go).
See [deployment configuration](../configuration.md#deployment-configuration) for
the profile shape and compatibility defaults.

Network operations are outside `Deployment`. Wiring supplies the Docker
task-environment constructor as `sandbox.Options.NewEnvironment`. Each occurrence
gets a fresh owner, including duplicate task IDs and retries. The
[task environment](../../pkg/deployment/docker/environment.go) creates and
validates a randomly named, labeled bridge network. `AllowNetwork` controls
whether Docker creates it as an internal network. Ownership checks prevent a
failed create response from authorizing cleanup of an unrelated resource.

The bridge container joins the task-owned network. Its SSH endpoint uses its
address on that network and an explicit service port; the private control endpoint
is published on loopback for Runner. Attachment and endpoint ownership follow the
[task-environment contract](../design/deployment.md#taskenvironment-operations-and-ownership).

`bridge.mode` is `managed`: native listeners run in a separate Docker container.
Explicit embedded/external modes are rejected. Bridge containers receive only
the explicit trusted infrastructure socket attachment needed to reach the exact
sandbox. See [supported combinations](../supported.md) for deployment boundaries.

## Runtime creation and execution

The sandbox chooses task labels, default workdir and numeric execution identity,
and private artifact paths. Docker translates deployment requests into container
configuration, validates ownership and isolation, and publishes requested services
only on loopback. `Address` checks for one loopback binding.

The common [execution implementation](../../pkg/deployment/docker/exec.go)
supports streaming input/output and confirms command process-group termination
after cancellation without stopping the task runtime. Commands preserve their
argv boundaries. The sandbox applies task defaults; explicit evaluator root
commands override the agent UID.

Archive transport returns source metadata so the sandbox can reject oversized or
non-regular downloads before reading payloads. The sandbox checks destination
paths against the run output root, checks archive sizes, and publishes private
files atomically. Missing-source errors require evidence that the deployment
still exists. This policy lives in [`pkg/sandbox`](../../pkg/sandbox/sandbox.go),
while Docker owns tar transfer through Moby.

## Removal and failure recovery

The [sandbox lifecycle contract](../design/sandbox.md#lifecycle-isolation-and-failure)
defines cleanup ordering and partial-start obligations.

Container cleanup inspects, stops or kills as needed, removes the container and
its anonymous volumes, and inspects again to confirm absence. Earlier lifecycle
errors may be logged as recovered only after that final absence confirmation.
Network cleanup also confirms absence. Lost allocation responses are recovered
through generated names and verified ownership before removing anything.

Bridge revocation happens earlier, while the sandbox must remain available for
evaluation. [Sandbox process cleanup](../../pkg/deployment/docker/sandbox_processes.go)
records PID/start-time identities before access is granted, then removes newly
created processes after native sessions stop. It preserves existing benchmark
processes and refuses to confirm cleanup when termination is uncertain. The
[live regression](../../pkg/deployment/docker/sandbox_processes_integration_test.go)
checks detached-process termination, baseline preservation, and repeated cleanup.

The [provider tests](../../pkg/deployment/docker/docker_test.go),
[allocation recovery tests](../../pkg/deployment/docker/create_recovery_test.go),
[task environment tests](../../pkg/deployment/docker/environment_test.go), and
[stream cancellation tests](../../pkg/deployment/docker/stream_test.go) record
these intended behaviors. The [deployment design](../design/deployment.md#substitution-and-current-limits)
records portability gaps; this page does not claim support for other providers.
