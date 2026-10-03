# Kubernetes backend

ARIES can run its task sandbox, its OpenClaw and Hermes agent harnesses, and
its SSH tool bridges on Kubernetes instead of Docker. The runner itself stays a
process on a host outside the cluster. This document describes the design, the
configuration surface, the deployment, and what has been verified. Operating
instructions — building a cluster, deploying the charts, starting a run — are in
[k8s/README.md](../../k8s/README.md).

The Kubernetes path is selected entirely through profile configuration; no code
paths are hard-wired. The Docker backends are untouched and remain the default.

Every role that touches the cluster drives it through the **`kubectl` binary**
rather than a `client-go` dependency. The binary and its resolved
kubeconfig/context are the entire cluster contract, which keeps the Kubernetes
backends as substitutable as the Docker ones and keeps an API-machinery
dependency tree out of a benchmark runner.

For a chronological walkthrough of one run — from `kubectl apply` through the
per-task lifecycle to teardown — see the [Kubernetes run flow](kubernetes-flow.md).

## Motivation

The Docker backends assume a local Docker Engine: the sandbox creates
containers through the Moby SDK, and the bridge/harness reach them over a local
Docker network. To study agent serving on cluster infrastructure — and to run
where no Docker socket is available — ARIES needs to place the task environment
and the agent in Kubernetes pods while preserving the four-role lifecycle and
its isolation gates.

## Architecture

The Kubernetes backend keeps the Runner's four roles and their ordering. Only
the concrete substrate changes: the task environment and the agent become pods,
and access between them is arranged over the cluster network.

```
                 ARIES runner (process on a host outside the cluster)
                 ├── ToolSandbox (kubernetes) ── kubectl ──► task pod
                 ├── ToolBridge  (remote)     ── kubectl exec ──► aries-bridge pod
                 └── AgentHarness (kubernetes) ── kubectl ──► agent pod (+ Service)
                                                              port-forward ──► gateway

   agent pod ──SSH──► aries-bridge pod ──kubectl exec──► task pod
```

- **Sandbox** (`pkg/sandbox/kubernetes`): one pod per task. Commands run via
  `kubectl exec`; files move via `kubectl exec`/`cp`. Satisfies the same Go
  interfaces the bridge type-asserts, so it is a drop-in for the Docker sandbox.
- **Bridge** (`pkg/bridge/openclawssh`, `pkg/bridge/hermesssh`): the existing
  SSH bridges, extended with an *advertise host* so the endpoint and
  `known_hosts` reference a cluster-reachable address rather than a Docker
  network gateway. With `bridge.deployment: "kubernetes"` they run inside the
  long-running `aries-bridge` pod rather than the runner (`pkg/bridge/remote`).
- **Harness** (`pkg/harness/openclaw` `KubeManager`, `pkg/harness/hermes`
  `NewKube`): one agent pod per task. ARIES stages the private runtime into the
  pod. OpenClaw also gets a per-task Service, and ARIES reaches its gateway
  through a `kubectl port-forward`; Hermes runs as a one-shot `kubectl exec`.

The agent's reverse hop — agent pod → SSH bridge — is the crux. It only works
when the bridge's advertised address is reachable from the agent pod, which is
what the deployment models below arrange.

## Decoupling the bridge from Docker types

The OpenClaw bridges type-assert the live sandbox to method sets that returned
concrete Docker types (`FileInfo`, and a `ProcessRef` with unexported fields).
A non-Docker sandbox could not construct those. The neutral package
`pkg/sandbox` now holds:

- `sandbox.FileInfo` — the filesystem metadata surface.
- `sandbox.ProcessRef{PID int; Handle any}` — an attached-process identity whose
  `Handle` carries backend-specific bookkeeping opaque to the bridge; each
  backend type-asserts it back to its own concrete handle.

Both the Docker and Kubernetes sandboxes return these neutral types, and the
bridge depends only on `pkg/sandbox`. This is what makes the sandbox genuinely
substitutable.

## The Kubernetes sandbox

`pkg/sandbox/kubernetes` implements `runner.ToolSandbox`/`runner.Sandbox` plus
the filesystem and attached-process surface the OpenClaw bridges require. It
drives the cluster with the `kubectl` binary rather than a client-go dependency.

- **Lifecycle** — `Start` renders a Pod manifest (image, workdir, resource
  limits from the environment, ownership labels; long run/task IDs are stored as
  **annotations** to respect the 63-byte label limit) and `kubectl apply`s it,
  then waits for `condition=Ready`. `Stop` deletes the pod and positively
  confirms its absence.
- **Processes** — `Exec`/`ExecStream`/`ExecProcessStream` run over `kubectl
  exec`. The attached form uses an early-PID handshake: a wrapper writes the
  child PID to a file and then `exec`s the real command, so stdout/stderr stay
  clean and `onStart` fires before completion. `SendProcessSignal`/
  `TerminateProcess` deliver signals with in-pod `kill`.
- **Filesystem** — `ReadFile`/`WriteFile`/`StatPath`/`ListDir`/`MakeDir`/
  `RemovePath`/`MovePath`/`Upload`/`Download` via `kubectl exec` (and `kubectl
  cp` for uploads/downloads), with the same absolute/normalized path validation
  the bridge expects.
- **Compile-time guards** (`assertions.go`) mirror the bridges' unexported
  interfaces so any signature drift in a sandbox method is a build failure
  rather than a runtime "sandbox does not support …".

The task image comes from each Terminal-Bench task's own `task.toml`, and its
CPU/memory limits are translated into container resources. Output per exec is
capped at 16 MiB; readiness and cleanup default to 120 s and 60 s.

### Network isolation

Every task pod gets a `NetworkPolicy` selecting exactly itself, created before
the pod and deleted after it. A task's `allow_internet` decides what that policy
*permits*, never whether it exists:

| `allow_internet` | Ingress | Egress |
| --- | --- | --- |
| false | denied | denied — the air-gapped equivalent of Docker's `Internal: true` |
| true | denied | internet yes; pod network, Services and API server no |

Docker supplies two separate guarantees that are easy to conflate. `Internal`
controls whether a task reaches the internet, while the fact that each task gets
*its own network* is what stops two concurrent tasks reaching each other. A
cluster has one flat pod network, so both have to be written down here. An
earlier version created no policy at all when `allow_internet` was true, which
left concurrent task pods mutually reachable — and since `fix-git` sets
`allow_internet = true`, that was the only case the benchmark exercised.

Denying ingress is the load-bearing half. A pod that accepts no connections
cannot be reached by a peer whatever that peer may send, so inter-task
isolation does not depend on `sandbox.pod_cidr` / `sandbox.service_cidr` being
set correctly. Getting those wrong widens outbound reach; it never lets tasks
talk to each other. Isolation costs nothing functionally, because the sandbox is
driven entirely by `kubectl exec` — API server to kubelet, over the host
network — so a pod with zero connectivity is still fully usable.

Three details are easy to get wrong:

- **DNS is permitted by selecting the CoreDNS pods**, not the `kube-dns` Service
  IP. kube-proxy DNATs Service addresses before egress policy is evaluated, so a
  rule naming the Service address never matches and the cluster-CIDR exclusion
  then drops the resolved pod IP — breaking name resolution while raw-IP traffic
  still works.
- **The policy is created before the pod.** One created after leaves a window in
  which the container runs unisolated, and that window is exactly when an
  image's entrypoint makes its network calls.
- **Deny-all is a direction named in `policyTypes` with no rule block.** An
  empty rule list would instead permit everything — valid YAML, silently wrong.

**This depends on the CNI.** NetworkPolicy is enforced by the network plugin,
not by Kubernetes. Under flannel the API server accepts every policy, `kubectl
get netpol` lists them, and nothing is enforced, so `aries-setup` defaults to
**Calico**.

### Resource telemetry

`NewResourceSource` reads the kubelet Summary API
(`/api/v1/nodes/<node>/proxy/stats/summary`) rather than metrics-server, for two
reasons. `pkg/monitor` wants a monotonic `CPUUsageNanoseconds` and does the rate
arithmetic itself; the Summary API reports exactly that as
`usageCoreNanoSeconds`, matching Docker's `CPUStats.CPUUsage.TotalUsage`,
whereas metrics-server reports a pre-averaged rate that cannot be converted
back. And one request per *node* returns every pod on it, so sampling cost
scales with node count rather than task count; reading cgroup files through
`kubectl exec` would put a process spawn and a TLS handshake on every pod on
every tick.

The cost is one cluster-scoped permission: `get` on `nodes/proxy`, from a
ClusterRole the chart creates. It is read-only and confined to that
subresource; delete it and telemetry goes quiet without affecting runs.

**Effective resolution is the kubelet's, not the monitor's.** The Summary API
serves whatever the kubelet's cAdvisor housekeeping last cached. With the
kubelet default, that was measured as identical timestamps across polls 2 s
apart followed by an 18 s jump, so at the monitor's 1 s interval most polls
repeat one observation. `pkg/monitor` divides the CPU delta by the gap between
observations, so the source suppresses repeats, and opts into absence tolerance
(`monitor.BaselineTolerantSource`) so a suppressed sample does not discard the
runtime's CPU baseline. Docker needs neither, because `ContainerStats` computes
fresh values per call.

`aries-setup` now sets the kubelet's housekeeping interval to 1 s
(`kubelet_housekeeping_interval`) and Prometheus scrapes cAdvisor every second.
The resulting per-container sample spacing has not been measured on a cluster
yet; until it is, assume the coarser resolution above.

## The SSH bridge advertise host

`openclawssh.Options.AdvertiseHost` switches the bridge to *advertised* mode:

- **Unset (Docker, default):** the SSH server binds the sandbox's Docker network
  gateway and advertises that gateway — unchanged behavior.
- **Set (Kubernetes):** the server binds `0.0.0.0` (reachable from pods) and the
  returned endpoint address and `known_hosts` line use the configured host.

The value supports environment expansion in wiring, which the old in-cluster
profiles used to advertise ARIES's own pod IP with `"$POD_IP"`. The bridge pod
now advertises its own pod IP itself.

## The OpenClaw harness

`KubeManager` (`pkg/harness/openclaw/kube.go`) is a second `AgentHarness`
backend in the OpenClaw package, reusing that package's config rendering,
private runtime archive, gateway WebSocket client, and artifact helpers
(`runtimeArchive` and `writeAgentResult` were extracted to shared functions).
Only the container-runtime operations differ. Agent (text) mode only; realtime
voice is rejected for this backend.

Per-task `Start` sequence:

1. Render the OpenClaw config and build the private runtime archive (plugin
   material, gateway launcher, keys, and the SSH client/identity/known_hosts
   the bridge produced).
2. `kubectl apply` a **Service** and a **Pod**. The pod boots into a wait state:
   it idles until a sentinel file appears, preserving the Docker "inject files,
   then start the gateway" ordering.
3. Wait for the pod to be `Running`, then stage the archive with `kubectl exec …
   tar -x -C /`, and release the gateway by creating the sentinel.
4. Wait for gateway readiness with the in-pod `/readyz` probe (over `kubectl
   exec`).
5. Open `kubectl port-forward service/… :18789` and point the gateway client at
   `ws://127.0.0.1:<local-port>`.

`Run` drives the agent exactly as the Docker backend does (connect → `Agent` →
redact → write result → collect logs). `Stop` kills the port-forward, deletes
the pod and Service, and confirms removal.

### Non-root staging

The agent container runs as uid 1000 (readiness requires it), so it cannot
write under root-owned `/run` or `/opt`. The pod therefore mounts writable
`emptyDir` volumes at `/run/aries` and `/opt/aries`, and a **root
initContainer** `chown`s them to 1000 so the uid-1000 `tar` extraction can
create and chmod the runtime files. This is the Kubernetes equivalent of
Docker's root-privileged `CopyToContainer`.

## The Hermes harness

`hermes.NewKube` (`pkg/harness/hermes/kube.go`), selected by `harness.type:
"hermes"` with `harness.deployment: "kubernetes"`, is simpler than the OpenClaw
port because Hermes has no gateway: the agent is a one-shot `exec` inside an
idling pod, so there is no Service, no port-forward and no sentinel. Its only
network peers are the model API and the `hermes-ssh` bridge, both of which it
dials itself.

1. `kubectl create` the pod, held at `sh -c 'exec sleep infinity'` instead of
   the image's s6 init. `create` rather than `apply`, so a pod that already has
   the fresh attempt name is never patched into ours.
2. `kubectl wait` for `Running`.
3. **Verify the admitted pod** (below) before any secret is staged.
4. Stage the runtime (config, model key, bridge identity, agent wrapper) with
   `kubectl exec -i -- tar -xpf - -C / --same-owner`. The pod runs as root, so
   GNU tar keeps the archive's `10000:10000` ownership; Docker's copy API resets
   it and needs a `chown` instead.
5. Poll `hermes --version` until it answers, which proves the staged runtime is
   readable by the unprivileged user the PATH shim drops to.

Run executes the same wrapper as Docker and writes the same artifact layout,
through code shared with the Docker backend rather than a copy of it.

**Admission verification** is the counterpart of Docker's `validateContainer`.
A mutating webhook can change a pod after ARIES creates it, so Start reads the
pod back and refuses — deleting it — unless it has exactly the Hermes container
with the pinned image and idle command, no init or ephemeral containers, no
volumes or mounts, no service-account token, no host network/PID/IPC, no
privileged container, no env sourced from the cluster, and no API key anywhere
in the spec. Tolerations are deliberately not compared: admission adds the
default `not-ready`/`unreachable` ones.

**Exit status** comes from a random-token trailer on stderr, as on Docker.
`kubectl exec` complicates it: a non-zero remote exit is reported as a kubectl
error *and* kubectl appends `command terminated with exit code N` to the same
stream. A task the agent fails is still a successful exec, so when the trailer
parses the kubectl error is disregarded and exactly that suffix is stripped; a
missing trailer, or anything else after it, is treated as the exec failing.

**Timeouts.** Cancelling `kubectl exec` stops the client, not reliably the
remote process. Stop deletes the pod, which is what actually ends an agent that
outlived its deadline.

## Configuration

| Field | Values | Meaning |
| --- | --- | --- |
| `sandbox.type` | `docker` \| `kubernetes` | Task environment backend. |
| `sandbox.namespace` | string (default `aries`) | Namespace for task pods. |
| `harness.deployment` | `docker` (default) \| `kubernetes` | Where the OpenClaw or Hermes agent runs. |
| `harness.namespace` | string (default `aries`) | Namespace for agent pods. |
| `bridge.advertise_host` | string | Host agents use to reach an in-runner bridge; env refs are expanded. Empty keeps Docker gateway binding. Must be empty for the bridge pod. |
| `bridge.deployment` | `process` (default) \| `kubernetes` \| `docker` | Where the bridge serves: inside the runner, in the `aries-bridge` pod, or in the `aries-bridge` container. |
| `bridge.namespace` | string (default: the sandbox namespace) | Where the bridge pod runs; must equal the sandbox namespace. |

Constraints: `harness.deployment: kubernetes` requires `harness.type` `openclaw`
or `hermes`, and rejects realtime mode. `bridge.deployment: kubernetes` requires
`hermes-ssh` or `openclaw-ssh` with a Kubernetes harness and sandbox.

Bundled profiles:

- `profiles/openclaw-tb2-fix-git-deepseek-k8s.json` — k8s sandbox with the E2B
  HTTP bridge.
- `profiles/openclaw-tb2-fix-git-deepseek-k8s-ssh.json` — k8s sandbox + k8s
  harness + SSH bridge, `advertise_host: host.docker.internal` (local clusters).
- `profiles/{openclaw,hermes}-tb2-fix-git-deepseek-k8s-pods.json` and their
  `x4` variants — agent, bridge and sandbox all in pods, with the runner outside
  the cluster. Formerly `-k8s-incluster`.

## Deployment

The runner runs as a process on a host that is **not** a cluster node; only
what must be in the cluster is. The runner generates the load and the bridge
sits on every tool call's path, so keeping them apart keeps the runner's own
work out of tool-call latency, and keeps profiles and runner code out of any
image.

- **`k8s/aries` chart** — the `aries-bridge` Deployment (`Dockerfile.bridge`:
  the bridge binary and `kubectl`, nothing else) with a Role of `get pods` and
  `pods/exec` and a NetworkPolicy admitting only harness pods; the runner's
  `aries` ServiceAccount, namespaced Role and long-lived token; the
  `nodes/proxy` ClusterRole for telemetry; the Grafana dashboard.
- **Runner host** — `aries-setup setup_runner` installs `kubectl` matched to the
  server, stages `bin/aries`, `bin/aries-ssh`, `profiles/` and `configs/`, and
  writes a kubeconfig for the `aries` ServiceAccount, so the runner holds the
  namespaced Role rather than cluster-admin. The model key stays on that host.

### The bridge pod

The runner meets the bridge pod only twice per task, so the control path can be
slow: `kubectl exec` into the pod runs `aries-bridge ctl`, which relays one JSON
request over a Unix socket to the daemon (`pkg/bridge/remote`). No port is
exposed, and who may drive the bridge is decided by RBAC. One daemon serves
every task's grant; each grant is an ordinary `hermesssh` or `openclawssh`
bridge advertising the pod's own IP, which `aries-bridge serve` reads from
`$POD_IP` (downward API), so `-k8s-pods` profiles leave `advertise_host` empty.

| Step | Runner | Bridge pod |
| --- | --- | --- |
| grant | generate the client key; send its **public** half with the sandbox's namespace, pod, ID, run and task | re-read the sandbox pod and refuse it unless it is a live ARIES sandbox with that ID, run and task; start an SSH bridge with a fresh host key; return address and host public key |
| (task runs) | — | tool calls: harness pod → SSH → `kubectl exec` into the sandbox |
| revoke | request revocation | stop the bridge: close the listener and sessions, drain in-flight calls |
| collect | stream the logs back into `runs/<task>/bridge/` at 0600 | serve them only once revoked |
| release | — | delete the grant's directory |

The client private key never leaves the runner host, and the files the harness
reads (`id_ed25519`, `known_hosts`, the `aries-ssh` helper) are written there
with the same names and modes as before, so the harnesses did not change.

**Revocation stays fail-closed.** `Stop` returns nil only when the bridge pod
answers `revoked` or `absent`, or when the pod that held the grant is gone or
has a different UID — grants live only in the daemon's memory, so a vanished
process serves nothing. A pod that is still there but does not answer blocks
evaluation. If revocation is proven by absence, the logs went with the pod; that
is reported as an error, which also blocks evaluation. The runner chooses the
grant ID itself, so a grant whose request failed in transit can still be revoked
by ID.

**The bridge's own privileges are narrow.** Its Role allows `get pods` and
`pods/exec`, nothing that creates or deletes. Its NetworkPolicy admits only
ARIES harness pods. It runs as uid 10001 with a read-only root filesystem, and
its grant directories and control socket are `emptyDir`s.

The same daemon serves Docker deployments from a container; see
[the bridge design](bridge.md#serving-from-a-separate-bridge).

Nodes carry no role labels or taints, so agent, sandbox, bridge and monitoring
pods mix on every worker.

## Operational notes

- **Architecture must match the nodes.** Build `linux/amd64` for x86 nodes;
  `setup_runner` builds the runner for the runner host's own architecture.
- **Image pulls.** The kubelet pulls the agent and task (Terminal-Bench) images
  per pod; ARIES skips the Docker pre-pull when the sandbox is Kubernetes. Nodes
  need egress to those registries, or the images mirrored into a reachable one.
  The bridge image comes from a registry or is imported into each worker's
  containerd.
- **Run artifacts** are written on the runner host under `runs/`, including
  each task's `bridge/tool-calls.jsonl`, collected from the bridge pod after
  revocation.
- **One bridge pod.** Restarting it ends every task in flight: access is
  provably revoked, their logs are lost, and their evaluation is blocked.

## Status

Verified on a CloudLab kubeadm cluster (Kubernetes v1.37, Calico) built from
bare hosts by `create_cluster`: a control plane, two workers with no node
roles, and a separate runner host set up by `setup_runner`.

| Check | Result |
| --- | --- |
| Runner kubeconfig | ServiceAccount token, 0600: can create pods and exec in `aries` and read `nodes/proxy`; cannot create pods in `kube-system`, list pods cluster-wide or bind cluster roles |
| Bridge ServiceAccount | `get pods` and `pods/exec` in `aries` only |
| Bridge pod, `kubecluster` test | grant 437 ms; a harness-labelled pod's tool call ran in the sandbox pod; the same key from an unlabelled pod was dropped by the NetworkPolicy before SSH; revoke and collect 635 ms, logs back at 0600; deleting the pod mid-grant proved revocation by absence and reported the logs lost |
| Hermes, `-k8s-pods` | reward 1, 2m47s, 30 tool calls through the bridge pod |
| Hermes x4, concurrency 4 | all four started together, each with its own grant; all reward 1, 77–94 s |
| OpenClaw, `-k8s-pods` | reward 1, 6m08s, 57 tool calls; staged key, helper and `known_hosts` removed on revocation |
| Task isolation (an earlier Calico cluster) | one policy per live task, none after teardown; from a task pod, the internet connected and a cluster pod timed out |

Not yet demonstrated:

- **The E2B HTTP bridge on Kubernetes.** The sandbox's exec and filesystem
  surface is complete, but the bridge's endpoint and authentication still assume
  a Docker network gateway; the SSH bridges are the validated path.
- **`openclaw-tb2-fix-git-deepseek-k8s-ssh.json`**, the bridge in the runner
  advertising `host.docker.internal`, has never run end to end.
- **Heterogeneous workloads.** Every cluster run repeats `fix-git`; no
  multi-task profile or `loop_duration` run has been made on the cluster.
- **Node reachability.** The egress exclusion covers the pod and Service CIDRs,
  not the nodes' own network, so a task with `allow_internet` can still reach
  node IPs — the kubelet, `hostNetwork` pods and NodePorts. This matters only
  for untrusted task images.
- **Realtime harness mode** is Docker-only, and `aries-setup` bootstraps only a
  single control plane.
