# vHive microVM sandboxes

[`pkg/deployment/vhive`](../../pkg/deployment/vhive/vhive.go) implements tool
sandbox deployment using vHive's Firecracker/containerd stack directly. Each
task occurrence and each fresh evaluation sandbox owns a separate VM and image
snapshot. OpenClaw or Hermes and the managed SSH bridge remain Docker containers
on one local Docker daemon and one run network. Runner runs on that same worker.

This integration uses native containerd guest APIs for execution and tar streams,
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
  Persistent mode mounts the execution socket and FIFO directory into the bridge.
  Checkpoint mode adds the control socket and directories described below.
  The guest receives none of these host mounts.

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
and logical running state before granting borrowed execution. Persistent mode
needs only containerd execution access. Checkpoint mode also needs VM control
access to suspend and restore the borrowed target. Session revocation precedes
verifier injection as on other deployments.

In persistent mode, cleanup first kills all processes in the owned guest
container and deletes the task through containerd's native `WithProcessKill` path. This releases outstanding
agent `Wait` calls before native `StopVM`, which waits for the VM shim to exit.
It then removes container metadata and the writable snapshot, and confirms
container absence. A persistent stopped marker lets snapshot cleanup retry
without trying to stop the VM again. Removal failures retain ownership.
Ambiguous VM-control failures remain cleanup errors: the native API can return
an untyped socket error after a lost response, and ARIES does not infer VM
absence from that error. Inspect the worker before manually removing retained
metadata. Containerd's cached image content is shared and is not task-owned.

## Suspend and resume

Set `sandbox.deployment.vhive.idle_mode` to `persistent` or `checkpoint`.
Omission selects `persistent`, which keeps each guest running through task
execution and evaluation. In `checkpoint` mode, a completed bridge command
parks the VM once concurrent operations have finished. ARIES pauses the guest,
saves its memory and VMM state to disk, and removes the VMM. The next command
restores that checkpoint before execution. The logical sandbox ID, guest boot,
process memory and writable filesystem remain the same. Background processes
are suspended with the VM and continue after restore; they do not run while the
VM is on disk.

Checkpoints live under `checkpoint_dir`, defaulting to
`/var/lib/firecracker-containerd/aries-checkpoints`, separated by containerd
namespace and sandbox ID. Use disk storage with sufficient room for guest memory;
checkpoint files contain private task state. The writable devmapper device stays
allocated while suspended. Network-enabled sandboxes also retain their original
CNI allocation, namespace, tap device, MAC and IP across VMM removal. Task cleanup
removes these owned allocations and checkpoint files. A checkpoint is temporary
state for the current sandbox, rather than a portable image or a new evaluation
sandbox.

Set `shim_dir` to the worker runtime's `shim_base_dir`; its default is
`/var/lib/firecracker-containerd/shim-base`. Checkpoint mode mounts that directory,
`checkpoint_dir` and the adjacent containerd `.ttrpc` control socket into the
Docker bridge at their host paths. Keep these paths consistent between Runner,
daemon and bridge. The checkpoint bridge also uses the host PID namespace so it
can confirm removal of the exact VMM and shim processes. Persistent mode retains
its smaller execution socket and FIFO mounts.

Stopping a paused VMM can produce the native force-termination error even when
termination succeeds. ARIES does not treat that error text as proof of absence.
Before stopping, it identifies the VMM through credentials on its Unix socket
and records its parent shim identity from host process metadata. It confirms
those exact processes have exited before marking the checkpoint suspended; an
unconfirmed exit retains ownership and reports an error.

The native snapshot loader restores the guest agent's process state, but does
not rebuild the host shim's task table. Checkpoint execution therefore calls the
native guest task API over vsock directly, preserving argv, streams and process
exit semantics after restore. Concurrent commands share access to the running
VM; checkpointing waits until they release it. Runner preparation and evaluation
operations restore the sandbox when needed without parking after every file
transfer. A restore or checkpoint error remains an operation error and retains
ownership for cleanup; an ambiguous command submission is never retried.

## Latency breakdown

The bridge's `tool-calls.jsonl` records the request's UTC receipt, monotonic
stage offsets/durations and observed milestones. See
[command latency](../run-results.md#command-latency) for the schema. Use
`command.completed` for arrival through guest exit and output drainage, and
`bridge.response_sent` for completion of the bridge's local response send.
Checkpointing remains on the synchronous final-response path; the measurements
separate that cost so its possible overlap with a later LLM turn can be assessed.

The main spans are:

| Stages | Work measured |
| --- | --- |
| `bridge.prepare`, `bridge.accept_reply` | Decode/adapt the native command and accept the SSH request before provider dispatch. |
| `runtime.acquire`, `runtime.lock.shared`, `runtime.lock.exclusive` | Read retained state and wait for competing execution, checkpoint or restore operations. |
| `restore.total` and its child spans | Validate retained state, then confirm previous process absence, prepare devmapper mounts, persist the restoring state, connect control, call `restore.create_vm`, and persist running state. The total starts after state validation. |
| `command.total` and its child spans | Load container/spec, prepare the OCI process, resolve VM/vsock information, connect the agent and streams, submit exec/start, wait for exit, drain output and delete the completed process. |
| `checkpoint.total` and its child spans | Read state, inspect the task and VMM/shim identity, publish state transitions, pause, create the snapshot, sync files, stop the VM, confirm absence, and remove old snapshots. |
| `bridge.send_exit_status`, `bridge.close_channel` | Send final completion and close the SSH channel after the backend returns. |

Before guest execution, inspect `restore.prepare_mounts`,
`restore.persist_restoring`, `restore.connect_control`, `restore.create_vm`,
`command.load_container`, `command.load_spec`, `command.vm_info`,
`command.connect_agent`, the stream connection spans, `command.exec_rpc` and
`command.start_rpc`. A fast Firecracker snapshot-load event alone does not
account for these calls or lock waits. `restore.requested` marks submission of
CreateVM to the worker; its offset exposes the elapsed time before that RPC.
`restore.total` appears only when restoration is needed. `runtime.ready` marks
completion of VM acquisition, before command setup. `command.started`, `command.exited` and
`command.completed` distinguish guest runtime from transport/output drainage;
start and exit are observations of native Start acknowledgement and Wait completion,
rather than guest CPU timestamps.
Completed-process deletion follows `command.completed`. Persistent mode has no
restore/checkpoint work. Concurrent calls can produce `checkpoint.skipped_busy`
when another operation retains the VM, leaving checkpointing to the last caller.
Do not sum parent spans and their children, or concurrent stream spans.

The earlier two-command mock smoke illustrates why the boundaries matter. Its
final tool record's `timestamp` was `13:25:08.746` UTC, after execution and
checkpointing; its 18.181-second duration placed execution entry near
`13:24:50.565`. The worker received mount preparation at `13:24:50.565` and
CreateVM at `13:24:50.567`, roughly 2 ms later. Snapshot creation took about
11.93 seconds and native stop about 5.01 seconds. The worker's next restore at
`13:25:15.216` belonged to evaluation after harness stop and bridge revocation.
The apparent seven-second interval was therefore between completed tool work
and evaluation, rather than a measured pre-restore delay for that tool. These
are historical run measurements; new runs should use the explicit receipt and
completion observations.

An instrumented two-command mock run on the smoke worker with a 2 GiB guest
measured the following for its second diagnostic command:

| Observation | Time |
| --- | ---: |
| Receipt to restore RPC submission | 2.27 ms |
| Prepare devmapper mounts (part of the interval above) | 0.75 ms |
| Native `CreateVM` restore RPC | 139.58 ms |
| Command setup after the VM is ready | 286.46 ms |
| Start acknowledgement to observed process exit | 9.28 ms |
| Receipt to command exit and drained output | 439.01 ms |
| Checkpoint work after command completion | 17,414.46 ms |
| Receipt to response sent | 17,854.83 ms |

The setup spans attributed about 101 ms to connecting the guest agent, 113 ms to
the exec RPC with concurrent stream setup, and 70 ms to the start RPC. The pinned
SDK waits for its first 100 ms retry tick before attempting a vsock connection;
agent and stream connections therefore show separate waits even for a ready VM.
The checkpoint included about 11.88 s creating the snapshot, 5.01 s stopping the
VM/shim, and 0.41 s removing the old snapshot. These are measurements of one
worker and command, not fixed backend costs. The command succeeded, and the run
confirmed isolation and task/shared cleanup; diagnostic commands left the Git
benchmark reward at zero.

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
10 GiB devmapper device). Set `ARIES_VHIVE_IDLE_MODE=checkpoint`,
`ARIES_VHIVE_CHECKPOINT_DIR` and, when needed, `ARIES_VHIVE_SHIM_DIR` to exercise
the lifecycle test with an explicit suspend/restore after archive upload. The
test covers guest I/O, archive permissions, nonzero exits, cancellation with
commands surviving, borrowed-target validation, fresh evaluation state and
removal.

The dedicated checkpoint test uses a 10 GiB devmapper device and requires a
checkpoint directory:

```sh
ARIES_VHIVE_INTEGRATION=1 \
ARIES_VHIVE_IMAGE=docker.io/library/alpine:3.21.3 \
ARIES_VHIVE_CHECKPOINT_DIR=/var/lib/firecracker-containerd/aries-checkpoints \
go test -tags=integration -run TestMicroVMCheckpointIntegration -v ./pkg/deployment/vhive
```

It accepts the socket, namespace, IO directory and shim directory settings above.
To include networking, set `ARIES_VHIVE_CNI_NETWORK`, and optionally
`ARIES_VHIVE_CNI_BIN_PATHS`, `ARIES_VHIVE_CNI_CONF_DIR` and
`ARIES_VHIVE_CNI_CACHE_DIR`. The binary path list uses the host's path-list
separator (`:` on Linux). Without a CNI network the test creates a network-disabled
sandbox. It checks three checkpoint/removal/restore cycles, private nonempty
checkpoint files, VMM/shim absence while suspended, the same guest boot ID and
background PID after restore, preserved disk content, stable interface addresses
and routes, and final removal of checkpoint files.

These native tests passed on the smoke worker in checkpoint mode, including the
dedicated test with and without CNI networking. They do not contact a model API.
Without the opt-in variable they skip; a skip is not execution evidence.

An OpenClaw end-to-end smoke run also passed using the mock LLM's
[two-command checkpoint example](mock-llm.md): both diagnostic tools completed
with exit zero, and the restored guest retained its boot ID, live background
process, file and network identity. HTTPS requests succeeded before and after
restore. Harness execution, isolation checks, task cleanup and shared cleanup
succeeded; the benchmark verifier ran and returned zero reward because the
diagnostic commands did not fix the benchmark task. Use `-tool-yield-ms 300000`
for this smoke run: checkpoint latency can exceed OpenClaw's default ten-second
yield, causing its native tool response to report that the command is still
running before the diagnostic result is available.
