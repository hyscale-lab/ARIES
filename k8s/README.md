# ARIES on Kubernetes (Kustomize)

Kustomize package for running ARIES as a long-lived pod on Kubernetes. For what
the Kubernetes port implements across ARIES, the harness, and the bridge — and
what is still missing — see [KUBERNETES.md](KUBERNETES.md).

```
k8s/
  install/              # kubeadm bootstrap scripts for master and worker nodes
  base/                 # namespace, service account, RBAC, Deployment
  overlays/
    local/              # kind/minikube: secret from secret.env, image pull Never
    incluster/          # remote cluster: image pulled from a registry, POD_IP bridge
```

If you do not have a cluster yet, [`install/`](install/README.md) bootstraps one
on bare Linux hosts with kubeadm. Either describe the nodes in a config file and
run `install-cluster.sh` once from your own machine, or run `install-master.sh`
and `install-worker.sh` on each host yourself. The kube packages, the containerd
runtime and the CNI are downloaded from upstream at run time.

## Layout

- **install/** — `install-cluster.sh` (SSH orchestrator driven by a topology
  file), plus the per-node `common.sh`, `install-master.sh`,
  `install-worker.sh` and `reset-node.sh`. See its
  [README](install/README.md).
- **base/deployment.yaml** — a `Deployment` whose container idles on
  `sleep infinity`. The ARIES binary is a batch runner, not a server, so runs are
  triggered on demand with `kubectl exec` rather than by the entrypoint; see
  [Triggering a run](#triggering-a-run). It references an `aries-registry` pull
  secret, which the in-cluster overlay generates from a gitignored
  `registry.json`; see [Image registry secret](#image-registry-secret).
- **base/rbac.yaml** — a namespace-scoped `Role` granting ARIES the permissions
  the upcoming Kubernetes _Tool Sandbox_ needs: create/manage sandbox **pods**,
  `pods/exec` + `pods/attach`, `pods/log`, and configmaps/secrets for per-task
  access tokens. This is provisioned ahead of the sandbox implementation.
- **base/serviceaccount.yaml** — the `aries` service account the Deployment runs as and
  the RBAC binds to.

## OpenClaw harness (Deployment + Service)

`base/openclaw/` deploys the OpenClaw agent gateway as a Deployment fronted by a
ClusterIP Service. ARIES runs **outside** the cluster and reaches the gateway
through the Service via a port-forward:

```sh
kubectl apply -k k8s/base/openclaw
kubectl -n aries port-forward svc/aries-openclaw 18789:18789
# ARIES then targets ws://127.0.0.1:18789
```

The pod boots into a wait state and idles until ARIES stages the per-task
runtime (plugin + gateway launcher + rendered config) and creates the
`/run/aries/ready` sentinel — preserving the Docker "inject files, then start the
gateway" ordering on Kubernetes.

**Integration status:** the ARIES-side Kubernetes harness backend is now wired
(`pkg/harness/openclaw`, `NewKube`), selected by `harness.deployment:
"kubernetes"`. It creates a per-task agent pod + Service, stages the private
runtime via `kubectl exec tar -x`, releases the gateway, and reaches it with an
automatic `kubectl port-forward` (agent/text mode only; realtime unsupported).
The static manifests in this directory are a reference/template for that flow.

Pair it with a Kubernetes-capable bridge: `bridge.type: "openclaw-ssh"` with
`bridge.advertise_host` set (see `profiles/openclaw-tb2-fix-git-deepseek-k8s-ssh.json`).

## Usage (local cluster)

```sh
# 1. Build and load the image into your local cluster:
docker build -t aries:latest .
kind load docker-image aries:latest         # or: minikube image load aries:latest

# 2. Provide the model key (gitignored):
cd k8s/overlays/local
cp secret.env.example secret.env            # then edit secret.env

# 3. Render / apply:
kubectl kustomize .                         # preview
kubectl apply -k .                          # apply

# 4. Trigger a run (the pod idles until you do):
kubectl -n aries exec deploy/aries -- sh -c './bin/aries "$ARIES_PROFILE"'
```

Tear down with `kubectl delete -k .`.

The local overlay needs no registry secret: it sets `imagePullPolicy: Never` and
uses the image already loaded into the node. The `aries-registry` pull secret
inherited from `base/deployment.yaml` is simply absent, which the kubelet ignores
because it never pulls.

## Usage (in-cluster overlay)

`overlays/incluster` runs ARIES **inside** a remote cluster, so the node has to
pull the ARIES image from a registry rather than have it loaded locally. Point
`images:` in `overlays/incluster/kustomization.yaml` at your own repository
first — it ships with `jingxiang212/aries:latest`.

### Kubeconfig

Use `ssh username@master_node 'sudo cat /etc/kubernetes/admin.conf' > ~/.kube/config` to get the kubeconfig file.

### Image registry secret

`base/deployment.yaml` declares `imagePullSecrets: [aries-registry]`, and the overlay's
`secretGenerator` builds that Secret from `registry.json` — a Docker config
document that is gitignored, exactly like `secret.env`:

```yaml
secretGenerator:
  - name: aries-registry
    type: kubernetes.io/dockerconfigjson
    files:
      - .dockerconfigjson=registry.json
```

`disableNameSuffixHash: true` keeps the generated name exactly `aries-registry`,
which is what `imagePullSecrets` references. Rename it in both places if you
change it.

Write `registry.json` with your own credentials. The least error-prone way is to
let `kubectl` build the document and never apply it — use an access token, not
an account password:

```sh
cd k8s/overlays/incluster

# Docker Hub (personal access token with read scope):
kubectl create secret docker-registry aries-registry \
  --docker-server=https://index.docker.io/v1/ \
  --docker-username=<docker-hub-user> \
  --docker-password=<access-token> \
  --dry-run=client -o jsonpath='{.data.\.dockerconfigjson}' \
  | base64 -d > registry.json

# GHCR (classic PAT with read:packages) — same command with:
#   --docker-server=ghcr.io --docker-username=<github-user> --docker-password=<pat>
```

`registry.json.example` shows the resulting shape if you prefer to write it by
hand. An existing `docker login` can also be reused with
`cp ~/.docker/config.json registry.json`, but only when that file holds a real
credential: Docker Desktop and `credsStore` setups keep the token in an external
keychain and leave an empty `auths` entry behind, which yields a Secret that
authenticates as anonymous. Verify what the overlay will actually ship:

```sh
kubectl kustomize . | grep -A4 'name: aries-registry'
```

If your image is public, delete the `aries-registry` generator and the
`imagePullSecrets` block instead of supplying dummy credentials — a missing pull
secret is only a kubelet warning for a public image, but `ImagePullBackOff` for
a private one.

### Deploy

```sh
# 1. Build and push the image to the registry the cluster pulls from:
docker build -t <registry>/aries:latest .
docker push <registry>/aries:latest

# 2. Provide the model key and the registry credentials (both gitignored):
cd k8s/overlays/incluster
cp secret.env.example secret.env            # then edit secret.env
#   ...and create registry.json as shown above

# 3. Render / apply:
kubectl kustomize .                         # preview
kubectl apply -k .                          # apply

# 4. Trigger a run (the pod idles until you do):
kubectl -n aries exec deploy/aries -- sh -c './bin/aries "$ARIES_PROFILE"'
```

Both files are required: Kustomize fails with `evalsymlink failure on
.../registry.json` if the credentials are missing, before anything reaches the
cluster. `kubectl delete -k .` now removes the pull secret along with the
Deployment and the `aries-model` Secret, so a redeploy recreates it from
`registry.json`.

## Triggering a run

The ARIES binary is a batch runner: `aries PROFILE.json` runs a profile to
completion and exits. It has no server mode and no task-intake API. A Deployment
restarts its container whenever the process exits, so running a profile as the
entrypoint would re-run the whole experiment in a loop.

`base/deployment.yaml` therefore overrides the image entrypoint with
`sleep infinity`. The pod comes up idle and stays up; runs are started on demand:

```sh
# Run the profile the overlay selected:
kubectl -n aries exec deploy/aries -- sh -c './bin/aries "$ARIES_PROFILE"'

# ...or name one explicitly:
kubectl -n aries exec deploy/aries -- ./bin/aries profiles/openclaw-tb2-five-deepseek.json

# Follow along, or attach a shell:
kubectl -n aries logs -f deploy/aries
kubectl -n aries exec -it deploy/aries -- bash
```

`ARIES_PROFILE` is an env var on the container, set per overlay, so the exec line
never has to repeat the path. `sh -c` is what expands it — it is evaluated
inside the pod, not by your local shell.

Keeping the pod up means the image, the model key, and the run-artifact volume
stay mounted between runs, so iterating is far quicker than recreating a Job each
time.

On the in-cluster overlay `runs/` is backed by a PersistentVolume
(`storage.yaml`), so artifacts outlive pod restarts:

```sh
kubectl -n aries exec deploy/aries -- ls runs/          # every run so far
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

Two consequences worth knowing. Nothing runs automatically on `kubectl apply` —
the deploy step only makes the pod ready. And a run is tied to your `exec`
session, so a dropped connection kills it; use `kubectl exec ... -- sh -c
'nohup ./bin/aries "$ARIES_PROFILE" > runs/last.log 2>&1 &'` for long
experiments, or keep the Job pattern for unattended batches.

## Status / caveats

- A Kubernetes-native task **sandbox** now exists (`pkg/sandbox/kubernetes`,
  selected by `sandbox.type: "kubernetes"`). It drives the cluster via the
  `kubectl` binary (bundled in the image): one pod per task, `kubectl exec` for
  commands, `kubectl cp`/streamed exec for files. The RBAC in `base/rbac.yaml`
  grants exactly the pod/exec/log permissions it needs. Use
  `profiles/openclaw-tb2-fix-git-deepseek-k8s.json` to select it.
- **Task network isolation** gives every task pod its own `NetworkPolicy`.
  Ingress is always denied, which is what keeps concurrent tasks from reaching
  each other; `allow_internet` decides only whether egress is denied outright or
  limited to non-cluster destinations. Set `sandbox.pod_cidr` and
  `sandbox.service_cidr` in the profile so the second case can exclude the
  cluster's own networks — read them with
  `kubectl cluster-info dump | grep -m2 -E 'cluster-cidr|service-cluster-ip-range'`.
  This requires a CNI that implements NetworkPolicy: `install/` defaults to
  **Calico**, and under flannel the policies are created and silently ignored.
- **Pod resource metrics** are read from the kubelet Summary API — no
  metrics-server needed. This needs the `aries-node-metrics` ClusterRole in
  `base/rbac.yaml` (`get` on `nodes/proxy`), the only permission ARIES holds
  outside its namespace. Remove it to disable telemetry.
- **Not yet solved:** the OpenClaw E2B bridge authorizes requests by matching
  the sandbox's `NetworkGateway` against the request origin and uses it to build
  the bridge endpoint address. On Kubernetes the address a pod uses to reach the
  ARIES bridge is not the pod IP, so end-to-end bridge reachability/auth for the
  E2B pairing still needs a cluster-network-aware adaptation. The sandbox's own
  exec/filesystem surface is complete; this is the remaining integration gap.
- Run artifacts use an `emptyDir` in `base`; the in-cluster overlay replaces it
  with a node-local PersistentVolume so results survive pod restarts. Swap in
  a `PersistentVolumeClaim` to retain results.
- The default profile references the `openclaw-e2b` bridge + `docker` sandbox;
  point it at a Kubernetes-backed profile once that sandbox lands.
