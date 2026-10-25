# ARIES on Kubernetes

ARIES does not bootstrap clusters. Build one with
[Invitro's `scripts/setup`](https://github.com/vhive-serverless/invitro/tree/main/scripts/setup),
then apply ARIES on top with the script in this directory. For how ARIES places
its components on a cluster, and what that provider does not support, see
[the Kubernetes implementation](../docs/implementation/kubernetes.md).

```
k8s/
  create-cluster.sh     # runs Invitro's setup, then apply-aries.sh, in one go
  apply-aries.sh        # applies everything below to an existing cluster
  prune-invitro.sh      # removes the Invitro components ARIES does not use
  publish-images.sh     # builds and pushes the bridge and Hermes images
  aries/                # the ARIES chart: runner and bridge RBAC, dashboard
  monitoring/           # values overlay for an existing kube-prometheus-stack
```

## What ARIES needs from the cluster

| Requirement | Invitro's setup |
| --- | --- |
| A CNI that enforces NetworkPolicy | Calico |
| containerd | Yes |
| `kubectl` and `helm` on the machine that applies ARIES | On the master node; `helm` is installed with Prometheus |
| kube-prometheus-stack, only for the Grafana dashboard | Optional (`DEPLOY_PROMETHEUS`) |

Invitro also installs components for its own serverless experiments, which
ARIES does not use. `create-cluster.sh` removes them unless told to keep them;
see [Removing Invitro's unused components](#removing-invitros-unused-components).

## Quick start: build the cluster and apply ARIES with one command

`create-cluster.sh` builds the cluster and applies ARIES in one go. Steps 1 and
2 below do the same separately, for when you need to run them one at a time.

From your own machine, with passwordless SSH to every node:

```sh
k8s/create-cluster.sh --prometheus user@master user@runner user@worker1 user@worker2
```

As in Invitro, the first node is the control plane and the second is the loader
node; the rest are workers.

Or keep the nodes and settings in a file. Copy `cluster.json.example` to the
gitignored `cluster.json`, fill it in, and run the script with no arguments:

```sh
cp k8s/cluster.json.example k8s/cluster.json
k8s/create-cluster.sh
```

| Key | Meaning |
| --- | --- |
| `master`, `runner`, `workers` | SSH targets. `runner` is Invitro's loader node |
| `runner_dir` | Directory under the runner's home that receives ARIES; default `aries` |
| `ssh_key`, `ssh_options` | Identity file and extra `ssh` options, applied to every connection including Invitro's |
| `deploy_prometheus` | Same as `--prometheus` |
| `prune_invitro` | `false` is the same as `--keep-invitro`; removing is the default |
| `kubelet_housekeeping_interval` | Same as `--housekeeping`; `1s` gives one-second pod measurements |
| `node_setup` | Same as `--node-setup` |

`--config FILE` names another file, and command-line options win over the file.

The script:

1. checks out Invitro at the revision pinned at the top of the script into
   `.cache/invitro` and runs its `create_multinode_cluster` against the nodes;
2. copies the committed `k8s/` tree to the master and runs `apply-aries.sh`
   there;
3. builds `bin/aries`, `bin/aries-ssh-client` and `bin/aries-replay` for the loader node and stages
   them with `profiles/`, `configs/` and a namespaced kubeconfig under `~/aries`.

| Option | Effect |
| --- | --- |
| `--prometheus` | Sets Invitro's `DEPLOY_PROMETHEUS` and applies the ARIES monitoring overlay |
| `--node-setup FILE` | Uses your own Invitro `node_setup.json`; needed when the nodes' internal addresses are not Invitro's default `10.0.1.x` layout |
| `--keep-invitro` | Keeps Invitro's unused components, which are otherwise removed before ARIES is applied |
| `--housekeeping DUR` | Sets the kubelet's `--housekeeping-interval` on every node and restarts it. Invitro leaves the 10-second default, which caps how fine pod measurements can be |
| `--skip-cluster` | The cluster exists already; only apply ARIES and restage the runner |
| `--no-runner` | Do not build or stage the runner |

ARIES holds no copy of Invitro's scripts: only the pinned revision, which
`INVITRO_REVISION` overrides. The two steps below are what the script runs, for
doing them by hand.

## 1. Build the cluster

Follow Invitro's instructions for `create_multinode_cluster`. Set
`"DEPLOY_PROMETHEUS": true` in its `setup.json` if you want the dashboard.

## 2. Apply ARIES

On the master node, or anywhere `kubectl` and `helm` reach the cluster as an
admin, from an ARIES checkout:

```sh
k8s/apply-aries.sh --monitoring --runner-kubeconfig ~/aries-kubeconfig
```

| Option | Effect |
| --- | --- |
| *(none)* | Installs the chart into namespace `aries`: the runner's and the bridge's service accounts and Roles, and the dashboard ConfigMap |
| `--monitoring` | Upgrades the existing kube-prometheus-stack release in place with [`monitoring/values-aries.yaml`](monitoring/values-aries.yaml): cAdvisor scraped every second instead of Invitro's 15, and the Grafana sidecar reading dashboards from every namespace |
| `--prune-invitro` | First runs `prune-invitro.sh`, below |
| `--runner-kubeconfig PATH` | Writes a kubeconfig for the chart's `aries` ServiceAccount, so the runner holds the namespaced Role rather than cluster-admin |
| `--values FILE` | Extra chart values, such as a `secret.yaml` for a private registry |
| `--namespace NAME` | A namespace other than `aries`; set the same one in the profile |

The script ends by printing the cluster's Kubernetes version, its pod and
service CIDRs, and the Prometheus address, which the next two steps need. It is
safe to run again.

The chart's only workload is a cAdvisor DaemonSet, one pod per node, which
Prometheus scrapes for the pod measurements ARIES records (see
[the Kubernetes implementation](../docs/implementation/kubernetes.md#where-the-readings-come-from)).
Set `cadvisor.enabled: false` to leave it out; measurements then fail rather
than read as idle. ARIES creates the harness, sandbox and bridge pods for each
run and removes them afterwards.

## Removing Invitro's unused components

`prune-invitro.sh` frees the cluster of what Invitro installs for running
serverless functions. `create-cluster.sh` runs it by default, through
`apply-aries.sh --prune-invitro`; `apply-aries.sh` on its own leaves the
cluster's components alone unless given that option. To run it
on its own where `kubectl` reaches the cluster as an admin, use `make k8s-prune`;
`make k8s-prune K8S_PRUNE_FLAGS="--dry-run"` lists what it would remove without
changing anything.

| Removed | What Invitro uses it for |
| --- | --- |
| Knative Serving and Eventing (`knative-serving`, `knative-eventing`) | Running the loader's functions |
| Istio (`istio-system`) | Knative's ingress |
| MetalLB (`metallb-system`) | Load-balancer addresses for Istio's gateway |
| vHive's image registry (`registry`) and its volume | Caching function images |
| The `prometheus-pushgateway` release and the Knative dashboards | Loader metrics |

It removes their admission webhooks first, so pod creation never calls a
webhook that has gone, then the namespaces, then what they left elsewhere: the
root certificate Istio copies into every namespace, and their cluster-wide roles
and custom resource definitions. Those are found by each component's labels, as
many Knative Eventing roles do not carry its name, and by name for MetalLB,
which labels nothing. Calico, containerd, metrics-server and
kube-prometheus-stack stay. The registry's data stays on the node's disk.

Afterwards the cluster can no longer run Invitro's own experiments. It is safe
to run again.

## 3. Publish the images the cluster pulls

Docker runs build two images locally. A cluster cannot see them, so they are
published to the lab's GitHub Container Registry, which the checked-in
`-k8s-pods` profiles already map to. Log in with a classic personal access token
that holds `write:packages`, then publish:

```sh
echo "$GHCR_TOKEN" | docker login ghcr.io -u <github-user> --password-stdin
make k8s-images REGISTRY=ghcr.io/hyscale-lab
```

The script checks the login before building, since otherwise GHCR refuses the
push only once the build is done. GHCR creates a new package as private. Either
make `aries-bridge` and `aries-hermes` public in the organization's package
settings, or give the cluster a pull secret for `ghcr.io` with a token holding
`read:packages` (see [Credentials](#credentials)). To use another registry, pass
it as `REGISTRY`, for example `docker.io/<user>`, and change the profiles'
`images` entries to match.

`publish-images.sh` builds both images for `linux/amd64` with `docker buildx`
and pushes them under the tags ARIES requests:

- **`<registry>/aries-bridge`** is `aries-bridge` on
  `bridge.kubernetes_base_image` from `configs/versions.json`, an `alpine/k8s`
  image whose `kubectl` matches the Kubernetes version the pinned Invitro
  installs. The script needs no cluster access. For a cluster on another
  version, add `K8S_IMAGES_FLAGS="--kube-version <version>"` with the version
  `apply-aries.sh` printed.

  Its tag carries the commit, such as `aries-bridge:v1-0123456789ab`: the runner
  requests the bridge tagged with the commit `bin/aries` was built from
  (`make build` and `create-cluster.sh` stamp it in). A node keeps whatever
  image it already holds under a name, so with a fixed tag a cluster would go
  on running an old bridge after the code changed, and the bridge would reject
  or misread the harness's commands. Now a bridge not published for the
  runner's commit cannot be pulled, and the pod fails with `ErrImagePull`.
  Publish again after every commit you run on the cluster. The script refuses
  to publish while tracked files have uncommitted changes, because the image
  would not match its commit. A runner built from such a tree requests a
  `-dirty` tag that is never published, so it fails the same way; to try
  uncommitted bridge code, load the image onto the node yourself and map
  `aries-bridge` to it in the profile's `images`.
- **`<registry>/aries-hermes`** is the pinned upstream Hermes image plus
  `hermes-otel`, the same image `aries setup` builds for Docker.

It ends by printing the `images` entries for the profile. `--only bridge` or
`--only hermes` builds one image, `--platform linux/arm64` targets arm64 nodes,
and `--dry-run` prints the build commands. OpenClaw and the task images are
pulled from their public registries unchanged, so an OpenClaw run needs only
the bridge image.

## 4. Set the profile

The `-k8s-pods` profiles put the agent, the bridge and the sandbox in pods. All
three blocks must select the same backend and namespace:

```json
"harness": { "type": "hermes", "deployment": { "backend": "kubernetes", "kubernetes": {
  "namespace": "aries", "images": { "aries-local/hermes": "<registry>/aries-hermes" } } } },
"sandbox": { "deployment": { "backend": "kubernetes", "kubernetes": {
  "namespace": "aries", "pod_cidr": "<pod CIDR>", "service_cidr": "<service CIDR>" } } },
"bridge":  { "type": "hermes-ssh", "deployment": { "backend": "kubernetes", "kubernetes": {
  "namespace": "aries", "service_account": "aries-bridge", "images": { "aries-bridge": "<registry>/aries-bridge" } } } }
```

- **`pod_cidr`, `service_cidr`:** use the values the script printed. They keep a
  network-allowed sandbox off the cluster's own networks.
- **`prometheus_url`:** add the printed address under
  `sandbox.deployment.kubernetes` so the Recorder can read pod measurements.
  `create-cluster.sh` writes it into the profiles it stages on the runner. It is
  the Service's cluster address, so it only works from a cluster node, which
  Invitro's loader node is.
- **`images`:** map each locally built image to your registry. A key without a
  tag maps the repository and keeps the requested tag.
- **`node_selector`:** add `"node_selector": {"loader-nodetype": "worker"}` to
  each block to keep ARIES pods on Invitro's worker nodes.

See [deployment configuration](../docs/configuration.md#kubernetes-deployment)
for every field.

## 5. Run

The runner is a host process, not a pod. Build ARIES where `kubectl` reaches the
cluster, add the model key, and run a profile:

```sh
make build
export KUBECONFIG=~/aries-kubeconfig
./bin/aries profiles/kubernetes/hermes-tb2-fix-git-deepseek-k8s-pods.json
```

`execution.concurrency` bounds how many task occurrences run at once. Each one
gets its own harness pod and sandbox pod, and its own session on the run's
bridge pod. Size it against the workers' capacity: pods that do not fit sit
`Pending` on `Insufficient cpu`.

Run artifacts land in `runs/`, including each task's `bridge/tool-calls.jsonl`.
A run is tied to your SSH session; use `tmux` for long experiments.

While a run is in progress:

```sh
kubectl -n aries get pods -l app.kubernetes.io/managed-by=aries -o wide
```

## Credentials

- **Model key.** It is not in the chart or in any Kubernetes object. The runner
  reads `DEEPSEEK_API.key` beside its binaries and stages it into each task's
  harness pod itself.
- **Runner access.** `--runner-kubeconfig` uses the `aries` ServiceAccount's
  long-lived token (Secret `aries-token`). Delete that Secret to revoke it.
- **Private images.** While the GHCR packages are private, copy
  `aries/secret.yaml.example` (already set up for `ghcr.io`) to the gitignored
  `aries/secret.yaml`, fill it in, pass it with `--values`, and set
  `"image_pull_secret": "aries-registry"` in the profile.

## Status and caveats

- **Supported set.** Terminal-Bench 2, SWE-bench Pro and Deep Research Bench,
  with OpenClaw or Hermes in text mode. SWE-bench Pro needs no extra step: its
  upstream images already start as root and include `setpriv`, which ARIES uses
  to run the agent's commands as a non-root user (see
  [the Kubernetes implementation](../docs/implementation/kubernetes.md#execution-and-transfer)).
  That page also covers how a task service is exposed.
- **Task network isolation** depends on the CNI enforcing NetworkPolicy. Calico
  does; under flannel the policies are created and silently ignored.
- **Pod resource metrics** are read from Prometheus, so they need the
  monitoring stack and a `prometheus_url` in the profile; without one they are
  recorded as unsupported and the run continues. They come from the chart's
  cAdvisor, which samples every container every second. The Grafana dashboard
  still plots the kubelet's own cAdvisor series, whose resolution is set by
  `kubelet_housekeeping_interval`.
- **One bridge pod per run.** If it dies mid-run, the tasks in flight lose
  their tool access and their evidence, and evaluation is blocked for them.
- **An interrupted runner can leave pods behind.** `make k8s-clean` removes
  the pods, services and NetworkPolicies ARIES labels
  `app.kubernetes.io/managed-by=aries` in the `aries` namespace (`NAMESPACE=`
  picks another), and keeps the chart's own objects. Every run shares that
  label, so run it only when no runner is active in the namespace: it stops
  runs in progress too.
