# Automated Kubernetes install (kubeadm)

Scripts that turn bare Linux hosts into a Kubernetes cluster ARIES can run on.
Nothing is vendored: the kube packages, the container runtime and the CNI
manifests are all downloaded from their upstream sources at run time.

```
k8s/install/
  install-cluster.sh    # orchestrator: whole cluster from one machine over SSH
  cluster.conf.example  # topology template for install-cluster.sh
  common.sh             # shared node preparation (sourced, never run directly)
  install-master.sh     # control-plane node: prepare + kubeadm init + CNI
  install-worker.sh     # worker node: prepare + kubeadm join
  reset-node.sh         # tear a node back down to a pre-kubeadm state
```

There are two ways to use this. **Orchestrated** — describe the nodes in a
config file and run one command from your laptop
([Whole cluster in one command](#whole-cluster-in-one-command)). **Per node** —
log into each host and run the installer there
([Control-plane node](#control-plane-node)). The orchestrator drives exactly the
same per-node scripts over SSH, so the two produce identical clusters.

## Whole cluster in one command

```sh
cd k8s/install
cp cluster.conf.example cluster.conf     # cluster.conf is gitignored
$EDITOR cluster.conf                     # set MASTER and WORKERS
./install-cluster.sh --config cluster.conf
```

`cluster.conf` is sourced as bash, so it is shell syntax rather than JSON:

```sh
MASTER="JXiang@node0.myexp.ntu-cloud-pg0.clemson.cloudlab.us"

ARIES_NODES=(   "JXiang@node1.myexp..." )   # runs the ARIES Job
HARNESS_NODES=( "JXiang@node2.myexp..." )   # runs the agent gateway pods
SANDBOX_NODES=( "JXiang@node3.myexp..." )   # runs the task sandbox pods
WORKERS=(       "JXiang@node4.myexp..." )   # general purpose, untainted

CNI="calico"
TAINT_NODES=1
```

### Why the CNI default is Calico

ARIES isolates each task sandbox with a deny-all Kubernetes `NetworkPolicy`, so
that concurrent tasks cannot reach each other or the internet. NetworkPolicy is
enforced by the **CNI plugin**, not by Kubernetes itself, and flannel does not
implement it.

The failure mode is the dangerous kind. Under flannel the API server accepts
every policy, `kubectl get netpol -n aries` lists them all, and not one of them
does anything — task pods stay fully connected while the cluster reports that
they are isolated. Calico enforces them. Pick `CNI=flannel` only for a cluster
where you do not need task isolation, and know that you have given it up.

Only assignments belong in it — anything else runs on your machine as you. The
remaining keys (`SSH_KEY`, `SSH_OPTS`, `POD_CIDR`, `K8S_VERSION`,
`ADVERTISE_ADDRESS`, `SINGLE_NODE`, `OPEN_FIREWALL`, `FETCH_KUBECONFIG`) are
documented inline in `cluster.conf.example`. Listing the master under a worker
pool as well is harmless — it is filtered out, and the pools are deduplicated.

## Dedicated nodes by role

Every node in a role pool is joined as a worker, then given **both** a label and
a taint:

| Pool | Label | Taint |
| --- | --- | --- |
| `ARIES_NODES` | `aries.dev/role=aries` | `aries.dev/role=aries:NoSchedule` |
| `HARNESS_NODES` | `aries.dev/role=harness` | `aries.dev/role=harness:NoSchedule` |
| `SANDBOX_NODES` | `aries.dev/role=sandbox` | `aries.dev/role=sandbox:NoSchedule` |
| `MASTER` | `aries.dev/role=master` | kubeadm's own `node-role.kubernetes.io/control-plane` |
| `WORKERS` | — | — |

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
labels with that prefix, so without it the pools are invisible unless you ask for
them explicitly:

```sh
kubectl get nodes -L aries.dev/role     # works with or without the extra label
kubectl get nodes                       # ROLES column, thanks to the extra label
```

Scheduling keys off `aries.dev/role` either way; nothing selects on the cosmetic
label.

The Kubernetes node name is read from each host with `hostname` rather than
derived from the SSH target — on CloudLab you connect to `clnodeNNN.<site>` but
the node joins the cluster as `nodeN.<experiment>.<project>.<site>`, and taints
must use the latter.

Both operations use `--overwrite`, so re-running the installer re-applies roles
cleanly and a node can be moved between pools by editing the config and running
it again.

> **Set `TAINT_NODES=0` for a first bring-up.** Once nodes are tainted, any pod
> without a matching toleration is unschedulable. If every worker carries a role
> taint and your pod specs do not yet tolerate them, nothing will schedule at
> all. Bring the cluster up untainted, confirm ARIES runs, then turn roles on.

What the orchestrator does:

1. **Preflight every node first** — SSH reachability and passwordless `sudo`,
   checked on all hosts before anything is modified, so a typo in the last
   worker does not leave the first one half-installed.
2. Ship the per-node scripts to each host (tar over SSH, into `~/.aries-k8s-install`).
3. Run `install-master.sh` on the control plane.
4. Mint a fresh join token with `kubeadm token create --print-join-command`.
5. Run `install-worker.sh` on **every worker in parallel**, each logging to its
   own file. One worker failing does not abort the others; failures are
   collected and reported together with the tail of the offending log.
6. Label and taint each role pool from the master (skipped when `TAINT_NODES=0`).
7. Print `kubectl get nodes -o wide -L aries.dev/role` from the master, so the
   role column confirms what landed where.

Useful flags:

| Flag | Meaning |
| --- | --- |
| `--config FILE` | Topology file. Default `./cluster.conf`. |
| `--check` | Run preflight only, then stop without changing anything. |
| `--skip-master` | Join workers to a control plane that already exists. |

Check connectivity before committing to an install:

```sh
./install-cluster.sh --config cluster.conf --check
```

Requirements on your machine: `ssh`, `scp`, and `tar`. Nothing is installed
locally. Requirements on each node: SSH access with a key (no password prompt)
and passwordless `sudo` — the install runs unattended and cannot answer a
prompt. CloudLab nodes satisfy both by default.

Re-running is safe: an initialized control plane skips `kubeadm init`, and an
already-joined worker reports the fact and fails rather than corrupting itself.
To rebuild a node, run `reset-node.sh --yes` on it first — the orchestrator
stages that script alongside the others, so it is already at
`~/.aries-k8s-install/reset-node.sh`.

## What the scripts do

Both role scripts run the same preparation (`prepare_node` in `common.sh`):

1. detect the distribution (Debian/Ubuntu → `apt`, RHEL/Fedora/Rocky → `dnf`)
   and the architecture (`x86_64` or `aarch64`);
2. resolve the release channel from `https://dl.k8s.io/release/stable.txt`
   unless `K8S_MINOR` or `K8S_VERSION` pins it;
3. disable swap, in the running kernel and in `/etc/fstab`;
4. load `overlay` + `br_netfilter` and set the bridge/forwarding sysctls;
5. install `containerd.io` from `download.docker.com`, write a default CRI
   config with `SystemdCgroup = true`, and point `crictl` at its socket;
6. install `kubelet`, `kubeadm` and `kubectl` from `pkgs.k8s.io`, held at the
   installed version so a distro upgrade cannot skew the node.

`install-master.sh` then pre-pulls the control-plane images, runs `kubeadm
init`, installs the kubeconfig for `root` and for the `sudo` invoker, applies
the CNI, and writes a worker join command to `/etc/aries/kubeadm-join.sh`.

`install-worker.sh` runs `kubeadm join` with the command produced above.

Both scripts are re-runnable: an already-initialized control plane skips
`kubeadm init`, and an already-joined worker refuses to run rather than
corrupting its state.

## Requirements

- A 64-bit Debian/Ubuntu or RHEL-family host, 2 CPUs and 2 GB RAM minimum per
  node (kubeadm preflight enforces this).
- Root (the scripts are run with `sudo`).
- Outbound network access to `dl.k8s.io`, `pkgs.k8s.io`, `download.docker.com`,
  `registry.k8s.io` and GitHub.
- Unique hostname, MAC address and product UUID per node.
- Workers must reach the control plane on TCP 6443.

## Control-plane node

```sh
sudo ./k8s/install/install-master.sh
```

Useful flags:

| Flag | Meaning |
| --- | --- |
| `--advertise-address IP` | API server address workers connect to. Default: the source address of the default route. |
| `--control-plane-endpoint HOST:PORT` | Set when a load balancer fronts multiple control-plane nodes (implies `--upload-certs`). |
| `--cni flannel\|calico\|none` | Pod network. Default `calico`; see the note below before choosing flannel. |
| `--pod-cidr CIDR` | Default `192.168.0.0/16` (calico) or `10.244.0.0/16` (flannel). |
| `--k8s-version 1.34.2` | Pin an exact patch instead of the channel latest. |
| `--single-node` | Remove the control-plane taint so ARIES Jobs and task pods schedule here. |
| `--open-firewall` | Open the required ports in an active `ufw`/`firewalld`. |

For a one-machine research cluster:

```sh
sudo ./k8s/install/install-master.sh --single-node
```

## Worker nodes

Copy `k8s/install/` to each worker, then run the join command the control plane
printed:

```sh
sudo ./install-worker.sh --join "kubeadm join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
    --discovery-token-ca-cert-hash sha256:<hash>"
```

The token expires after 24 hours. Mint a new one on the control plane:

```sh
sudo kubeadm token create --print-join-command
```

The parts can also be passed separately with `--api-server`, `--token` and
`--discovery-hash`.

Verify from the control plane:

```sh
kubectl get nodes -o wide
```

## Running ARIES on the new cluster

```sh
docker build -t aries:latest .
# push to a registry both nodes can pull from, or load the image on each node
cd k8s/overlays/local && cp secret.env.example secret.env   # then edit
kubectl apply -k .
kubectl -n aries logs -f job/aries
```

Kubernetes-backed profiles select the in-cluster sandbox and harness — see the
[k8s README](../README.md) and `profiles/openclaw-tb2-fix-git-deepseek-k8s.json`.

## Resetting a node

```sh
sudo ./k8s/install/reset-node.sh --yes
```

This runs `kubeadm reset`, removes the CNI/kubelet/kubeconfig state, deletes the
leftover `cni0`/`flannel.1`/`vxlan.calico` interfaces and flushes the iptables
rules kube-proxy left behind. It destroys all cluster state on that node —
including etcd on a control plane — and keeps containerd and the kube packages
installed so a re-bootstrap is fast.

## Caveats

- The Flannel manifest hard-codes `10.244.0.0/16`; a different `--pod-cidr`
  needs a patched manifest. The script warns instead of failing.
- Calico is installed through the Tigera operator from the project's latest
  release, so the CNI version is not pinned by this repository.
- Only single control-plane clusters are bootstrapped end to end. For HA, pass
  `--control-plane-endpoint` and join the additional control-plane nodes
  manually with the certificate key `kubeadm init` prints.
- `--open-firewall` opens the standard kubeadm port set plus the CNI's ports; it
  does not touch cloud provider security groups.
