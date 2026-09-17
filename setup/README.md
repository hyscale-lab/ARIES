# Automated Kubernetes install: `aries-setup`

`aries-setup` (`setup/aries-setup`) turns bare Linux hosts into a Kubernetes
cluster ARIES can run on. It is laid out like vHive's
[`scripts/setup`](https://github.com/vhive-serverless/vHive/tree/main/scripts/setup):
one binary, named subcommands, JSON configuration. The kube packages, the
container runtime and the CNI are downloaded from upstream at run time; the Helm
charts are the exception, committed under [`k8s/`](../k8s/README.md) so a
deployment needs no chart-repository access and matches what is in git.

| Subcommand | Does | Runs on |
|---|---|---|
| `setup_node` | install containerd and kubelet/kubeadm/kubectl | every node, as root |
| `setup_master_node` | `kubeadm init`, CNI, write a join command | control plane |
| `setup_worker --join "<cmd>"` | `kubeadm join` | each worker |
| `setup_prometheus` | install Prometheus + Grafana from the vendored chart | control plane |
| `setup_aries` | install the ARIES chart | control plane |
| `reset_node --yes` | tear the node back to a pre-kubeadm state | any node |
| `create_cluster` | all of the above over SSH, plus role labels and taints | your machine |

There are two ways to use it. **From your machine** — describe the nodes in
`cluster.json` and run one command
([Whole cluster in one command](#whole-cluster-in-one-command)). **By hand** —
run each on-node subcommand on its host yourself, which is how vHive's tool
works ([By hand](#by-hand)). `create_cluster` runs exactly the same on-node
subcommands over SSH, so both produce identical clusters.

## Configuration

```
setup/configs/
  kube.json              # Kubernetes version, CNI, pod CIDR, Calico pin  -> shipped to every node
  system.json            # upstream URLs and node paths (optional)        -> shipped to every node
  cluster.json.example   # SSH topology and role pools                    -> stays on your machine
  prometheus/
    prom_config.json     # chart version + Helm pins, monitoring role     -> shipped when deploy_prometheus
  aries/
    aries_config.json    # namespace, release, values file order          -> shipped when deploy_aries
```

The Helm charts themselves live in [`k8s/`](../k8s/README.md), not here: the
vendored `k8s/prometheus/chart`, its `values.yaml`, `k8s/grafana/values.yaml`,
and the `k8s/aries` chart. `--charts-dir` points at that tree and defaults to
`k8s`. Only the charts being deployed are shipped, and only to the master — the
vendored monitoring chart alone is 6.7MB.

`cluster.json` is gitignored and never copied to a node, since it names every
host and user. Unknown keys are rejected in every file, so a typo fails
immediately rather than silently taking a default.

### `cluster.json`

| Key | Meaning |
|---|---|
| `master` | SSH target of the control plane, e.g. `JXiang@hp030.utah.cloudlab.us` |
| `aries_nodes`, `harness_nodes`, `sandbox_nodes` | role pools; see [Dedicated nodes by role](#dedicated-nodes-by-role) |
| `workers` | joined but neither labelled nor tainted |
| `skip_role_taints` | join every node as a plain worker |
| `ssh_key` | identity file; empty uses your ssh-agent and `~/.ssh/config` |
| `ssh_options` | extra ssh arguments, one per entry: `["-o", "ConnectTimeout=15"]` |
| `fetch_kubeconfig` | copy the admin kubeconfig to `setup/kubeconfig` |
| `deploy_prometheus` | run `setup_prometheus` once roles are applied |
| `deploy_aries` | run `setup_aries` once roles are applied; needs a pullable image and a filled-in `k8s/aries/secret.yaml` |

Every SSH target is validated as `[user@]host`: a value starting with `-` would
be read by ssh as an option. Listing the master in a pool is harmless — it is
filtered out — and a node in several pools is installed once.

### `kube.json`

| Key | Default | Meaning |
|---|---|---|
| `k8s_version` | stable | exact patch, e.g. `1.34.2` |
| `k8s_minor` | derived | package channel, e.g. `v1.34` |
| `cni` | `calico` | `calico`, `flannel` or `none`; read [why Calico](#why-the-cni-default-is-calico) first |
| `pod_cidr` | per CNI | `192.168.0.0/16` for Calico, `10.244.0.0/16` otherwise |
| `calico_version` | `v3.32.2` | pinned, so the CNI does not depend on the day the cluster was built |
| `advertise_address` | detected | API server address workers dial; default is the master's default-route source address |
| `control_plane_endpoint` | — | `host:port` when a load balancer fronts several control planes |
| `single_node` | `false` | remove the control-plane taint so workloads schedule on the master |
| `open_firewall` | `false` | open the kubeadm and CNI ports in an active `ufw` or `firewalld` |

`system.json` holds the upstream URLs and the join-file path; every field has a
default and the file may be omitted.

### Why the CNI default is Calico

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

## Whole cluster in one command

Run from the repository root, so `create_cluster` can build the Linux node
binary for you:

```sh
cp setup/configs/cluster.json.example setup/configs/cluster.json
$EDITOR setup/configs/cluster.json
ssh-add ~/.ssh/id_ed25519                         # if your key has a passphrase
go run ./setup/aries-setup create_cluster --check   # preflight only; changes nothing
go run ./setup/aries-setup create_cluster
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
6. Label and taint each role pool from the master (skipped with
   `skip_role_taints`).
7. Run `setup_prometheus` on the master when `deploy_prometheus` is set, then
   `setup_aries` when `deploy_aries` is set.
8. Print `kubectl get nodes -o wide -L aries.dev/role`, and fetch the
   kubeconfig when `fetch_kubeconfig` is set.

| Flag | Meaning |
|---|---|
| `--check` | Run preflight only, then stop without changing anything. |
| `--reset` | Reset every node first. **Destroys all cluster state**, including hostPath volume contents. Needed to rebuild a live cluster: `kubeadm init` refuses an initialised control plane. |
| `--skip-master` | Join workers to a control plane that already exists. |
| `--skip-workers` | Leave the existing workers untouched — no preflight, no token, no join. With `--skip-master` a run reduces to the role labels and the chart deployments, which is how the charts are re-installed on a live cluster. |
| `--node-binary PATH` | Ship a prebuilt linux binary instead of building one. |
| `--node-arch ARCH` | Node GOARCH. Default `amd64`. |
| `--kubeconfig-out PATH` | Where `fetch_kubeconfig` writes. Default `setup/kubeconfig`. |
| `--charts-dir DIR` | Where the Helm charts live. Default `k8s`. |

SSH runs with `BatchMode=yes`, so your key must work without a prompt. Nodes
need passwordless `sudo`, since every step runs unattended. CloudLab nodes
provide both.

Every command and its output is appended to `aries-setup-<subcommand>.log` in
the working directory (on nodes, in `~/.aries-setup`). The logs are gitignored;
they contain the short-lived bootstrap token. The admin kubeconfig fetched by
`fetch_kubeconfig` is deliberately kept out of them, and written `0600`.

Re-running is safe: an initialised control plane skips `kubeadm init`, and an
already-joined worker refuses to join again rather than corrupting itself.

## Dedicated nodes by role

Every node in a role pool is joined as a worker, then given **both** a label and
a taint:

| Pool | Label | Taint |
| --- | --- | --- |
| `aries_nodes` | `aries.dev/role=aries` | `aries.dev/role=aries:NoSchedule` |
| `harness_nodes` | `aries.dev/role=harness` | `aries.dev/role=harness:NoSchedule` |
| `sandbox_nodes` | `aries.dev/role=sandbox` | `aries.dev/role=sandbox:NoSchedule` |
| `master` | `aries.dev/role=master` | kubeadm's own `node-role.kubernetes.io/control-plane` |
| `workers` | — | — |

A node listed in several pools takes the last one, since it can hold only one
`aries.dev/role`.

**Both halves are required, and they do different jobs.** The taint keeps
unrelated pods *off* the node. The label is what lets a pod ask *for* it. A
taint on its own cannot pin a pod anywhere: a sandbox pod that merely tolerates
the sandbox taint is still free to schedule onto an untainted general worker. So
a pod that must land on a dedicated node needs both:

```yaml
nodeSelector:
  aries.dev/role: sandbox
tolerations:
  - key: aries.dev/role
    operator: Equal
    value: sandbox
    effect: NoSchedule
```

Each role node also gets a matching `node-role.kubernetes.io/<role>` label. That
one is purely cosmetic: `kubectl get nodes` builds its `ROLES` column *only* from
labels with that prefix. Scheduling keys off `aries.dev/role`; nothing selects on
the cosmetic label.

The Kubernetes node name is read from each host with `hostname` rather than
derived from the SSH target — on CloudLab you connect to `hpNNN.<site>` but the
node joins the cluster as `nodeN.<experiment>.<project>.<site>`, and taints must
use the latter. Both operations use `--overwrite`, so re-running re-applies
roles cleanly and a node can be moved between pools by editing `cluster.json`.

> **Set `skip_role_taints` for a first bring-up.** Once nodes are tainted, any
> pod without a matching toleration is unschedulable. If every worker carries a
> role taint and your pod specs do not yet tolerate them, nothing will schedule
> at all.

## By hand

vHive's setup tool does no SSH — you run each subcommand on the node yourself.
`aries-setup` supports exactly that:

```sh
# on your machine: build the node binary and copy it with the configs
make setup-tool
scp bin/aries-setup-linux-amd64 setup/configs/{kube,system}.json <node>:

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

Role labels and taints are then yours to apply (see
[Dedicated nodes by role](#dedicated-nodes-by-role)).

### What `setup_node` does

1. Detect the distribution (Debian/Ubuntu → `apt`, RHEL/Fedora/Rocky → `dnf`).
2. Resolve the release channel from `https://dl.k8s.io/release/stable.txt`
   unless `k8s_minor` or `k8s_version` pins it.
3. Disable swap, in the running kernel and in `/etc/fstab` (backed up first).
4. Load `overlay` + `br_netfilter` and set the bridge/forwarding sysctls.
5. Install `containerd.io` from `download.docker.com`, write a default CRI
   config with `SystemdCgroup = true`, and point `crictl` at its socket.
6. Install `kubelet`, `kubeadm` and `kubectl` from `pkgs.k8s.io`, held at the
   installed version so a distro upgrade cannot skew the node.

`setup_master_node` then pre-pulls the control-plane images, runs `kubeadm
init`, installs the kubeconfig for `root` and for the `sudo` invoker, applies
the CNI, and writes the worker join command. `setup_worker` opens the worker
firewall ports when asked and runs `kubeadm join`.

## Prometheus

`setup_prometheus` installs
[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
from the control plane. It follows the vHive loader's
[`setup_prometheus.go`](https://github.com/vhive-serverless/loader/blob/main/scripts/setup/cluster/setup_prometheus.go)
— install Helm, create the `monitoring` namespace, install the chart — and is
switched on the same way, with `"deploy_prometheus": true` in `cluster.json`.

**Grafana comes with it.** Grafana is a subchart of kube-prometheus-stack, so
one release brings up both; `k8s/grafana/values.yaml` is a second values file
applied to that same release, not a second install. Keeping it bundled is what
supplies the Prometheus datasource and the default Kubernetes dashboards
without configuring either.

**The chart is vendored**, at `k8s/prometheus/chart`, so the install needs no
chart-repository access and cannot drift between runs. `chart_version` in
`prom_config.json` is checked against the chart's own `Chart.yaml` first and the
install is refused on a mismatch — Helm ignores values keys a chart does not
define, so a chart re-vendored without re-checking the values would silently
drop every override. See
[k8s/prometheus/VENDORED.md](../k8s/prometheus/VENDORED.md) to refresh it.

To add it to a cluster that already exists, run it on the master:

```sh
make setup-tool
ssh <master> 'mkdir -p aries-setup/configs'
scp bin/aries-setup-linux-amd64 <master>:aries-setup/
scp -r setup/configs/prometheus <master>:aries-setup/configs/
# COPYFILE_DISABLE and the exclude keep macOS AppleDouble files out; see VENDORED.md
ssh <master> 'mkdir -p aries-setup/charts'
COPYFILE_DISABLE=1 tar -C k8s --exclude '._*' -cf - prometheus grafana \
  | ssh <master> 'tar -C aries-setup/charts -xf -'
ssh <master> 'cd aries-setup && sudo ./aries-setup-linux-amd64 \
  --configs-dir configs --charts-dir charts setup_prometheus'
```

Or let `create_cluster` do the staging for you on a cluster that already
exists:

```sh
go run ./setup/aries-setup create_cluster --skip-master --skip-workers
```

`--skip-workers` matters here. Without it the run reaches the join step and
`setup_worker` refuses every node with *this node already belongs to a cluster*
— correctly, since re-running `kubeadm join` against a live worker would
disrupt it. With both flags the run preflights only the master, re-stages it,
re-applies the role labels, and installs the charts.

**Placement.** Prometheus, Alertmanager, Grafana, the operator and
kube-state-metrics run on the node with `aries.dev/role` equal to `node_role`
(default `aries`), keeping them off the harness and sandbox nodes being
measured. Each gets both a `nodeSelector` and a toleration for that role's
taint; without the toleration every pod stays Pending, since every ARIES role
node is tainted. node-exporter is a DaemonSet and runs on every node. The
master cannot be chosen: its taint is kubeadm's control-plane taint, not a role
taint.

**Where this differs from the loader**, and why:

| Loader step | Here |
|---|---|
| `curl …/helm/master/scripts/get-helm-3 \| bash` | A pinned Helm release whose tarball is checked against the SHA-256 in `prom_config.json` before it is unpacked. |
| `helm install`, `kubectl create namespace` | `helm upgrade --install --create-namespace`, so a re-run does not fail. |
| `helm repo add` then install by chart name | The chart is committed at `k8s/prometheus/chart` and installed from disk, with its version checked against `chart_version`. |
| `loader-nodetype` node affinity | `aries.dev/role` nodeSelector plus toleration, generated from `node_role`. |
| `perf_event_paranoid=-1` on every node | Not done. It would let task containers on the sandbox node read host-wide perf events, and the perf collector that needs it is commented out in the loader's values. |
| Re-render controller-manager, scheduler, kube-proxy with `kubeadm init phase` | Not done; those scrapes (and etcd's) are disabled instead. The loader's `kubeadm_init.yaml` hardcodes a pod subnet and version that do not match an ARIES cluster. |
| metrics-server, pushgateway, Knative monitors and dashboards | Not installed. ARIES has no Knative or Istio, and its own pod telemetry reads the kubelet Summary API. |
| Port-forwards kept running in tmux on the master | Grafana is a NodePort on 30300, printed as a URL; Prometheus stays ClusterIP with a forward command. |
| Grafana password `prom-operator` (chart default) | Generated by the chart and stored in the `prometheus-grafana` Secret. |

Reach it from your machine:

Grafana is a NodePort, so open `http://<any-node-ip>:30300` — `setup_prometheus`
prints the resolved URL when it finishes. That port listens on every node
interface, so on nodes with public addresses it is publicly reachable; a login
is required, and the password is generated rather than the chart's published
default. Prometheus stays cluster-internal.

```sh
# the generated Grafana password (user: admin)
kubectl -n monitoring get secret prometheus-grafana -o jsonpath='{.data.admin-password}' | base64 -d
# Prometheus, on demand
kubectl -n monitoring port-forward svc/prometheus-kube-prometheus-prometheus 9090
```

Prometheus and Grafana both store data in an `emptyDir`, Prometheus with 7 days'
retention, so history does not survive the pod being rescheduled. Grafana's
dashboards and datasource are re-created from ConfigMaps on every restart, so
only ad-hoc UI state is lost.

Helm ignores values keys a chart does not define, so re-vendoring the chart
means re-checking `k8s/prometheus/values.yaml` and `k8s/grafana/values.yaml`
against it — see [VENDORED.md](../k8s/prometheus/VENDORED.md).

## Deploying ARIES

`setup_aries` installs the [`k8s/aries`](../k8s/README.md) chart from the
control plane, switched on with `"deploy_aries": true` in `cluster.json`. It
runs where `setup_prometheus` runs and for the same reason: the admin kubeconfig
and the charts are already there, so you never have to log in to the master or
keep a kubeconfig locally to deploy.

Two things it cannot supply, both checked before Helm runs — and, for
`create_cluster`, before any node is touched:

1. **An image the cluster can pull.** Set `image.repository` in
   `k8s/aries/values-incluster.yaml` and push it first.
2. **`k8s/aries/secret.yaml`**, holding the model API key and, for a private
   image, the registry document. Copy `secret.yaml.example` and fill it in. It
   is gitignored, and `create_cluster` ships it to the master with the chart.

`aries_config.json` controls the rest:

| Key | Default | Meaning |
|---|---|---|
| `namespace` | `aries` | Helm release namespace, created if absent |
| `release` | `aries` | Helm release name |
| `values_files` | `values-incluster.yaml`, `secret.yaml` | applied in order, so put `secret.yaml` last |
| `timeout` | `10m` | passed to `helm --timeout` |

ARIES is pinned to the `aries` role pool by the chart, so `aries_nodes` must
have at least one entry — `create_cluster` refuses otherwise rather than leaving
the pod `Pending` behind the role taint.

Because the key is rendered from values, it is also in the Helm release
history and readable with `helm get values aries`. See
[k8s/README.md](../k8s/README.md#credentials) for the trade-off and how to avoid
it.

## Requirements

- A 64-bit Debian/Ubuntu or RHEL-family host, 2 CPUs and 2 GB RAM minimum per
  node (kubeadm preflight enforces this).
- Outbound network access to `dl.k8s.io`, `pkgs.k8s.io`, `download.docker.com`,
  `registry.k8s.io`, GitHub, and — for Prometheus — `get.helm.sh` for the Helm
  binary. The charts are vendored, so no chart repository is contacted.
- Unique hostname, MAC address and product UUID per node.
- Workers must reach the control plane on TCP 6443.
- On your machine: Go (to build the node binary), `ssh` and `tar`.

## Resetting a node

```sh
sudo ./aries-setup-linux-amd64 --configs-dir . reset_node --yes
```

This runs `kubeadm reset`, removes the CNI/kubelet/kubeconfig state, deletes the
leftover `cni0`/`flannel.1`/`vxlan.calico` interfaces and flushes the iptables
rules kube-proxy left behind. It destroys all cluster state on that node —
including etcd on a control plane — and keeps containerd and the kube packages
installed so a re-bootstrap is fast. `create_cluster --reset` does this on every
node, workers first.

## Caveats

- **Image pulls can be rate-limited.** `registry.k8s.io` is served by Google
  Artifact Registry, which throttles by source IP. A throttled worker never
  starts kube-proxy, so Calico cannot reach the API server Service and the node
  stays NotReady with `cni plugin not initialized`. `kubectl -n kube-system
  describe pod <kube-proxy pod>` shows `429 Too Many Requests`; deleting the
  kube-proxy and then the calico-node pod on that node retries once the limit
  lifts.
- **Copying the charts from macOS needs `COPYFILE_DISABLE=1`.** Every vendored
  chart file carries the unremovable `com.apple.provenance` xattr, and macOS
  `tar` turns each into a binary `._<file>` archive member. On the node those
  become real files, and `helm` parses one as a CRD, failing with `control
  characters are not allowed` for a file you never created. `create_cluster`
  handles this, and `setup_prometheus` refuses a chart directory containing
  `._*` files; by hand, see
  [k8s/prometheus/VENDORED.md](../k8s/prometheus/VENDORED.md).
- The Flannel manifest hard-codes `10.244.0.0/16`; a different `pod_cidr` needs
  a patched manifest. `setup_master_node` warns instead of failing.
- Only single control-plane clusters are bootstrapped end to end. For HA, set
  `control_plane_endpoint` and join the additional control-plane nodes manually
  with the certificate key `kubeadm init` prints.
- `open_firewall` opens the standard kubeadm port set plus the CNI's ports; it
  does not touch cloud provider security groups.
