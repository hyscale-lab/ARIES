# Kubernetes backend

ARIES can run its task sandbox and OpenClaw agent harness on Kubernetes instead
of Docker, driven by the `kubectl` binary, with ARIES itself running either
inside the cluster or out-of-cluster. This document describes the design, the
configuration surface, the deployment models, and the per-task lifecycle.

The Kubernetes path is selected entirely through profile configuration; no code
paths are hard-wired. The Docker backends are untouched and remain the default.

## Motivation

The Docker backends assume a local Docker Engine: the sandbox creates
containers through the Moby SDK, and the bridge/harness reach them over a local
Docker network. To study agent serving on cluster infrastructure — and to run
where no Docker socket is available — ARIES needs to place the task environment
and the agent in Kubernetes pods while preserving the four-role lifecycle and
its isolation gates.

## Architecture

The Kubernetes backend keeps the Runner's four roles and their ordering. Only
the concrete substrate changes: the task environment and the agent become pods,
and access between them is arranged over the cluster network.

```
                 ARIES (in-cluster Job, or out-of-cluster process)
                 ├── ToolSandbox (kubernetes) ── kubectl ──► task pod
                 ├── ToolBridge  (openclaw-ssh) ── SSH server on ARIES
                 └── AgentHarness (kubernetes)  ── kubectl ──► agent pod + Service
                                                              port-forward ──► gateway

   agent pod ──SSH──► ARIES SSH bridge ──kubectl exec──► task pod
```

- **Sandbox** (`pkg/sandbox/kubernetes`): one pod per task. Commands run via
  `kubectl exec`; files move via `kubectl exec`/`cp`. Satisfies the same Go
  interfaces the bridge type-asserts, so it is a drop-in for the Docker sandbox.
- **Bridge** (`pkg/bridge/openclawssh`): the existing SSH bridge, extended with
  an *advertise host* so its endpoint and `known_hosts` reference a
  cluster-reachable address rather than a Docker network gateway.
- **Harness** (`pkg/harness/openclaw`, `KubeManager`): one agent pod + Service
  per task. ARIES stages the private runtime into the pod and reaches the agent
  gateway through a `kubectl port-forward`.

The agent's reverse hop — agent pod → SSH bridge — is the crux. It only works
when the bridge's advertised address is reachable from the agent pod, which is
what the deployment models below arrange.

## Decoupling the bridge from Docker types

The OpenClaw bridges type-assert the live sandbox to method sets that returned
concrete Docker types (`FileInfo`, and a `ProcessRef` with unexported fields).
A non-Docker sandbox could not construct those. The neutral package
`pkg/sandbox` now holds:

- `sandbox.FileInfo` — the filesystem metadata surface.
- `sandbox.ProcessRef{PID int; Handle any}` — an attached-process identity whose
  `Handle` carries backend-specific bookkeeping opaque to the bridge; each
  backend type-asserts it back to its own concrete handle.

Both the Docker and Kubernetes sandboxes return these neutral types, and the
bridge depends only on `pkg/sandbox`. This is what makes the sandbox genuinely
substitutable.

## The Kubernetes sandbox

`pkg/sandbox/kubernetes` implements `runner.ToolSandbox`/`runner.Sandbox` plus
the filesystem and attached-process surface the OpenClaw bridges require. It
drives the cluster with the `kubectl` binary rather than a client-go dependency.

- **Lifecycle** — `Start` renders a Pod manifest (image, workdir, resource
  limits from the environment, ownership labels; long run/task IDs are stored as
  **annotations** to respect the 63-byte label limit) and `kubectl apply`s it,
  then waits for `condition=Ready`. `Stop` deletes the pod and positively
  confirms its absence.
- **Processes** — `Exec`/`ExecStream`/`ExecProcessStream` run over `kubectl
  exec`. The attached form uses an early-PID handshake: a wrapper writes the
  child PID to a file and then `exec`s the real command, so stdout/stderr stay
  clean and `onStart` fires before completion. `SendProcessSignal`/
  `TerminateProcess` deliver signals with in-pod `kill`.
- **Filesystem** — `ReadFile`/`WriteFile`/`StatPath`/`ListDir`/`MakeDir`/
  `RemovePath`/`MovePath`/`Upload`/`Download` via `kubectl exec` (and `kubectl
  cp` for uploads/downloads), with the same absolute/normalized path validation
  the bridge expects.
- **Compile-time guards** (`assertions.go`) mirror the bridges' unexported
  interfaces so any signature drift in a sandbox method is a build failure
  rather than a runtime "sandbox does not support …".

Pod resource metrics are a no-op placeholder (`NewResourceSource`); per-pod
CPU/memory telemetry would require the metrics API and is deferred.

## The SSH bridge advertise host

`openclawssh.Options.AdvertiseHost` switches the bridge to *advertised* mode:

- **Unset (Docker, default):** the SSH server binds the sandbox's Docker network
  gateway and advertises that gateway — unchanged behavior.
- **Set (Kubernetes):** the server binds `0.0.0.0` (reachable from pods) and the
  returned endpoint address and `known_hosts` line use the configured host.

The value supports environment expansion in wiring, so a profile can advertise
ARIES's own pod IP with `"$POD_IP"` (see in-cluster deployment).

## The Kubernetes harness

`KubeManager` (`pkg/harness/openclaw/kube.go`) is a second `AgentHarness`
backend in the OpenClaw package, reusing that package's config rendering,
private runtime archive, gateway WebSocket client, and artifact helpers
(`runtimeArchive` and `writeAgentResult` were extracted to shared functions).
Only the container-runtime operations differ. Agent (text) mode only; realtime
voice is rejected for this backend.

Per-task `Start` sequence:

1. Render the OpenClaw config and build the private runtime archive (plugin
   material, gateway launcher, keys, and the SSH client/identity/known_hosts
   the bridge produced).
2. `kubectl apply` a **Service** and a **Pod**. The pod boots into a wait state:
   it idles until a sentinel file appears, preserving the Docker "inject files,
   then start the gateway" ordering.
3. Wait for the pod to be `Running`, then stage the archive with `kubectl exec …
   tar -x -C /`, and release the gateway by creating the sentinel.
4. Wait for gateway readiness with the in-pod `/readyz` probe (over `kubectl
   exec`).
5. Open `kubectl port-forward service/… :18789` and point the gateway client at
   `ws://127.0.0.1:<local-port>`.

`Run` drives the agent exactly as the Docker backend does (connect → `Agent` →
redact → write result → collect logs). `Stop` kills the port-forward, deletes
the pod and Service, and confirms removal.

### Non-root staging

The agent container runs as uid 1000 (readiness requires it), so it cannot
write under root-owned `/run` or `/opt`. The pod therefore mounts writable
`emptyDir` volumes at `/run/aries` and `/opt/aries`, and a **root
initContainer** `chown`s them to 1000 so the uid-1000 `tar` extraction can
create and chmod the runtime files. This is the Kubernetes equivalent of
Docker's root-privileged `CopyToContainer`.

## Configuration

| Field | Values | Meaning |
| --- | --- | --- |
| `sandbox.type` | `docker` \| `kubernetes` | Task environment backend. |
| `sandbox.namespace` | string (default `aries`) | Namespace for task pods. |
| `harness.deployment` | `docker` (default) \| `kubernetes` | Where the OpenClaw agent runs. |
| `harness.namespace` | string (default `aries`) | Namespace for agent pods. |
| `bridge.advertise_host` | string | Host agents use to reach the bridge; env refs are expanded (e.g. `"$POD_IP"`). Empty keeps Docker gateway binding. |

Constraints: `harness.deployment: kubernetes` requires `harness.type: openclaw`
and rejects realtime mode.

Bundled profiles:

- `profiles/openclaw-tb2-fix-git-deepseek-k8s.json` — k8s sandbox with the E2B
  HTTP bridge.
- `profiles/openclaw-tb2-fix-git-deepseek-k8s-ssh.json` — k8s sandbox + k8s
  harness + SSH bridge, `advertise_host: host.docker.internal` (local clusters).
- `profiles/openclaw-tb2-fix-git-deepseek-k8s-incluster.json` — the same with
  `advertise_host: "$POD_IP"` for ARIES running inside the cluster.

## Deployment

ARIES is packaged as a container image that bundles `kubectl`. The image is
cross-compiled so a `linux/amd64` image builds quickly on an arm64 host
(`FROM --platform=$BUILDPLATFORM` + `GOOS/GOARCH`), avoiding an emulated Go
compile.

Kustomize manifests live under `k8s/`:

```
k8s/
  base/            namespace, service account, RBAC, ARIES Job
  base/openclaw/   reference Deployment + Service for the agent gateway
  overlays/local/  kind/minikube/Docker Desktop: secret + imagePullPolicy: Never
  overlays/incluster/  ARIES in-cluster: image ref, $POD_IP, imagePullPolicy: Always
```

The `aries-sandbox` Role grants the permissions ARIES needs as an in-cluster
workload owner: pods, `pods/exec`/`attach`/`portforward`, `pods/log`, services,
and configmaps/secrets — namespace-scoped so ARIES cannot touch other
workloads.

### Model A — ARIES in-cluster (recommended for remote clusters)

ARIES, the bridge, and the agent/sandbox pods share the cluster network, so the
agent's reverse SSH hop is pod-to-pod. `bridge.advertise_host: "$POD_IP"` is
resolved from the downward-API `POD_IP` env, so the bridge advertises ARIES's
own pod IP.

```sh
# 1. Push the ARIES image to a registry the cluster can pull.
docker buildx build --platform linux/amd64 -t <registry>/aries:latest --push .
#    Set images.newName in k8s/overlays/incluster/kustomization.yaml.

# 2. Provide the model key (gitignored).
cd k8s/overlays/incluster && cp secret.env.example secret.env   # edit DEEPSEEK_API_KEY

# 3. (private image) create the pull secret in the namespace.
kubectl create namespace aries
kubectl -n aries create secret docker-registry aries-registry \
  --docker-server=https://index.docker.io/v1/ \
  --docker-username=<user> --docker-password=<access-token>

# 4. Deploy. Jobs are immutable, so delete before re-applying.
kubectl -n aries delete job aries --ignore-not-found
kubectl apply -k k8s/overlays/incluster
kubectl -n aries logs -f job/aries
```

### Model B — ARIES on an in-VPC host

Run the ARIES container on a host with a cluster-routable IP (bastion/VM),
mount its kubeconfig, set `advertise_host` to that host's IP, and run the
profile. No manifests are needed; ARIES drives the cluster via `kubectl`.

### Local clusters

For kind/minikube/Docker Desktop, use `overlays/local`: load the image into the
node (`kind load docker-image` / `minikube image load`), set `advertise_host` to
`host.docker.internal` (Docker Desktop) or the host-reachable address, and
`kubectl apply -k k8s/overlays/local`.

## Operational notes

- **Jobs are immutable.** Changing image/args/env requires `kubectl delete job`
  before re-apply. The in-cluster overlay sets `imagePullPolicy: Always` so a
  re-pushed `:latest` is picked up.
- **Architecture must match the nodes.** Build `linux/amd64` for x86 nodes.
- **Image pulls.** The kubelet pulls the agent (`ghcr.io/openclaw/openclaw`) and
  task (Terminal-Bench) images per pod; ARIES skips the Docker pre-pull when the
  sandbox is Kubernetes. Nodes need egress to those registries, or the images
  mirrored into a reachable one.
- **Run artifacts** use an `emptyDir` on the ARIES pod and are lost when the Job
  is deleted; swap in a PersistentVolumeClaim to retain results.

## Status

Validated end-to-end on a kubeadm cluster (Model A): all three pods run, the
agent executes tools in the sandbox pod through the SSH bridge, and independent
evaluation scores the task. The E2B HTTP bridge has a Kubernetes-capable
sandbox but its endpoint/auth still assume a Docker network gateway; the SSH
bridge is the validated Kubernetes path. Realtime harness mode is Docker-only.
