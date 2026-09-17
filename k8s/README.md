# ARIES on Kubernetes (Helm)

Helm charts for running ARIES on Kubernetes, plus the monitoring stack it is
observed with. For what the Kubernetes port implements across ARIES, the
harness, and the bridge — and what is still missing — see
[KUBERNETES.md](KUBERNETES.md).

```
k8s/
  aries/                # the ARIES chart: Deployment, RBAC, secrets, storage
  prometheus/           # vendored kube-prometheus-stack + our overrides
  grafana/              # Grafana overrides (a subchart of the above)
```

These three directories are the whole deployment surface. `aries-setup` installs
from them directly, so what a cluster receives is exactly what is committed
here.

If you do not have a cluster yet, `aries-setup` bootstraps one on bare Linux
hosts with kubeadm and can deploy all three charts in the same run; see
[`setup/`](../setup/README.md).

## Layout

- **aries/** — the ARIES chart. One long-lived idling pod, the namespaced Role
  for sandbox pod lifecycle, the `nodes/proxy` ClusterRole for telemetry, the
  model/registry Secrets, and optional node-local run storage. Environment
  differences are values files, not overlays:
  `values-local.yaml` for kind/minikube and `values-incluster.yaml` for a remote
  kubeadm cluster.
- **prometheus/** — the upstream kube-prometheus-stack chart, committed under
  `chart/` so an install needs no chart-repository access, with our overrides in
  `values.yaml`. Provenance and the refresh procedure are in
  [VENDORED.md](prometheus/VENDORED.md).
- **grafana/** — Grafana's overrides. Grafana is a **subchart** of
  kube-prometheus-stack, not a release of its own, so this file is applied to
  the same release and every key is nested under `grafana:`. Keeping it bundled
  is what supplies the Prometheus datasource and the default Kubernetes
  dashboards without configuring either.

## Credentials

Both charts that need secrets read them from a gitignored values file:

```sh
cp k8s/aries/secret.yaml.example k8s/aries/secret.yaml
$EDITOR k8s/aries/secret.yaml     # model API key, and a registry document if the image is private
```

Pass it last so it wins over the environment file. `aries-setup` ships it to the
control plane for you when `deploy_aries` is set, so there is no need to log in
to the master.

**Where the key ends up:** Helm stores a release's values in a Secret in the
release namespace, so `model.apiKey` is recoverable with `helm get values aries`
and stays in the release history until those revisions are pruned. Anyone who
can read Secrets in the namespace can already read the rendered Secret, so this
adds the history rather than the exposure — but use a key scoped to this project
and rotate it if the cluster is shared. To keep the key out of Helm entirely,
set `model.create=false` and create the `aries-model` Secret yourself.

## Deploy ARIES

### Local cluster (kind/minikube)

```sh
docker build -t aries:latest .
kind load docker-image aries:latest         # or: minikube image load aries:latest

helm upgrade --install aries ./k8s/aries \
  --namespace aries --create-namespace \
  -f ./k8s/aries/values-local.yaml -f ./k8s/aries/secret.yaml
```

`values-local.yaml` sets `pullPolicy: Never` (the image is side-loaded), drops
the pull secret, and disables role pinning — kind/minikube nodes carry no
`aries.dev/role` labels, so pinning would leave every pod `Pending`.

### Remote cluster, ARIES in-cluster

ARIES, the bridge, and the agent/sandbox pods share the cluster network, so the
agent's reverse SSH hop to the bridge is pod-to-pod and the bridge advertises
ARIES's own pod IP via the downward API.

```sh
docker build -t <registry>/aries:latest . && docker push <registry>/aries:latest
# point image.repository in values-incluster.yaml at your registry first

helm upgrade --install aries ./k8s/aries \
  --namespace aries --create-namespace \
  -f ./k8s/aries/values-incluster.yaml -f ./k8s/aries/secret.yaml
```

This pins ARIES to the `aries` role pool, persists run artifacts on that node,
sets `pullPolicy: Always` (the `:latest` tag is mutable, so a redeploy must
re-pull), and injects `$POD_IP`.

Preview before applying with `helm template` or `--dry-run`, and remove
everything with `helm uninstall aries -n aries`. The StorageClass, PV and PVC
carry `helm.sh/resource-policy: keep`, so uninstalling does **not** delete run
artifacts.

### Useful values

| Value | Default | What it does |
|---|---|---|
| `image.repository` / `.tag` | `aries:latest` | The image to run |
| `profile` | the E2B profile | `$ARIES_PROFILE`, the profile the exec line runs |
| `role.enabled` | `false` | Pin to `aries.dev/role=aries` (selector **and** toleration) |
| `runs.persistence.enabled` | `false` | PVC instead of `emptyDir` |
| `storage.create` | `false` | Also create the `no-provisioner` StorageClass and hostPath PV |
| `podIP` | `false` | Expose `$POD_IP` for a bridge `advertise_host` |
| `rbac.nodeMetrics` | `true` | The `nodes/proxy` ClusterRole for pod telemetry |
| `openclaw.enabled` | `false` | The static OpenClaw gateway Deployment + Service |

## Deploy Prometheus and Grafana

One release brings up both, because Grafana is a subchart:

```sh
helm upgrade --install prometheus ./k8s/prometheus/chart \
  --namespace monitoring --create-namespace \
  -f ./k8s/prometheus/values.yaml -f ./k8s/grafana/values.yaml \
  --wait --timeout 15m
```

`aries-setup setup_prometheus` runs exactly this, and additionally renders a
placement values file from `node_role` so every single-instance component lands
on the monitoring node and tolerates its taint. Prefer that path on a role-
labelled cluster: by hand, the components have no toleration and stay `Pending`.

**Grafana is published on a NodePort**, so it needs no port-forward — open
`http://<any-node-ip>:30300`. kube-proxy answers on every node and routes to
whichever node the pod runs on, so any node's address works. The port is fixed
in `k8s/grafana/values.yaml` rather than assigned, so the URL survives a
reinstall, and `setup_prometheus` prints the resolved URL when it finishes.

```sh
# Grafana user is admin; the password is generated, not the chart's public default:
kubectl -n monitoring get secret prometheus-grafana -o jsonpath='{.data.admin-password}' | base64 -d
```

> **A NodePort listens on every node interface.** On a cluster whose nodes have
> public addresses — CloudLab's do — this publishes Grafana to the internet. A
> login is still required (the password is generated and anonymous access is
> off), but it is a public login page. Keep the generated password, do not
> enable `auth.anonymous`, and set `grafana.service.type` back to `ClusterIP` if
> you would rather tunnel in.

Prometheus stays ClusterIP deliberately: it is scraped from inside the cluster
and read through Grafana, so publishing it would add exposure for nothing.
Reach it on demand:

```sh
kubectl -n monitoring port-forward svc/prometheus-kube-prometheus-prometheus 9090
```

An empty `grafana.adminPassword` makes the chart reuse the password already in
the `prometheus-grafana` Secret, or generate a 40-character one on a first
install. `helm template` and `--dry-run` cannot perform that lookup, so they
render a throwaway password rather than the live one.

ARIES exposes no Prometheus endpoint of its own; pod CPU and memory come from
the kubelet/cAdvisor scrapes this stack already performs. See
[KUBERNETES.md](KUBERNETES.md) for what telemetry ARIES records itself.

## The OpenClaw gateway

`openclaw.enabled=true` deploys the gateway as a Deployment fronted by a
ClusterIP Service. It is off by default because ARIES's Kubernetes harness
backend (`pkg/harness/openclaw`, `NewKube`, selected by
`harness.deployment: "kubernetes"`) creates its own per-task agent pod and
Service; these static resources are the reference/template for that flow, and
are useful when driving one long-lived gateway by port-forward:

```sh
kubectl -n aries port-forward svc/aries-openclaw 18789:18789
# ARIES then targets ws://127.0.0.1:18789
```

The pod boots into a wait state and idles until ARIES stages the per-task
runtime (plugin + gateway launcher + rendered config) and creates the
`/run/aries/ready` sentinel — preserving the Docker "inject files, then start the
gateway" ordering on Kubernetes. It therefore reports NotReady until a task
starts, which is expected. Pair it with a Kubernetes-capable bridge:
`bridge.type: "openclaw-ssh"` with `bridge.advertise_host` set (see
`profiles/openclaw-tb2-fix-git-deepseek-k8s-ssh.json`).

## Triggering a run

The ARIES binary is a batch runner: `aries PROFILE.json` runs a profile to
completion and exits. It has no server mode and no task-intake API. A Deployment
restarts its container whenever the process exits, so running a profile as the
entrypoint would re-run the whole experiment in a loop.

The chart therefore overrides the image entrypoint with `sleep infinity`. The pod
comes up idle and stays up; runs are started on demand:

```sh
# Run the profile the values file selected:
kubectl -n aries exec deploy/aries -- sh -c './bin/aries "$ARIES_PROFILE"'

# ...or name one explicitly:
kubectl -n aries exec deploy/aries -- ./bin/aries profiles/openclaw-tb2-five-deepseek.json

# Follow along, or attach a shell:
kubectl -n aries logs -f deploy/aries
kubectl -n aries exec -it deploy/aries -- bash
```

`ARIES_PROFILE` is an env var on the container, so the exec line never has to
repeat the path. `sh -c` is what expands it — inside the pod, not in your local
shell.

Keeping the pod up means the image, the model key, and the run-artifact volume
stay mounted between runs, so iterating is far quicker than recreating a Job
each time.

With `runs.persistence.enabled`, `runs/` is backed by a PersistentVolume, so
artifacts outlive pod restarts:

```sh
kubectl -n aries exec deploy/aries -- ls runs/                  # every run so far
kubectl -n aries cp aries-<pod>:/app/runs/<run-id> ./<run-id>   # pull one down
```

A bare kubeadm cluster has no dynamic provisioner, so the volume is defined
statically: a `no-provisioner` StorageClass, a 20 GiB `hostPath` PV at
`/var/lib/aries/runs` with `nodeAffinity` pinning it to the aries-role node, and
a matching PVC. `WaitForFirstConsumer` binding means the scheduler places the pod
first and then matches the volume on that node. `reclaimPolicy: Retain` keeps the
data if the PVC is deleted.

### Running tasks concurrently

`execution.concurrency` in the profile bounds how many task occurrences run at
once. ARIES builds a fresh harness, sandbox, and bridge per occurrence, so each
concurrent task gets its own agent pod on the harness node and its own sandbox
pod on the sandbox node:

```json
"execution": { "concurrency": 4 }
```

Size it against the pools, not the number of tasks: N concurrent tasks means N
agent pods on one node and N sandbox pods on another, each with the CPU and
memory its `task.toml` requests. Overshoot and pods sit `Pending` on
`Insufficient cpu`.

Two consequences worth knowing. Nothing runs automatically on `helm install` —
deploying only makes the pod ready. And a run is tied to your `exec` session, so
a dropped connection kills it; use `kubectl exec ... -- sh -c 'nohup
./bin/aries "$ARIES_PROFILE" > runs/last.log 2>&1 &'` for long experiments.

## Status / caveats

- A Kubernetes-native task **sandbox** now exists (`pkg/sandbox/kubernetes`,
  selected by `sandbox.type: "kubernetes"`). It drives the cluster via the
  `kubectl` binary (bundled in the image): one pod per task, `kubectl exec` for
  commands, `kubectl cp`/streamed exec for files. The chart's Role grants
  exactly the pod/exec/log permissions it needs. Use
  `profiles/openclaw-tb2-fix-git-deepseek-k8s.json` to select it.
- **Task network isolation** gives every task pod its own `NetworkPolicy`.
  Ingress is always denied, which is what keeps concurrent tasks from reaching
  each other; `allow_internet` decides only whether egress is denied outright or
  limited to non-cluster destinations. Set `sandbox.pod_cidr` and
  `sandbox.service_cidr` in the profile so the second case can exclude the
  cluster's own networks — read them with
  `kubectl cluster-info dump | grep -m2 -E 'cluster-cidr|service-cluster-ip-range'`.
  This requires a CNI that implements NetworkPolicy: `setup/` defaults to
  **Calico**, and under flannel the policies are created and silently ignored.
- **Pod resource metrics** are read from the kubelet Summary API — no
  metrics-server needed. This needs the `aries-node-metrics` ClusterRole
  (`get` on `nodes/proxy`), the only permission ARIES holds outside its
  namespace. Set `rbac.nodeMetrics=false` to disable telemetry.
- **Not yet solved:** the OpenClaw E2B bridge authorizes requests by matching
  the sandbox's `NetworkGateway` against the request origin and uses it to build
  the bridge endpoint address. On Kubernetes the address a pod uses to reach the
  ARIES bridge is not the pod IP, so end-to-end bridge reachability/auth for the
  E2B pairing still needs a cluster-network-aware adaptation. The sandbox's own
  exec/filesystem surface is complete; this is the remaining integration gap.
- The chart's ClusterRole and ClusterRoleBinding are **cluster-scoped** and
  named from `fullnameOverride`, which values.yaml pins to `aries`. Two releases
  in different namespaces would fight over them; give each a distinct
  `fullnameOverride`.
- The default profile references the `openclaw-e2b` bridge + `docker` sandbox;
  point `profile` at a Kubernetes-backed profile for cluster runs.
