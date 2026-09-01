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

The task image comes from each Terminal-Bench task's own `task.toml`, and CPU/
memory limits are translated into container resources. Output per exec is capped
at 16 MiB; readiness and cleanup default to 120 s and 60 s.

**Not implemented:** resource sampling. `resourceSource.Sample` returns
`nil, nil` — a deliberate no-op placeholder. Per-pod telemetry needs
metrics-server or cAdvisor wiring, so runs on Kubernetes currently produce task
outcomes and timings but no pod-level resource readings.

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

- **`base/job.yaml`** — ARIES runs a profile to completion and exits, so it is a
  `Job` (`restartPolicy: Never`, `backoffLimit: 0`), not a Deployment. An
  experiment is not safe to retry mid-run, so it fails loudly instead.
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

## Current deployment state

A two-node cluster is running on CloudLab (`jxiang-314278.ntu-cloud-pg0`):

| | |
| --- | --- |
| Nodes | `node0` (control-plane), `node1` (worker) — both `Ready` |
| Kubernetes | v1.37.0 |
| Runtime | containerd 2.3.4 |
| OS | Ubuntu 22.04.2 LTS, kernel 5.15.0 |
| CNI | flannel, pod CIDRs `10.244.0.0/24` and `10.244.1.0/24` |
| Manifests | applied from `/tmp/k8s` via the `incluster` overlay |

**The in-cluster profile has run to completion on this cluster.** The `aries`
Job reports `Complete 1/1` with a duration of 5m02s and its pod exited
`Completed` with zero restarts. Because the Job is configured
`restartPolicy: Never` / `backoffLimit: 0`, a clean completion means the runner
finished its own lifecycle without a fatal error rather than being retried into
one.

That single result exercises the whole in-cluster path end to end: the image
pulled through the generated `aries-registry` secret, the model key resolved
from `aries-model`, the namespace-scoped RBAC covered every call the backends
make, the Kubernetes sandbox and harness created and tore down their per-task
pods, and the SSH bridge reached ARIES at its own pod IP via `$POD_IP` from the
downward API.

Getting there required the pull secret described in [README.md](README.md): the
Job was first applied before `aries-registry` existed, so the kubelet pulled
anonymously and Docker Hub returned `401`.

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
- **One task, one run.** The result covers a single occurrence of `fix-git` at
  `concurrency: 1`. Nothing yet exercises parallel task pods, loop duration, or
  a multi-task profile on the cluster.
- **No automated tests for the Kubernetes backends.** `pkg/sandbox/kubernetes`
  has no `_test.go` files, and there is no Kubernetes equivalent of the Docker
  integration tests. Both packages compile and are wired into `cmd/aries`, and
  the Docker paths they share are covered, but the Kubernetes-specific
  code paths are currently unexercised by CI.
- **Pod resource telemetry is a no-op**, so Kubernetes runs cannot yet answer
  the resource-pressure questions the framework exists to study.
- **Run artifacts use an `emptyDir`** and are lost when the Job is deleted.
  Retaining results needs a PersistentVolumeClaim.
- **Single control-plane only.** The bootstrap scripts do not join additional
  control-plane nodes.
