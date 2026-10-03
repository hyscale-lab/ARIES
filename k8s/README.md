# ARIES on Kubernetes

Everything needed to run ARIES on Kubernetes: the Helm charts, the monitoring
stack it is observed with, and `aries-setup`, which turns bare Linux hosts into
a cluster and deploys both. For what the Kubernetes port implements across
ARIES, the harness and the bridge — and what is still missing — see
[the Kubernetes design](../docs/design/kubernetes.md). For a single Docker host instead, see
[Docker deployment](../docker/README.md).

```
k8s/
  aries/                # the ARIES chart: tool bridge pod, runner RBAC, dashboard
  prometheus/           # our kube-prometheus-stack overrides
  grafana/              # Grafana overrides (a subchart of the above)
  setup/                # aries-setup: cluster bootstrap and deployment tool
    aries-setup/        #   the binary's entry point
    configs/            #   its JSON configuration
    internal/           #   node, cluster, config and exec packages
```

These directories are the whole deployment surface. `aries-setup` installs
from them directly: the ARIES chart as committed, and the upstream
kube-prometheus-stack pulled by the version and digest pinned in
`setup/configs/prometheus/prom_config.json`, with the values files here.

- **aries/** — the in-cluster half of ARIES: the tool bridge pod
  (`aries-bridge`) with its own narrow Role and NetworkPolicy, the runner's
  ServiceAccount, namespaced Role and token, the `nodes/proxy` ClusterRole for
  telemetry, the registry pull Secret and the Grafana dashboard. The runner is
  not in the chart: it runs as a process on a host outside the cluster. Values
  files select the environment: `values-local.yaml` for kind/minikube and
  `values-cluster.yaml` for a remote kubeadm cluster.
- **prometheus/** — our overrides for the upstream kube-prometheus-stack chart,
  in `values.yaml`. The chart itself is not committed; see
  [Deploying Prometheus and Grafana](#deploying-prometheus-and-grafana).
- **grafana/** — Grafana's overrides. Grafana is a **subchart** of
  kube-prometheus-stack, not a release of its own, so this file is applied to
  the same release and every key is nested under `grafana:`. Keeping it bundled
  is what supplies the Prometheus datasource and the default Kubernetes
  dashboards without configuring either.
- **setup/** — `aries-setup`, laid out like vHive's
  [`scripts/setup`](https://github.com/vhive-serverless/vHive/tree/main/scripts/setup):
  one binary, named subcommands, JSON configuration. The kube packages, the
  container runtime and the CNI are downloaded from upstream at run time; only
  the charts are taken from this tree.

## Architecture

```
 runner host (not a node)            cluster
┌────────────────────────┐          ┌───────────────────────────────────────────┐
│ bin/aries (runner)     │ kubectl  │ aries-bridge pod   harness pods           │
│  sandbox / harness mgr ├─────────►│  (one per run)      (one per task)        │
│  remote bridge client  │ API srv  │     ▲    │              │                 │
└────────────────────────┘          │     │    └─kubectl exec─┼──► sandbox pods │
                                    │     └──── SSH ──────────┘   (one per task)│
                                    └───────────────────────────────────────────┘
```

- **The runner** creates and deletes sandbox and harness pods and drives the
  per-task lifecycle, all through the API server. It meets the bridge pod twice
  per task — grant and revoke — by `kubectl exec` into it (see
  `pkg/bridge/remote`). Nothing on the tool-call path runs on the runner host,
  so its load stays out of the measurements.
- **The tool bridge pod** serves every task's SSH bridge. Harness pods dial it
  on its pod IP; it runs each tool call in the task's sandbox with `kubectl
exec`. Its Role allows only `get pods` and `pods/exec`, and its
  NetworkPolicy admits only ARIES harness pods.
- **Revocation** is confirmed by the bridge pod, or proven by its absence:
  grants live only in the daemon's memory, so a bridge pod that is gone (or
  replaced, by UID) serves nothing. A live pod that does not answer blocks
  evaluation. The tool-call logs are collected back to the runner after
  revocation.

## `aries-setup`

| Subcommand                    | Does                                                               | Runs on             |
| ----------------------------- | ------------------------------------------------------------------ | ------------------- |
| `setup_node`                  | install containerd and kubelet/kubeadm/kubectl                     | every node, as root |
| `setup_master_node`           | `kubeadm init`, CNI, write a join command                          | control plane       |
| `setup_worker --join "<cmd>"` | `kubeadm join`                                                     | each worker         |
| `setup_prometheus`            | install Prometheus + Grafana from the pinned upstream chart        | control plane       |
| `setup_aries`                 | install the ARIES chart: the tool bridge pod and the runner's RBAC | control plane       |
| `reset_node --yes`            | tear the node back to a pre-kubeadm state                          | any node            |
| `create_cluster`              | all of the above over SSH, then `setup_runner`                     | your machine        |
| `setup_runner`                | prepare the runner host, which is **not** a cluster node           | your machine        |

There are two ways to use it. **From your machine** — describe the nodes in
`cluster.json` and run one command
([Whole cluster in one command](#whole-cluster-in-one-command)). **By hand** —
run each on-node subcommand on its host yourself, which is how vHive's tool
works ([By hand](#by-hand)). `create_cluster` runs exactly the same on-node
subcommands over SSH, so both produce identical clusters. If you already have a
cluster, skip to [Deploying ARIES](#deploying-aries).

### Configuration

```
k8s/setup/configs/
  kube.json              # Kubernetes version, CNI, pod CIDR, Calico pin  -> shipped to every node
  system.json            # upstream URLs and node paths (optional)        -> shipped to every node
  cluster.json.example   # SSH topology and the runner host               -> stays on your machine
  prometheus/
    prom_config.json     # chart version + Helm pins                      -> shipped when deploy_prometheus
  aries/
    aries_config.json    # namespace, release, values file order          -> shipped when deploy_aries
```

`--configs-dir` defaults to `k8s/setup/configs` and `--charts-dir` to `k8s`,
both relative to the repository root. Only what is being deployed is shipped,
and only to the master: the ARIES chart, and for Prometheus just the two values
files, since the master pulls the chart itself.

`cluster.json` is gitignored and never copied to a node, since it names every
host and user. Unknown keys are rejected in every file, so a typo fails
immediately rather than silently taking a default.

#### `cluster.json`

| Key                 | Meaning                                                                                               |
| ------------------- | ----------------------------------------------------------------------------------------------------- |
| `master`            | SSH target of the control plane, e.g. `JXiang@hp030.utah.cloudlab.us`                                 |
| `workers`           | joined as plain workers; see [No node roles](#no-node-roles)                                          |
| `runner`            | the host the ARIES runner runs on; **never joined**. See [The runner host](#the-runner-host)          |
| `runner_dir`        | where the runner is staged, relative to its home. Default `aries`                                     |
| `ssh_key`           | identity file; empty uses your ssh-agent and `~/.ssh/config`                                          |
| `ssh_options`       | extra ssh arguments, one per entry: `["-o", "ConnectTimeout=15"]`                                     |
| `fetch_kubeconfig`  | copy the admin kubeconfig to `k8s/setup/kubeconfig`                                                   |
| `deploy_prometheus` | run `setup_prometheus` once the workers have joined                                                   |
| `deploy_aries`      | run `setup_aries`, then `setup_runner` when `runner` is set; needs a bridge image the cluster can use |

Every SSH target is validated as `[user@]host`: a value starting with `-` would
be read by ssh as an option. Listing the master or a worker twice is harmless —
each is installed once. The runner may not also be the master or a worker.

The retired `aries_nodes`, `harness_nodes`, `sandbox_nodes` and
`skip_role_taints` keys are rejected by name, so an old `cluster.json` fails
loudly: move those hosts into `workers`, and the old ARIES node into `runner`.

#### `kube.json`

| Key                             | Default   | Meaning                                                                                                                               |
| ------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `k8s_version`                   | stable    | exact patch, e.g. `1.34.2`                                                                                                            |
| `k8s_minor`                     | derived   | package channel, e.g. `v1.34`                                                                                                         |
| `cni`                           | `calico`  | `calico`, `flannel` or `none`; read [why Calico](#why-the-cni-default-is-calico) first                                                |
| `pod_cidr`                      | per CNI   | `192.168.0.0/16` for Calico, `10.244.0.0/16` otherwise                                                                                |
| `calico_version`                | `v3.32.2` | pinned, so the CNI does not depend on the day the cluster was built                                                                   |
| `advertise_address`             | detected  | API server address workers dial; default is the master's default-route source address                                                 |
| `control_plane_endpoint`        | —         | `host:port` when a load balancer fronts several control planes                                                                        |
| `single_node`                   | `false`   | remove the control-plane taint so workloads schedule on the master                                                                    |
| `open_firewall`                 | `false`   | open the kubeadm and CNI ports in an active `ufw` or `firewalld`                                                                      |
| `kubelet_housekeeping_interval` | `1s`      | how often the kubelet's cAdvisor refreshes container stats; see [Deploying Prometheus and Grafana](#deploying-prometheus-and-grafana) |

`system.json` holds the upstream URLs and the join-file path; every field has a
default and the file may be omitted.

#### Why the CNI default is Calico

ARIES isolates each task sandbox with a deny-all Kubernetes `NetworkPolicy`, so
that concurrent tasks cannot reach each other or the internet. NetworkPolicy is
enforced by the **CNI plugin**, not by Kubernetes itself, and flannel does not
implement it.

The failure mode is the dangerous kind. Under flannel the API server accepts
every policy, `kubectl get netpol -n aries` lists them all, and not one of them
does anything — task pods stay fully connected while the cluster reports that
they are isolated. Calico enforces them. Pick `"cni": "flannel"` only for a
cluster where you do not need task isolation, and know that you have given it
up.

### Whole cluster in one command

Run from the repository root, so `create_cluster` can build the Linux node
binary for you:

```sh
cp k8s/setup/configs/cluster.json.example k8s/setup/configs/cluster.json
$EDITOR k8s/setup/configs/cluster.json
ssh-add ~/.ssh/id_ed25519                         # if your key has a passphrase
go run ./k8s/setup/aries-setup create_cluster --check   # preflight only; changes nothing
go run ./k8s/setup/aries-setup create_cluster
```

What it does:

1. **Preflight every node first** — non-interactive SSH, passwordless `sudo`,
   and a CPU architecture matching the node binary, checked on all hosts before
   anything is modified, so a typo in the last worker does not leave the first
   one half-installed.
2. Build `aries-setup` for linux and stage it with `kube.json` and
   `system.json` on each host (tar over SSH, into `~/.aries-setup`).
3. Run `setup_node` and `setup_master_node` on the control plane, streaming
   progress.
4. Mint a fresh join token with `kubeadm token create --print-join-command`.
5. Run `setup_node` and `setup_worker` on **every worker in parallel**. One
   worker failing does not abort the others; failures are collected and
   reported together.
6. Run `setup_prometheus` on the master when `deploy_prometheus` is set, then
   `setup_aries` when `deploy_aries` is set.
7. Print `kubectl get nodes -o wide`, and fetch the kubeconfig when
   `fetch_kubeconfig` is set.
8. Run `setup_runner` when `runner` is set and the ARIES chart was deployed.

| Flag                    | Meaning                                                                                                                                                                                                             |
| ----------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--check`               | Run preflight only, then stop without changing anything.                                                                                                                                                            |
| `--reset`               | Reset every node first. **Destroys all cluster state**, including hostPath volume contents. Needed to rebuild a live cluster: `kubeadm init` refuses an initialised control plane.                                  |
| `--skip-master`         | Join workers to a control plane that already exists.                                                                                                                                                                |
| `--skip-workers`        | Leave the existing workers untouched — no preflight, no token, no join. With `--skip-master` a run reduces to the chart deployments and `setup_runner`, which is how the charts are re-installed on a live cluster. |
| `--node-binary PATH`    | Ship a prebuilt linux binary instead of building one.                                                                                                                                                               |
| `--node-arch ARCH`      | Node GOARCH. Default `amd64`.                                                                                                                                                                                       |
| `--kubeconfig-out PATH` | Where `fetch_kubeconfig` writes. Default `k8s/setup/kubeconfig`.                                                                                                                                                    |
| `--charts-dir DIR`      | Where the Helm charts live. Default `k8s`.                                                                                                                                                                          |

SSH runs with `BatchMode=yes`, so your key must work without a prompt. Nodes
need passwordless `sudo`, since every step runs unattended. CloudLab nodes
provide both.

Every command and its output is appended to `aries-setup-<subcommand>.log` in
the working directory (on nodes, in `~/.aries-setup`). The logs are gitignored;
they contain the short-lived bootstrap token. The admin kubeconfig fetched by
`fetch_kubeconfig` is deliberately kept out of them, and written `0600`.

Re-running is safe: an initialised control plane skips `kubeadm init`, and an
already-joined worker refuses to join again rather than corrupting itself.

To re-install the charts on a cluster that already exists, without touching
its nodes:

```sh
go run ./k8s/setup/aries-setup create_cluster --skip-master --skip-workers
```

`--skip-workers` matters here. Without it the run reaches the join step and
`setup_worker` refuses every node with _this node already belongs to a cluster_
— correctly, since re-running `kubeadm join` against a live worker would
disrupt it. With both flags the run preflights only the master, re-stages it,
and installs the charts.

### By hand

vHive's setup tool does no SSH — you run each subcommand on the node yourself.
`aries-setup` supports exactly that:

```sh
# on your machine: build the node binary and copy it with the configs
make setup-tool
scp bin/aries-setup-linux-amd64 k8s/setup/configs/{kube,system}.json <node>:

# on every node
sudo ./aries-setup-linux-amd64 --configs-dir . setup_node

# on the control plane — prints the join command at the end
sudo ./aries-setup-linux-amd64 --configs-dir . setup_master_node

# on each worker
sudo ./aries-setup-linux-amd64 --configs-dir . setup_worker --join "kubeadm join ..."
```

The join token expires after 24 hours; mint a new one on the control plane with
`sudo kubeadm token create --print-join-command`. It is also saved at
`/etc/aries/kubeadm-join.sh`. The parts can be passed separately with
`--api-server`, `--token` and `--discovery-hash`. The join command is parsed and
re-rendered rather than executed as given, so only fields matching kubeadm's
formats reach the shell.

The runner host is set up separately, from your machine, with `setup_runner`
(see [The runner host](#the-runner-host)).

#### What `setup_node` does

1. Detect the distribution (Debian/Ubuntu → `apt`, RHEL/Fedora/Rocky → `dnf`).
2. Resolve the release channel from `https://dl.k8s.io/release/stable.txt`
   unless `k8s_minor` or `k8s_version` pins it.
3. Disable swap, in the running kernel and in `/etc/fstab` (backed up first).
4. Load `overlay` + `br_netfilter` and set the bridge/forwarding sysctls.
5. Install `containerd.io` from `download.docker.com`, write a default CRI
   config with `SystemdCgroup = true`, and point `crictl` at its socket.
6. Install `kubelet`, `kubeadm` and `kubectl` from `pkgs.k8s.io`, held at the
   installed version so a distro upgrade cannot skew the node.
7. Write the kubelet's extra arguments (`kubelet_housekeeping_interval`) to
   `/etc/default/kubelet`, restarting the kubelet if it is already configured.

`setup_master_node` then pre-pulls the control-plane images, runs `kubeadm
init`, installs the kubeconfig for `root` and for the `sudo` invoker, applies
the CNI, and writes the worker join command. `setup_worker` opens the worker
firewall ports when asked and runs `kubeadm join`.

### Resetting a node

```sh
sudo ./aries-setup-linux-amd64 --configs-dir . reset_node --yes
```

This runs `kubeadm reset`, removes the CNI/kubelet/kubeconfig state, deletes the
leftover `cni0`/`flannel.1`/`vxlan.calico` interfaces and flushes the iptables
rules kube-proxy left behind. It destroys all cluster state on that node —
including etcd on a control plane — and keeps containerd and the kube packages
installed so a re-bootstrap is fast. `create_cluster --reset` does this on every
node, workers first.

### Requirements

- A 64-bit Debian/Ubuntu or RHEL-family host, 2 CPUs and 2 GB RAM minimum per
  node (kubeadm preflight enforces this).
- Outbound network access to `dl.k8s.io`, `pkgs.k8s.io`, `download.docker.com`,
  `registry.k8s.io`, GitHub, and — for Prometheus — `get.helm.sh` for the Helm
  binary and `ghcr.io` for the kube-prometheus-stack chart.
- Unique hostname, MAC address and product UUID per node.
- Workers must reach the control plane on TCP 6443.
- On your machine: Go (to build the node binary), `ssh` and `tar`.

## No node roles

Nodes carry no ARIES labels or taints. Harness pods, sandbox pods, the tool
bridge pod and the monitoring stack all schedule wherever the scheduler finds
room, so workloads mix on every worker. The control plane keeps kubeadm's own
`node-role.kubernetes.io/control-plane` taint and runs none of them.

Profiles can still pin pods with `harness.node_role` and `sandbox.node_role`,
which select and tolerate `aries.dev/role=<role>`. Nothing labels nodes that way
any more, so an experiment that wants dedicated pools labels and taints them
itself:

```sh
kubectl label node <node> aries.dev/role=sandbox
kubectl taint node <node> aries.dev/role=sandbox:NoSchedule
```

## The runner host

The ARIES runner is a process, not a pod, and its host is deliberately outside
the cluster: the runner generates the load, and keeping it off the nodes whose
pods it measures keeps its CPU out of their numbers. It reaches the cluster
only through the API server, with `kubectl`. The tool bridge, which does sit on
the measured path, runs in the cluster as its own pod (see
[Architecture](#architecture)).

`setup_runner` prepares that host from your machine. `create_cluster` runs it
last when `runner` is set; run it on its own after changing runner code or
profiles:

```sh
go run ./k8s/setup/aries-setup setup_runner
```

It:

1. Checks the host is reachable with passwordless `sudo`, and **refuses it if it
   is a cluster node** or still runs a kubelet — `reset_node --yes` it first.
2. Installs `git` and a `kubectl` matching the cluster's server version, checked
   against its published SHA-256.
3. Builds `bin/aries` and `bin/aries-ssh` for the host and stages them with
   `profiles/` and `configs/` under `~/<runner_dir>`. Only those three
   directories are replaced; `runs/`, `.cache/` and the model key are kept.
4. Writes `~/<runner_dir>/kubeconfig` (0600) for the chart's `aries`
   ServiceAccount, from its long-lived token Secret. The runner therefore holds
   the namespaced Role, not cluster-admin; the step fails if the kubeconfig can
   create pods in `kube-system`. The token never appears on a command line or in
   the setup log.
5. From the host, confirms it can see the bridge pod.

The model key is never copied. Put it there yourself:

```sh
scp DEEPSEEK_API.key <runner>:aries/ && ssh <runner> chmod 600 aries/DEEPSEEK_API.key
```

## Credentials

None, by default. The bridge image `jingxiang212/aries-bridge` is public — it
holds only the bridge binary and `kubectl` — so the chart deploys without a
pull secret. For a private registry, copy `aries/secret.yaml.example` to the
gitignored `aries/secret.yaml`, fill in the registry document, and pass it last
with `registry.create=true` and `imagePullSecrets`. A registry document in
`secret.yaml` is rendered from values, so it is also in the Helm release
history and readable with `helm get values aries`.

The model key is **not** in the chart. The runner reads `DEEPSEEK_API.key`
beside its binaries on the runner host and stages it into each task's agent pod
itself, so it never sits in a Kubernetes Secret or in Helm's release history.

The runner authenticates with the `aries` ServiceAccount's long-lived token
(Secret `aries-token`), which `setup_runner` turns into a kubeconfig on the
runner host. Delete that Secret to revoke the runner's access.

## Deploying ARIES

### Build and publish the bridge image

```sh
make image-bridge BRIDGE_IMAGE=jingxiang212/aries-bridge:latest   # Dockerfile.bridge
docker push jingxiang212/aries-bridge:latest
```

The image holds only `aries-bridge` and `kubectl`: no profiles, no runner, no
key. Runner and profile changes never need it rebuilt; bridge changes do.

### With `aries-setup`

`setup_aries` installs the `aries` chart from the control plane, switched on
with `"deploy_aries": true` in `cluster.json`. It runs where `setup_prometheus`
runs and for the same reason: the admin kubeconfig and the charts are already
there, so you never have to log in to the master or keep a kubeconfig locally
to deploy. `pullPolicy: Always` makes each deploy pick up a newly pushed
`:latest`.

Only `Chart.yaml`, `values.yaml` and the files in `values_files` are shipped to
the master; any other `*.yaml` beside the chart, such as a `secret.yaml` that is
not in use, stays on your machine. `aries_config.json` controls the rest:

| Key            | Default               | Meaning                                                |
| -------------- | --------------------- | ------------------------------------------------------ |
| `namespace`    | `aries`               | Helm release namespace, created if absent              |
| `release`      | `aries`               | Helm release name                                      |
| `values_files` | `values-cluster.yaml` | applied in order; a `secret.yaml`, if added, goes last |
| `timeout`      | `10m`                 | passed to `helm --timeout`                             |

### With Helm directly

On a remote cluster:

```sh
helm upgrade --install aries ./k8s/aries \
  --namespace aries --create-namespace -f ./k8s/aries/values-cluster.yaml
```

`values-cluster.yaml` pulls the public image with `pullPolicy: Always`, so a
redeploy picks up a newly pushed `:latest`. Then set up the runner host with
`setup_runner` ([The runner host](#the-runner-host)).

Without a registry, import the image into each worker's containerd and use a
gitignored `values-local-<name>.yaml` with `bridge.image.pullPolicy: Never`:

```sh
docker save aries-bridge:dev | ssh <worker> 'sudo ctr -n k8s.io images import -'
```

On a local cluster (kind/minikube):

```sh
make image-bridge BRIDGE_IMAGE=aries-bridge:latest
kind load docker-image aries-bridge:latest    # or: minikube image load aries-bridge:latest
helm upgrade --install aries ./k8s/aries --namespace aries --create-namespace \
  -f ./k8s/aries/values-local.yaml
```

### Useful values

| Value                              | Default                            | What it does                                                                   |
| ---------------------------------- | ---------------------------------- | ------------------------------------------------------------------------------ |
| `bridge.enabled`                   | `true`                             | The tool bridge pod; needed by every `bridge.deployment: "kubernetes"` profile |
| `bridge.image.repository` / `.tag` | `jingxiang212/aries-bridge:latest` | The bridge image (public)                                                      |
| `bridge.networkPolicy.enabled`     | `true`                             | Admit only ARIES harness pods to the bridge                                    |
| `bridge.resources`                 | 1–4 CPU                            | Kept clear of throttling: a throttled bridge adds latency to every tool call   |
| `serviceAccount.create`            | `true`                             | The runner's ServiceAccount and its token Secret                               |
| `rbac.nodeMetrics`                 | `true`                             | The `nodes/proxy` ClusterRole for pod telemetry                                |
| `openclaw.enabled`                 | `false`                            | The static OpenClaw gateway Deployment + Service                               |

## Deploying Prometheus and Grafana

`setup_prometheus` installs
[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
from the control plane, switched on with `"deploy_prometheus": true` in
`cluster.json`. It follows the vHive loader's
[`setup_prometheus.go`](https://github.com/vhive-serverless/loader/blob/main/scripts/setup/cluster/setup_prometheus.go)
— install Helm, create the `monitoring` namespace, install the chart. One
release brings up both Prometheus and Grafana, because Grafana is a subchart.
By hand, it is:

```sh
helm upgrade --install prometheus \
  oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack:72.6.2@sha256:b6b5993a143594475fcaa3d5fff74382f06be02cae491b80f0cf6d1b68f9ec6c \
  --namespace monitoring --create-namespace \
  -f ./k8s/prometheus/values.yaml -f ./k8s/grafana/values.yaml \
  --wait --timeout 15m
```

**The chart is pinned, not committed.** `prom_config.json` names the chart's
OCI repository, `chart_version` and `chart_digest`, and the install uses all
three as one reference. Helm refuses it unless the tag resolves to that digest,
so the version the values were checked against and the bytes installed cannot
drift apart, and a re-pushed tag cannot change what is installed.

To move to a new version:

```sh
V=<new-version>
helm show values oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack --version $V  # re-check our keys
helm pull oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack --version $V         # prints the Digest
```

Then update `chart_version` and `chart_digest` together, and re-check
`prometheus/values.yaml` and `grafana/values.yaml` against the new chart: Helm
ignores values keys a chart does not define, so a renamed key drops an override
silently.

To run `setup_prometheus` alone on a cluster that already exists, either use
`create_cluster --skip-master --skip-workers`, or stage it on the master
yourself:

```sh
make setup-tool
ssh <master> 'mkdir -p aries-setup/configs'
scp bin/aries-setup-linux-amd64 <master>:aries-setup/
scp -r k8s/setup/configs/prometheus <master>:aries-setup/configs/
ssh <master> 'mkdir -p aries-setup/charts/prometheus aries-setup/charts/grafana'
scp k8s/prometheus/values.yaml <master>:aries-setup/charts/prometheus/
scp k8s/grafana/values.yaml <master>:aries-setup/charts/grafana/
ssh <master> 'cd aries-setup && sudo ./aries-setup-linux-amd64 \
  --configs-dir configs --charts-dir charts setup_prometheus'
```

**Placement.** Nothing pins the monitoring pods: nodes carry no role taints, so
Prometheus, Alertmanager, Grafana, the operator and kube-state-metrics schedule
on any worker, beside the pods they measure. node-exporter is a DaemonSet and
runs on every node.

**Scrape resolution.** cAdvisor is scraped every second; every other target
keeps the 15-second global interval. A 1-second scrape only helps if the
kubelet refreshes container stats as often, which is what
`kubelet_housekeeping_interval` in `kube.json` sets on every node. The ARIES
dashboard panels use a 1-second minimum interval to match.

### Reaching Grafana and Prometheus

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

Prometheus and Grafana both store data in an `emptyDir`, Prometheus with 7 days'
retention, so history does not survive the pod being rescheduled. Grafana's
dashboards and datasource are re-created from ConfigMaps on every restart, so
only ad-hoc UI state is lost.

ARIES exposes no Prometheus endpoint of its own; pod CPU and memory come from
the kubelet/cAdvisor scrapes this stack already performs. See
[the Kubernetes design](../docs/design/kubernetes.md#resource-telemetry) for what telemetry ARIES records itself.

### Where this differs from the vHive loader

| Loader step                                                                   | Here                                                                                                                                                                 |
| ----------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `curl …/helm/master/scripts/get-helm-3 \| bash`                               | A pinned Helm release whose tarball is checked against the SHA-256 in `prom_config.json` before it is unpacked.                                                      |
| `helm install`, `kubectl create namespace`                                    | `helm upgrade --install --create-namespace`, so a re-run does not fail.                                                                                              |
| `helm repo add` then install by chart name                                    | The chart is pulled from its OCI registry by version and digest, both pinned in `prom_config.json`.                                                                  |
| `loader-nodetype` node affinity                                               | No placement; ARIES nodes carry no role labels.                                                                                                                      |
| `perf_event_paranoid=-1` on every node                                        | Not done. It would let task containers on the sandbox node read host-wide perf events, and the perf collector that needs it is commented out in the loader's values. |
| Re-render controller-manager, scheduler, kube-proxy with `kubeadm init phase` | Not done; those scrapes (and etcd's) are disabled instead. The loader's `kubeadm_init.yaml` hardcodes a pod subnet and version that do not match an ARIES cluster.   |
| metrics-server, pushgateway, Knative monitors and dashboards                  | Not installed. ARIES has no Knative or Istio, and its own pod telemetry reads the kubelet Summary API.                                                               |
| Port-forwards kept running in tmux on the master                              | Grafana is a NodePort on 30300, printed as a URL; Prometheus stays ClusterIP with a forward command.                                                                 |
| Grafana password `prom-operator` (chart default)                              | Generated by the chart and stored in the `prometheus-grafana` Secret.                                                                                                |

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

## Running an experiment

Runs start on the runner host, not in the cluster:

```sh
ssh <runner>
cd ~/aries && export KUBECONFIG=$PWD/kubeconfig
./bin/aries profiles/hermes-tb2-fix-git-deepseek-k8s-pods.json
```

The `-k8s-pods` profiles put the agent, the bridge and the sandbox in pods:

```json
"harness": { "type": "hermes", "deployment": "kubernetes", "namespace": "aries" },
"sandbox": { "type": "kubernetes", "namespace": "aries" },
"bridge":  { "type": "hermes-ssh", "deployment": "kubernetes", "namespace": "aries" }
```

Run artifacts land in `~/aries/runs/` on the runner host, including each task's
`bridge/tool-calls.jsonl`, collected from the bridge pod after revocation.
Editing a profile, or rebuilding the runner with `setup_runner`, takes effect
on the next run; only a bridge change needs a new image.

A run is tied to your SSH session; use `tmux` or
`nohup ./bin/aries ... > runs/last.log 2>&1 &` for long experiments.

### Running tasks concurrently

`execution.concurrency` in the profile bounds how many task occurrences run at
once. Each concurrent task gets its own agent pod and sandbox pod, placed
wherever the scheduler finds room, and its own grant on the one bridge pod:

```json
"execution": { "concurrency": 4 }
```

Size it against the workers' capacity: N concurrent tasks means N agent pods
and N sandbox pods, each with the CPU and memory its `task.toml` requests.
Overshoot and pods sit `Pending` on `Insufficient cpu`.

## Status and caveats

### ARIES on Kubernetes

- A Kubernetes-native task **sandbox** exists (`pkg/sandbox/kubernetes`,
  selected by `sandbox.type: "kubernetes"`). It drives the cluster via the
  `kubectl` binary: one pod per task, `kubectl exec` for
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
  This requires a CNI that implements NetworkPolicy: `aries-setup` defaults to
  **Calico**, and under flannel the policies are created and silently ignored.
- **Pod resource metrics** are read from the kubelet Summary API — no
  metrics-server needed. This needs the `aries-node-metrics` ClusterRole
  (`get` on `nodes/proxy`), the only permission the runner holds outside its
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
- **One bridge pod.** A restart of it ends every task in flight: their access
  is provably revoked, but their tool-call logs are lost and evaluation is
  blocked for them. Grants are sharded by nothing yet, so there are no replicas.

### Cluster bootstrap

- **Image pulls can be rate-limited.** `registry.k8s.io` is served by Google
  Artifact Registry, which throttles by source IP. A throttled worker never
  starts kube-proxy, so Calico cannot reach the API server Service and the node
  stays NotReady with `cni plugin not initialized`. `kubectl -n kube-system
describe pod <kube-proxy pod>` shows `429 Too Many Requests`; deleting the
  kube-proxy and then the calico-node pod on that node retries once the limit
  lifts.
- The Flannel manifest hard-codes `10.244.0.0/16`; a different `pod_cidr` needs
  a patched manifest. `setup_master_node` warns instead of failing.
- Only single control-plane clusters are bootstrapped end to end. For HA, set
  `control_plane_endpoint` and join the additional control-plane nodes manually
  with the certificate key `kubeadm init` prints.
- `open_firewall` opens the standard kubeadm port set plus the CNI's ports; it
  does not touch cloud provider security groups.
