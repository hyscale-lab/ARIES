# Automated Kubernetes install (kubeadm)

Scripts that turn bare Linux hosts into a Kubernetes cluster ARIES can run on.
Nothing is vendored: the kube packages, the container runtime and the CNI
manifests are all downloaded from their upstream sources at run time.

```
k8s/install/
  common.sh           # shared node preparation (sourced, never run directly)
  install-master.sh   # control-plane node: prepare + kubeadm init + CNI
  install-worker.sh   # worker node: prepare + kubeadm join
  reset-node.sh       # tear a node back down to a pre-kubeadm state
```

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
| `--cni flannel\|calico\|none` | Pod network. Default `flannel`. |
| `--pod-cidr CIDR` | Default `10.244.0.0/16` (flannel) or `192.168.0.0/16` (calico). |
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
