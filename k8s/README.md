# ARIES on Kubernetes (Kustomize)

Kustomize package for running ARIES as a batch Job on Kubernetes.

```
k8s/
  base/                 # namespace, service account, RBAC, Job
  overlays/
    local/              # kind/minikube: secret from secret.env, image pull Never
```

## Layout

- **base/job.yaml** — ARIES runs a profile to completion, so it is a `Job`
  (`restartPolicy: Never`, `backoffLimit: 0`), not a Deployment.
- **base/rbac.yaml** — a namespace-scoped `Role` granting ARIES the permissions
  the upcoming Kubernetes *Tool Sandbox* needs: create/manage sandbox **pods**,
  `pods/exec` + `pods/attach`, `pods/log`, and configmaps/secrets for per-task
  access tokens. This is provisioned ahead of the sandbox implementation.
- **base/serviceaccount.yaml** — the `aries` service account the Job runs as and
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

# 4. Watch it:
kubectl -n aries logs -f job/aries
```

Tear down with `kubectl delete -k .`.

## Status / caveats

- A Kubernetes-native task **sandbox** now exists (`pkg/sandbox/kubernetes`,
  selected by `sandbox.type: "kubernetes"`). It drives the cluster via the
  `kubectl` binary (bundled in the image): one pod per task, `kubectl exec` for
  commands, `kubectl cp`/streamed exec for files. The RBAC in `base/rbac.yaml`
  grants exactly the pod/exec/log permissions it needs. Use
  `profiles/openclaw-tb2-fix-git-deepseek-k8s.json` to select it.
- **Not yet solved:** the OpenClaw E2B bridge authorizes requests by matching
  the sandbox's `NetworkGateway` against the request origin and uses it to build
  the bridge endpoint address. On Kubernetes the address a pod uses to reach the
  ARIES bridge is not the pod IP, so end-to-end bridge reachability/auth for the
  E2B pairing still needs a cluster-network-aware adaptation. The sandbox's own
  exec/filesystem surface is complete; this is the remaining integration gap.
- Pod resource metrics are a no-op placeholder (needs metrics-server/cAdvisor).
- Run artifacts use an `emptyDir` and are lost when the Job is deleted. Swap in
  a `PersistentVolumeClaim` to retain results.
- The default profile references the `openclaw-e2b` bridge + `docker` sandbox;
  point it at a Kubernetes-backed profile once that sandbox lands.
