# vHive microVM sandboxes

[`pkg/deployment/vhive`](../../pkg/deployment/vhive/vhive.go) implements tool
sandbox deployment using vHive's Firecracker/containerd stack directly. Each
task occurrence and each fresh evaluation sandbox owns a separate VM and image
snapshot. OpenClaw or Hermes and the managed SSH bridge remain Docker containers
on one local Docker daemon and one run network. Runner runs on that same worker.

This integration uses native containerd APIs for guest execution and tar streams,
and the native Firecracker control API for VM lifecycle. It does not deploy
Knative functions. vHive's Knative CRI exec forwards to a host-side placeholder,
so that endpoint cannot provide guest tool execution. See upstream
[CRI exec](https://github.com/vhive-serverless/vHive/blob/main/cri/proxy.go) and
[placeholder allocation](https://github.com/vhive-serverless/vHive/blob/main/cri/firecracker/service.go).

## Prepare the worker

Use a dedicated Linux worker with KVM, Docker, devmapper and the vHive fork of
firecracker-containerd. ARIES pins that fork at
[`bb007a1ef569`](https://github.com/vhive-serverless/firecracker-containerd/tree/bb007a1ef5692c0e30d8343b20d17b93ab7596f5)
in `go.mod`. Use the matching daemon, shim, guest kernel and guest root image.
Upstream [vHive worker setup](https://github.com/vhive-serverless/vHive/blob/main/docs/quickstart_guide.md)
and [native stack setup](https://github.com/vhive-serverless/firecracker-containerd/blob/bb007a1ef569/docs/getting-started.md)
describe installing those components. The Knative control plane and vHive CRI
daemon are not required for ARIES.

A verified smoke stack uses the pinned daemon/shim with Firecracker `1.13.1`,
vHive's guest root image, and the published
[Firecracker CI kernel 5.10.239](https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.13/x86_64/vmlinux-5.10.239).
Configure `kernel_image_path` in the worker's runtime JSON and include `acpi=off`
with its normal overlay-init boot arguments. The kernel named `vmlinux-5.10.186`
in the tested vHive asset bundle reported `5.10.0` and failed device probing on
the smoke worker; it was not used for the successful run.

The worker must provide:

- The containerd socket at `/run/firecracker-containerd/containerd.sock` and
  its Firecracker control socket at the same path plus `.ttrpc`.
- A configured devmapper thin pool. Its `base_image_size` must exactly match
  each task's nonzero `storage_mb`. ARIES reads the native snapshot device's
  capacity with `BLKGETSIZE64` before booting. A worker sized for 10 GiB tasks
  cannot run a task requesting 20 GiB without reconfiguration.
- A runner with permissions for the sockets, devmapper device inspection, and
  containerd image-config filesystem mounts. Native Linux mounting generally
  requires root privileges. These are worker setup requirements, not permissions
  granted to the agent or guest.
- An exec FIFO directory, by default `/run/firecracker-containerd/aries-io`,
  writable by Runner and accessible by the daemon. Wiring creates it before
  starting the bridge and mounts it at the same path into that container.
  Only the execution socket and FIFO directory are mounted into the bridge;
  the guest receives neither.

For network-enabled tasks, install a CNI configuration and its plugins, including
`tc-redirect-tap`. The upstream
[`fcnet.conflist`](https://github.com/vhive-serverless/firecracker-containerd/blob/bb007a1ef569/tools/demo/fcnet.conflist)
is an example; choose a subnet that does not overlap the worker's other networks.
Set `cni_network` to its network name. The shim must be able to find that
configuration and its binaries through its native CNI configuration paths.
The profile's optional `cni_bin_paths`, `cni_conf_dir` and `cni_cache_dir` pass
those paths directly to the native API, allowing a separate experiment setup.
Use DNS resolvers reachable from the guest; a host resolver bound only to
loopback is not reachable through the guest's loopback address.

Keep the shim's `default_network_interfaces` absent or empty. For a task whose
benchmark sets `allow_network: false`, ARIES supplies no NIC and verifies inside
the guest that only loopback exists before exposing access. An unexpected
default interface fails validation. Tasks allowing network access get the
explicit configured CNI interface. That network's routing and peer-access
policy come from the operator's CNI configuration; ARIES does not install a
firewall. The run records `benchmark-controlled-cni` as its network policy.

## Run

Start with either checked-in profile:

```sh
make build
./bin/aries setup profiles/vhive/openclaw-tb2-fix-git-deepseek-vhive.json
./bin/aries profiles/vhive/openclaw-tb2-fix-git-deepseek-vhive.json
```

The run needs the usual model credentials and uses that API. The Hermes example
is `profiles/vhive/hermes-tb2-fix-git-deepseek-vhive.json`. Profile fields and
defaults are in the [configuration reference](../configuration.md#vhive-deployment).

Image preparation stays on the selected Docker daemon. At allocation, wiring
exports the prepared task image through Moby; the provider imports it into the
configured containerd namespace and unpacks it with devmapper. This also handles
locally built benchmark images. The import is refreshed for each allocation,
including repeated image tags; image content may be reused by containerd, but
each writable snapshot is fresh. `aries setup` prepares images and data without
booting VMs or contacting the model API.

## Execution and ownership

Allocation first creates persistent containerd ownership metadata, then boots
the VM and starts the sandbox's idle process. Guest exec needs a running task,
so the idle `/bin/sleep infinity` starts during `Create`; `Start` confirms it is
running. This provider deploys sandboxes only. It cannot start harness or bridge
processes, and it does not provide Docker's injected init process.

Exec preserves argv, stdin, stdout, stderr, exit status, workdir and numeric
UID:GID overrides. It uses native guest process settings, so user switching does
not need `setpriv`. Explicit command environment settings override image values.
Cancellation closes local transport and FIFOs while allowing submitted guest
commands to continue; runtime removal owns termination. Tool output retains the
normal deadlines and output bounds. File transfers use guest `tar` and `stat`;
images must provide these, `/bin/sh` and `/bin/sleep`.

The bridge validates the exact runtime, guest VM binding, component, run, task
and running state before granting borrowed execution. It has no VM control
socket. Session revocation precedes verifier injection as on other deployments.

Cleanup first kills all processes in the owned guest container and deletes the
task through containerd's native `WithProcessKill` path. This releases outstanding
agent `Wait` calls before native `StopVM`, which waits for the VM shim to exit.
It then removes container metadata and the writable snapshot, and confirms
container absence. A persistent stopped marker lets snapshot cleanup retry
without trying to stop the VM again. Removal failures retain ownership.
Ambiguous VM-control failures remain cleanup errors: the native API can return
an untyped socket error after a lost response, and ARIES does not infer VM
absence from that error. Inspect the worker before manually removing retained
metadata. Containerd's cached image content is shared and is not task-owned.

## Limits and verification

Terminal-Bench 2, SWE-Atlas QA and SWE-bench Pro fit the sandbox API when their
images, requested disk capacity and network policy meet the requirements above.
Each combination still requires validation on its target worker. GPUs, host
mounts, network aliases, published services, remote workers and remote
snapshotters are unsupported. Deep Research Bench and Toolathlon are rejected
because their in-sandbox services need address routing this backend does not
implement.

A VM always has finite hardware capacity. Omitted task CPU and memory limits use
the configured default VM capacity; explicit limits select vCPU count and guest
memory. Task CPU requests must be in `0.01..32`; fractional requests additionally
set the guest process quota. Guest memory includes kernel overhead.
Nonzero disk limits are enforced by checking
the fixed devmapper capacity, rather than treating the setting as metadata.

Occurrence resource observation is explicitly reported as unsupported, including
its Docker harness: the current recorder has a capability status for the whole
source. Shared bridge observation continues using Docker stats. Do not interpret
the missing VM measurements as zero consumption. Sandbox idle-process logs are
empty; tool output is captured by the normal bridge and exec evidence.

Unit tests cover native execution semantics, output limits, cancellation,
ownership checks and cleanup failures without KVM or paid APIs. The real-worker
test is opt-in. Pre-import an image providing the utilities above into the
configured containerd namespace, then run:

```sh
ARIES_VHIVE_INTEGRATION=1 \
ARIES_VHIVE_IMAGE=docker.io/library/alpine:3.21.3 \
go test -tags=integration -run TestMicroVMLifecycleIntegration -v ./pkg/deployment/vhive
```

Optional test settings are `ARIES_VHIVE_SOCKET`, `ARIES_VHIVE_NAMESPACE`,
`ARIES_VHIVE_IO_DIR` and `ARIES_VHIVE_STORAGE_MB` (default `10240`, matching a
10 GiB devmapper device). The test boots real VMs, checks storage capacity and network-disabled validation,
guest I/O, archive permissions, cancellation, borrowed-target validation, fresh
evaluation state and removal. It does not contact a model API. Without the
opt-in variable it skips; a skip is not execution evidence.
