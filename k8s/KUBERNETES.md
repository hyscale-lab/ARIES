# ARIES on Kubernetes: implementation record

What has been built so far to run ARIES, the OpenClaw agent harness, and the
tool bridge on Kubernetes, and what is still missing. This is a status record of
the Kubernetes port — the architectural contract it must satisfy is in
[docs/design.md](../docs/design.md), and the deployment instructions are in
[k8s/README.md](README.md).

Every Runner role that touches the cluster drives it through the **`kubectl`
binary** rather than a `client-go` dependency. The binary and its resolved
kubeconfig/context are the entire cluster contract, which keeps the Kubernetes
backends as substitutable as the Docker ones and avoids pulling an API-machinery
dependency tree into a benchmark runner.

## The two topologies

Kubernetes changes one thing that matters: on Docker, the agent reaches the
ARIES bridge over the sandbox's network gateway, and that address does not
exist in a cluster. Both Kubernetes profiles use the `openclaw-ssh` bridge and
differ only in how the agent reaches it.

| Profile | ARIES runs | Harness | Sandbox | Bridge | Status |
| --- | --- | --- | --- | --- | --- |
| `openclaw-tb2-fix-git-deepseek-k8s-ssh.json` | outside | Kubernetes | Kubernetes | `openclaw-ssh` (`advertise_host: host.docker.internal`) | Implemented, not yet run end to end |
| `openclaw-tb2-fix-git-deepseek-k8s-incluster.json` | **inside** | Kubernetes | Kubernetes | `openclaw-ssh` (`advertise_host: $POD_IP`) | **Tested and working** — completed a full run in-cluster |

The in-cluster profile is the one that closes the reachability problem
cleanly: ARIES, the bridge, and the agent/sandbox pods all sit on the same pod
network, so the agent's reverse SSH hop to the bridge is plain pod-to-pod
traffic and needs no external routing at all.

## Tool Sandbox — `pkg/sandbox/kubernetes`

Selected by `sandbox.type: "kubernetes"`. One pod per task in a fixed namespace
(`sandbox.namespace`, default `aries`). It satisfies the same interfaces as the
Docker backend, so it is chosen purely by profile configuration with no code
change at the call site.

| Capability | How |
| --- | --- |
| Command execution | `kubectl exec` |
| Streamed exec (stdin/stdout/stderr) | `kubectl exec -i` |
| File transfer | `kubectl cp` and streamed exec |
| Filesystem surface | `ReadFile`, `WriteFile`, `StatPath`, `ListDir`, `MakeDir`, `RemovePath`, `MovePath` |
| Attached processes | `ExecProcessStream` with a PID file at `/tmp/.aries-k8s-<token>`, plus `SendProcessSignal` / `TerminateProcess` |
| Identity | `ContainerID()` / `NetworkName()` report `<namespace>/<pod>` |
| Network isolation | A per-task deny-all `NetworkPolicy`, created before the pod and deleted after it |
| Resource telemetry | Cumulative CPU/memory counters from the kubelet Summary API |

The task image comes from each Terminal-Bench task's own `task.toml`, and CPU/
memory limits are translated into container resources. Output per exec is capped
at 16 MiB; readiness and cleanup default to 120 s and 60 s.

### Network isolation

Every task pod gets a `NetworkPolicy` selecting exactly itself. A task's
`allow_internet` decides what that policy *permits*, never whether it exists:

| `allow_internet` | Ingress | Egress |
| --- | --- | --- |
| false | denied | denied — the air-gapped equivalent of Docker's `Internal: true` |
| true | denied | internet yes; pod network, Services and API server no |

The distinction matters because Docker supplies two separate guarantees that are
easy to conflate. `Internal` controls whether a task reaches the internet, while
the fact that each task gets *its own network* is what stops two concurrent tasks
reaching each other. A cluster has one flat pod network, so both have to be
written down here. An earlier version of this code created no policy at all when
`allow_internet` was true, which left concurrent task pods mutually reachable —
and since `fix-git` sets `allow_internet = true`, that was the only case the
benchmark actually exercised.

Denying ingress is the load-bearing half. A pod that accepts no connections
cannot be reached by a peer whatever that peer is allowed to send, so inter-task
isolation does not depend on `sandbox.pod_cidr` / `sandbox.service_cidr` being
set correctly. Getting those wrong widens outbound reach; it never lets tasks
talk to each other.

DNS is permitted by selecting the CoreDNS *pods*, not the `kube-dns` Service IP.
kube-proxy DNATs Service addresses before egress policy is evaluated, so a rule
naming the Service address never matches and the cluster-CIDR exclusion then
drops the resolved pod IP — breaking name resolution while leaving raw-IP traffic
working.

Isolation costs nothing functionally, because the sandbox is driven entirely by
`kubectl exec` — API server to kubelet, over the host network. A task pod with
zero connectivity is still fully usable.

Two details are easy to get wrong and are worth stating. The policy is created
*before* the pod, because one created after leaves a window in which the
container is running unisolated, and that window is exactly when an image's
entrypoint makes its network calls. And the deny-all spelling is naming a
direction in `policyTypes` with **no** corresponding rule block; adding an empty
rule list would instead permit everything, which is valid YAML and silently
wrong.

**This depends on the CNI.** NetworkPolicy is enforced by the network plugin,
not by Kubernetes. Under flannel the API server accepts every policy, `kubectl
get netpol` lists them, and nothing is enforced. `k8s/install` therefore
defaults to **Calico**.

### Resource telemetry

`Sample` reads the kubelet Summary API
(`/api/v1/nodes/<node>/proxy/stats/summary`) rather than metrics-server, for two
reasons. Semantically, `pkg/monitor` wants a monotonic `CPUUsageNanoseconds` and
does the rate arithmetic itself; the Summary API reports exactly that as
`usageCoreNanoSeconds`, matching Docker's `CPUStats.CPUUsage.TotalUsage`, whereas
metrics-server reports a pre-averaged rate in millicores that cannot be converted
back. Operationally, one request per *node* returns every pod on it, so sampling
cost scales with node count rather than task count — which matters, because the
alternative of reading cgroup files through `kubectl exec` would put a process
spawn and a TLS handshake on every pod on every tick.

The cost is one cluster-scoped permission: `get` on `nodes/proxy`, granted by a
ClusterRole in `base/rbac.yaml`. It is read-only and confined to that
subresource. Delete it and telemetry goes quiet without affecting runs.

**Effective resolution is the kubelet's, not the configured interval.** The
kubelet does not compute stats on demand; the Summary API serves whatever its
cAdvisor housekeeping loop last cached. Measured on this cluster: identical
timestamps across polls 2s apart, then an 18s jump. At the monitor's 1s interval
roughly nine polls in ten are therefore repeats of the same observation.

That matters because `pkg/monitor` divides the CPU counter delta by the wall gap
between consecutive observations, so a repeated timestamp is a division by zero —
it rejects the reading and fails the whole monitor for that task. The source
suppresses repeats rather than raising the interval, which keeps the monitor
contract deployment-neutral: Docker genuinely can sample every second, because
`ContainerStats` computes fresh values per call. The practical consequence is
that Kubernetes runs resolve resource pressure at roughly 10-20s granularity,
and short spikes inside that window are invisible.

## Agent Harness — `pkg/harness/openclaw/kube.go`

Selected by `harness.deployment: "kubernetes"`, constructed by `NewKube`. It
reuses the package's existing config rendering, runtime archive staging, gateway
client, and artifact helpers — only the container-runtime operations differ
(`kubectl` + port-forward instead of the Docker SDK).

The hard part it solves is **ordering**. On Docker, ARIES injects the per-task
private runtime into the container and only then starts the gateway. A pod
normally starts its entrypoint immediately, which would boot the gateway before
its config exists. The port preserves the Docker ordering with a sentinel:

1. create the per-task **Service**, then the **Pod**;
2. the pod's command idles in a wait loop, polling for `/run/aries/ready`;
3. `kubectl wait` for pod readiness;
4. stage the private runtime — plugin, gateway launcher, rendered config —
   by piping a tar archive into `kubectl exec -i -- tar -xmf - -C /`;
5. `touch /run/aries/ready`, releasing the wait loop, which then `exec`s the
   staged gateway launcher;
6. wait for the gateway to report ready;
7. open a `kubectl port-forward` so out-of-cluster ARIES can reach the gateway.

Teardown kills the port-forward and deletes the Pod and Service, confirming
removal. The OpenClaw image is validated as a pinned tag before anything is
created.

**Not supported:** realtime voice mode. The Kubernetes harness backend is
agent/text mode only.

## Tool Bridge — `pkg/bridge/openclawssh`

The SSH bridge gained an **advertised mode**, which is the change that makes it
usable off Docker. `Options.AdvertiseHost` switches behavior:

- **empty (Docker):** resolve the sandbox's network gateway, bind the SSH
  listener to it, and advertise that same address.
- **set (Kubernetes):** bind `0.0.0.0` and advertise the configured,
  cluster-reachable host instead.

The advertised host — not the bound interface — is what goes into the generated
`known_hosts` line and the endpoint handed to the agent, so host-key
verification still matches what the agent dials.

`cmd/aries/wiring.go` passes the profile's `advertise_host` through
`os.ExpandEnv`, which is what lets the in-cluster profile write the literal
string `"$POD_IP"` and have it resolve to ARIES's own pod IP at run time. The
in-cluster overlay injects `POD_IP` through the downward API
(`fieldRef: status.podIP`).

Per-task session keys, the staged `aries-ssh` client, the structured
`tool-calls.jsonl`, and the raw `ssh_raw.log` are unchanged from the Docker
path — the Kubernetes work did not fork the audit or key-handling logic.

## Manifests — `k8s/base`, `k8s/overlays`

- **`base/deployment.yaml`** — ARIES runs as a long-lived pod. The binary is a
  batch runner with no server mode, so the container overrides the image
  entrypoint with `sleep infinity` and runs are triggered on demand with
  `kubectl exec`. Running a profile as the entrypoint under a Deployment would
  re-run the whole experiment every time the process exited. `strategy: Recreate`
  keeps two ARIES pods from ever running against the same namespace at once.
- **`base/rbac.yaml`** — a namespace-scoped Role granting exactly what the
  Kubernetes backends use: `pods` (create/get/list/watch/delete/patch),
  `pods/exec` + `pods/attach` + `pods/portforward`, `pods/log`, `services` for
  the harness's per-task Service, and `configmaps`/`secrets` for per-task access
  material.
- **`base/openclaw/`** — Deployment + ClusterIP Service running the gateway as a
  long-lived workload, booting into the same sentinel wait state. This is a
  reference/template for the flow the Go harness now performs per task, not
  something the runner applies itself.
- **`overlays/local`** — kind/minikube: `imagePullPolicy: Never`, image loaded
  into the node.
- **`overlays/incluster`** — remote cluster: image pulled from a registry,
  `imagePullPolicy: Always` (the `:latest` tag is mutable), the in-cluster
  profile selected, `POD_IP` injected, and two generated Secrets —
  `aries-model` from `secret.env` and `aries-registry` from `registry.json`,
  both gitignored.

## Cluster bootstrap — `k8s/install`

Added so a cluster can be created from bare Linux hosts rather than assumed.
`install-master.sh` and `install-worker.sh` share `common.sh`, which resolves
the release channel from `dl.k8s.io/release/stable.txt`, disables swap, loads
`overlay`/`br_netfilter`, installs containerd with `SystemdCgroup = true`, and
installs the kube packages from `pkgs.k8s.io`. The control-plane script runs
`kubeadm init`, installs the CNI (flannel or Calico), and emits a join command;
`reset-node.sh` returns a node to a pre-kubeadm state. Full detail in
[k8s/install/README.md](install/README.md).

The CNI default is **Calico**, and that is a correctness requirement rather than
a preference: task isolation is expressed as NetworkPolicy, and flannel accepts
those objects without enforcing them. Choosing `CNI=flannel` leaves task pods
mutually reachable while every policy still appears in `kubectl get netpol`.

## Current deployment state

A four-node, role-partitioned cluster is running on CloudLab
(`jxiang-314636.ntu-cloud-pg0`), built by `k8s/install/install-cluster.sh`:

| | |
| --- | --- |
| Nodes | `node0` control-plane/`master`, `node1` `aries`, `node2` `harness`, `node3` `sandbox` — all `Ready`, 64 CPU each |
| Kubernetes | v1.37.0 |
| Runtime | containerd 2.3.4 |
| OS | Ubuntu 22.04.2 LTS, kernel 5.15.0 |
| CNI | flannel |
| Manifests | `overlays/incluster`, ARIES as a long-lived Deployment on the `aries` node |

**The in-cluster profile has completed a four-way concurrent run.**
`openclaw-tb2-fix-git-x4-deepseek-k8s-incluster` ran `fix-git` four times at
`concurrency: 4`. All four occurrences started in the same second, each with its
own agent pod on the `harness` node and its own sandbox pod on the `sandbox`
node:

| Occurrence | Duration | Harness | Evaluation |
| --- | --- | --- | --- |
| `fix-git-001` | 6m10s | succeeded | solved |
| `fix-git-002` | 1m22s | succeeded | reward 0 |
| `fix-git-003` | 5m45s | succeeded | solved |
| `fix-git-004` | 6m42s | succeeded | solved |

Summary: 4 harnesses succeeded, 4 evaluations run, 3 succeeded, 0 cleanup
failures. The single failure is a benchmark outcome, not an infrastructure one —
the agent finished its turn and the verifier scored the result 0.

Worth noting for its own sake: the failing occurrence also finished roughly five
times faster than the three that solved the task. Identical inputs, and duration
tracked success — the kind of per-trajectory signal an aggregate pass rate hides,
and the reason the framework records whole trajectories rather than outcomes.

That single result exercises the whole in-cluster path end to end: the image
pulled through the generated `aries-registry` secret, the model key resolved
from `aries-model`, the namespace-scoped RBAC covered every call the backends
make, the Kubernetes sandbox and harness created and tore down their per-task
pods, and the SSH bridge reached ARIES at its own pod IP via `$POD_IP` from the
downward API.

Getting there required the pull secret described in [README.md](README.md): the
Job was first applied before `aries-registry` existed, so the kubelet pulled
anonymously and Docker Hub returned `401`.

### Verified on the cluster

Both features were exercised on the rebuilt Calico cluster, not merely compiled:

| Check | Result |
| --- | --- |
| Policy per live task | 4 task pods → 4 policies; 0 after teardown |
| Egress to the internet | `connect_ex(('1.1.1.1', 443))` → `0`, immediate |
| Egress to a cluster pod | `connect_ex((<agent pod>, 18789))` → `11`, after the full timeout |
| Telemetry populated | both `sandbox` and `harness` components, counters monotonic |
| CPU rate | peaks of 96-139% across four occurrences, matching the raw counter deltas |

The two egress results are the same syscall from the same pod, and the contrast
is what makes them evidence. The agent gateway *is* listening, so an unenforced
policy returns `0` immediately and a closed port refuses immediately; only a
dropped packet times out.

Three bugs surfaced here that no unit test caught, because all three are
properties of a *sequence* of samples rather than of one value:

1. `RuntimeID` was `<namespace>/<pod>`, and `pkg/monitor` rejects `/` as an
   unsafe path component. Docker never trips this — container IDs are hex.
2. The kubelet serves cached stats, so polling faster than its housekeeping
   interval returns a repeated observation time, and the recorder divides by the
   gap between observations.
3. Fixing (2) by suppressing repeats made live pods intermittently absent, and
   the recorder discarded a runtime's CPU baseline on first absence — so
   `cpu_percent` read 0 forever while the raw counter climbed. Fixed by letting a
   source opt into absence tolerance via `monitor.BaselineTolerantSource`, which
   leaves the Docker path's behaviour exactly as it was.

Each was a Docker-shaped assumption that does not hold on Kubernetes.

## What has not been demonstrated

Stated plainly, because the code existing is not the same as the path working:

- **The out-of-cluster SSH profile is still unrun.**
  `openclaw-tb2-fix-git-deepseek-k8s-ssh.json` depends on the agent reaching
  ARIES at `host.docker.internal`, which is a different reachability path from
  the in-cluster `$POD_IP` one that has been exercised.
- **The benchmark outcome of the completed run is not recorded here.** A
  `Complete` Job proves the runner finished its lifecycle; it does not by itself
  say whether the `fix-git` task was scored correct. Read the run artifacts for
  that — and note they live in an `emptyDir` (below).
- **One task, repeated.** The concurrent run covers four occurrences of the same
  task. Nothing yet exercises a heterogeneous multi-task profile or
  `loop_duration` on the cluster.
- **ARIES is not a service.** The Deployment keeps a pod warm for `kubectl exec`;
  it does not accept task requests. A real long-running server needs an
  `aries serve` subcommand with a task-intake API, a queue, and per-request run
  directories — none of which exist.
- **No automated tests for the Kubernetes backends.** `pkg/sandbox/kubernetes`
  has no `_test.go` files, and there is no Kubernetes equivalent of the Docker
  integration tests. Both packages compile and are wired into `cmd/aries`, and
  the Docker paths they share are covered, but the Kubernetes-specific
  code paths are currently unexercised by CI.
- **Task pods can still reach the node network.** The egress exclusion covers
  the pod and Service CIDRs, not the underlay the nodes sit on, so a task with
  `allow_internet` can still reach node IPs — kubelet on `:10250`, anything on
  `hostNetwork`, and NodePort Services. Adding the node CIDR to
  `sandbox.pod_cidr`'s exclusion list would close it. This matters only if task
  images are untrusted; they are not, today.
- **Run artifacts survive restarts on the in-cluster overlay**, which binds a
  20 GiB node-local PersistentVolume at `/var/lib/aries/runs` on the aries node.
  They do not survive that node being rebuilt — the volume is `hostPath`, not
  replicated. `base` still uses an `emptyDir` so the local/kind path needs no
  storage setup.
- **Single control-plane only.** The bootstrap scripts do not join additional
  control-plane nodes.
