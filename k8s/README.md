# ARIES on Kubernetes

`create-cluster.sh` builds a cluster with [vHive's setup tool](https://github.com/vhive-serverless/vHive/tree/main/scripts),
with Kubernetes and its pod network only, and applies ARIES to it. `apply-aries.sh`
applies ARIES to any other cluster. For how ARIES places its components on a
cluster, and what that provider does not support, see
[the Kubernetes implementation](../docs/implementation/kubernetes.md).

```
k8s/
  create-cluster.sh     # builds the cluster with vHive's setup tool, then runs apply-aries.sh
  apply-aries.sh        # applies everything below to an existing cluster
  publish-images.sh     # builds and pushes the bridge and Hermes images
  aries/                # the ARIES chart: runner and bridge RBAC, cAdvisor, dashboard
  monitoring/           # ARIES's values for kube-prometheus-stack
```

## What ARIES needs from the cluster

| Requirement | A `create-cluster.sh` cluster |
| --- | --- |
| A CNI that enforces NetworkPolicy | Calico |
| containerd | Yes, as a systemd service |
| `kubectl` and `helm` on the machine that applies ARIES | On the master node; `helm` at a pinned version |
| kube-prometheus-stack, for pod measurements and the dashboard | With `--prometheus`, on the runner |

## Quick start: build the cluster and apply ARIES with one command

From your own machine, with passwordless SSH and `sudo` on every node (Ubuntu,
as vHive's setup tool requires):

```sh
k8s/create-cluster.sh --prometheus --testbed user@master user@runner user@worker1 user@worker2
```

The first node is the control plane. The second, the runner, joins the cluster,
holds the monitoring stack and receives the ARIES runner; the rest are workers,
where ARIES puts harness, sandbox and bridge pods.

Or keep the nodes and settings in a file. Copy `cluster.json.example` to the
gitignored `cluster.json`, fill it in, and run the script with no arguments:

```sh
cp k8s/cluster.json.example k8s/cluster.json
k8s/create-cluster.sh
```

| Key | Meaning |
| --- | --- |
| `master`, `runner`, `workers` | SSH targets |
| `runner_dir` | Directory under the runner's home that receives ARIES; default `aries` |
| `ssh_key`, `ssh_options` | Identity file and extra `ssh` options, applied to every connection |
| `deploy_prometheus` | Same as `--prometheus` |
| `kubelet_housekeeping_interval` | Same as `--housekeeping`; `1s` gives one-second pod measurements |
| `testbed` | Same as `--testbed` |
| `vhive_dir` | Same as `--vhive-dir` |

`--config FILE` names another file, and command-line options win over the file.

The script:

1. copies `scripts/testbed/` to every node and installs vHive at the commit
   pinned at the top of the script on every node, with
   `setup_tool --k8s-only --containerd-service setup_node stock-only`: containerd,
   runc, the CNI plugins and the Kubernetes packages, but no Knative CLI. It
   then quiets each node for timing with `scripts/testbed/quiet-node.sh` (SMT,
   transparent huge pages, NUMA balancing and swap off) and turns turbo off;
2. runs `kubeadm init` on the master through
   `setup_tool --k8s-only --init-only create_multinode_cluster stock-only`, and
   joins the runner and workers with a fresh token;
3. installs Calico with `setup_tool --k8s-only setup_master_node stock-only` and
   waits for every node; labels the runner `aries/role=monitoring` and the
   workers `aries/role=worker`;
4. sets the kubelet housekeeping interval, if asked;
5. installs Helm at a pinned version, checked against its published checksum,
   copies the committed `k8s/` tree to the master and runs `apply-aries.sh`
   there;
6. with `--testbed`, pins the CPU clock on the runner and workers and installs
   Docker on the workers (`scripts/testbed/`), for Docker runs on the same
   nodes;
7. builds `bin/aries`, `bin/aries-ssh-client` and `bin/aries-replay` for the runner and
   stages them with `profiles/`, `configs/` and a namespaced kubeconfig under `~/aries`.

Every step skips what is already done, so a run that failed part-way can be
run again.

| Option | Effect |
| --- | --- |
| `--prometheus` | Installs kube-prometheus-stack with ARIES's values, on the runner |
| `--housekeeping DUR` | Sets the kubelet's `--housekeeping-interval` on every node and restarts it. The kubelet's 10-second default caps how fine pod measurements can be |
| `--testbed` | Pins the CPU clock on the runner and workers and installs Docker on the workers |
| `--vhive-dir DIR` | Installs vHive from this local checkout's `HEAD`, copied to the nodes, instead of fetching the pinned commit |
| `--skip-cluster` | The cluster exists already; only apply ARIES and restage the runner |
| `--no-runner` | Do not build or stage the runner |

`VHIVE_REPO` and `VHIVE_REVISION` override the vHive the nodes fetch, and
`HELM_VERSION` the Helm. vHive's setup tool keeps its logs on each node in
`~/aries-setup-node.log` and, on the master, `~/aries-create-cluster.log` and
`~/aries-master-node.log`.

## Apply ARIES to an existing cluster

On the master node, or anywhere `kubectl` and `helm` reach the cluster as an
admin, from an ARIES checkout:

```sh
k8s/apply-aries.sh --monitoring --runner-kubeconfig ~/aries-kubeconfig
```

| Option | Effect |
| --- | --- |
| *(none)* | Installs the chart into namespace `aries`: the runner's and the bridge's service accounts and Roles, the cAdvisor DaemonSet and the dashboard ConfigMap |
| `--monitoring` | Installs or upgrades kube-prometheus-stack (chart 72.6.2, `MONITORING_CHART_VERSION` overrides it) with [`monitoring/values-aries.yaml`](monitoring/values-aries.yaml): on the node labelled `aries/role=monitoring`, cAdvisor scraped every second, monitors selected from every release, and the Grafana sidecar reading dashboards from every namespace |
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
  image whose `kubectl` matches the Kubernetes version the vHive pinned in
  `create-cluster.sh` installs (1.35.3). The script needs no cluster access. For a cluster on another
  version, add `K8S_IMAGES_FLAGS="--kube-version <version>"` with the version
  `apply-aries.sh` printed.

  Its tag carries the last commit that changed the bridge, such as
  `aries-bridge:v1-0123456789ab`, as `scripts/bridge-revision.sh` prints it:
  the newest commit touching the Go packages `aries-bridge` compiles (tests
  aside), `go.mod`, `go.sum`, `Dockerfile.bridge` or `configs/versions.json`.
  `make build` and `create-cluster.sh` stamp it into `bin/aries`, which
  requests that tag. A node keeps whatever image it already holds under a
  name, so with a fixed tag a cluster would go on running an old bridge after
  the code changed, and the bridge would reject or misread the harness's
  commands. Now a bridge not published for the runner's code cannot be pulled,
  and the pod fails with `ErrImagePull` naming the image. Publish again after
  a commit that changes the bridge; other commits keep the tag. The script
  refuses to publish while the bridge's files have uncommitted changes,
  because the image would not match its commit. A runner built from such a
  tree requests a `-dirty` tag that is never published, so it fails the same
  way; to try uncommitted bridge code, load the image onto the node yourself
  and map `aries-bridge` to it in the profile's `images`.
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
  the runner is.
- **`images`:** map each locally built image to your registry. A key without a
  tag maps the repository and keeps the requested tag.
- **`node_selector`:** the profiles set `"node_selector": {"aries/role": "worker"}`
  in each block, which keeps ARIES pods on the workers `create-cluster.sh`
  labels; on another cluster, label its workers so or change the selector.

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
