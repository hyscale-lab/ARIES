# Kubernetes run flow

This document traces one complete run on the Kubernetes backend
(`harness.deployment: kubernetes`, `sandbox.type: kubernetes`,
`bridge.type: openclaw-ssh`) from `kubectl apply` through task execution to
teardown. It is the chronological companion to the structural
[Kubernetes backend](kubernetes.md) design.

For the in-cluster deployment model, ARIES runs as a Kubernetes Job and drives
the cluster with the `kubectl` binary using its in-cluster ServiceAccount. The
sandbox and agent each become their own pod, and the SSH bridge runs inside the
ARIES pod.

## Actors

- **`aries` Job/Pod** — the orchestrator: Runner lifecycle, the SSH bridge
  listener, and all `kubectl` calls.
- **`aries-task-<id>` Pod** — the task sandbox (Terminal-Bench image).
- **`aries-openclaw-<id>` Pod + Service** — the OpenClaw agent gateway.
- **DeepSeek** — the external model endpoint the agent calls.

## Sequence

```mermaid
sequenceDiagram
    participant U as kubectl apply
    participant J as aries Job/Pod
    participant K as kube-apiserver
    participant S as aries-task Pod
    participant B as SSH bridge (in aries Pod)
    participant H as aries-openclaw Pod+Svc
    participant M as DeepSeek

    U->>K: apply -k overlays/incluster
    K->>J: schedule aries Pod (SA aries)
    J->>J: preparation (benchmark checkout; skip Docker pre-pull)
    J->>J: run started; model runtime healthy
    Note over J: per task fix-git-001
    J->>K: create aries-task Pod
    K->>S: start; wait Ready
    J->>S: benchmark sanitizes sandbox (kubectl exec)
    J->>B: start SSH bridge, bind 0.0.0.0, advertise $POD_IP
    J->>K: create aries-openclaw Service + Pod
    K->>H: initContainer chown; container idles on sentinel
    J->>H: kubectl exec tar -x (stage runtime)
    J->>H: touch sentinel -> gateway launches
    J->>H: /readyz probe until ready
    J->>H: kubectl port-forward svc :18789
    J->>H: gateway.Connect + Agent(instruction)
    loop agent tool loop
        H->>M: model call
        M-->>H: tool call / response
        H->>B: SSH exec (tool) to $POD_IP
        B->>S: kubectl exec in task pod
        S-->>B: stdout/stderr/exit
        B-->>H: SSH result
    end
    H-->>J: agent final response
    J->>H: Stop: kill port-forward, delete Pod+Svc
    J->>B: revoke bridge (positive)
    J->>S: benchmark evaluates live sandbox (kubectl exec)
    J->>K: delete aries-task Pod
    J-->>U: run-result.json (score)
```

## Phase 0 — Deploy

`kubectl apply -k k8s/overlays/incluster` creates the namespace, the `aries`
ServiceAccount, the `aries-sandbox` Role/RoleBinding, the `aries-model` Secret,
and the `aries` Job. The Job template carries the image, the profile argument,
`DEEPSEEK_API_KEY` from the Secret, `POD_IP` from the downward API, and the
`aries-registry` image pull secret.

Because a Job's pod template is immutable, redeploying requires
`kubectl delete job aries` first. The overlay sets `imagePullPolicy: Always` so
a re-pushed `:latest` image is picked up.

The kubelet pulls `<registry>/aries:latest` and starts the pod as
ServiceAccount `aries`. `kubectl` inside the pod uses the auto-mounted SA token;
the RBAC grants it pods, `pods/exec`/`attach`/`portforward`, `pods/log`,
services, and configmaps/secrets — namespace-scoped.

## Phase 1 — Preparation

The ARIES process starts and runs profile preparation:

1. `SetupBenchmark` verifies the pinned Terminal-Bench checkout.
2. `LoadPreparationTasks` loads the task list.
3. **Image pre-pull is skipped** for `sandbox.type: kubernetes` — there is no
   local Docker engine; the kubelet pulls the agent and task images per pod.

Log markers: `preparation_state: preparing` → `prepared`, then
`experiment run started` and `model runtime lifecycle … healthy`.

## Phase 2 — Per-task lifecycle

The Runner drives each task through the ordered, fail-closed sequence. For
`fix-git-001`:

### 2a. Start the sandbox pod

The Kubernetes sandbox `kubectl apply`s `aries-task-<id>` (Terminal-Bench image,
`sleep infinity`, workdir, resource limits; long run/task IDs are annotations,
not labels) and waits for `condition=Ready`.

### 2b. Sanitize

The benchmark inspects the live sandbox over `kubectl exec` and confirms
verifier paths are absent before any bridge or agent exists.

### 2c. Start the bridge

The OpenClaw SSH bridge starts inside the ARIES pod. In advertised mode it binds
`0.0.0.0:<port>` and mints session host/client keys; the endpoint address and
`known_hosts` use the advertised host — ARIES's own pod IP via `$POD_IP`. It
returns a `ToolEndpoint` describing the SSH connection plus the client, identity,
and known-hosts source files.

Log marker: `OpenClaw SSH bridge started` with `address: <podIP>:<port>`.

### 2d. Start the harness (agent pod)

`KubeManager.Start`:

1. Renders the OpenClaw config and builds the private runtime archive, folding in
   the bridge's SSH client/identity/known-hosts.
2. `kubectl apply`s the **Service** and the **Pod**. The pod's `fix-perms`
   initContainer (root) chowns the `/run/aries` and `/opt/aries` emptyDir mounts
   to uid 1000; the main container (uid 1000) then idles waiting for the
   `/run/aries/ready` sentinel.
3. Waits for the pod `Running`, then stages the archive:
   `kubectl exec -i … tar -xmf - -C / --no-same-owner --no-overwrite-dir`.
4. Releases the gateway by creating the sentinel; the bootstrap `exec`s the
   gateway launcher.
5. Polls the in-pod `/readyz` (uid 1000, ready true) over `kubectl exec` until
   ready.
6. Opens `kubectl port-forward service/… :18789` and parses the local port,
   setting the gateway URL to `ws://127.0.0.1:<local-port>`.

Log marker: `OpenClaw Kubernetes harness started` with the agent pod name.

### 2e. Run the agent

`Run` connects the gateway WebSocket client (through the port-forward), checks
the `operator.write` scope, and issues one `Agent` request with the task
instruction. The agent then loops:

- calls DeepSeek (`model-fetch … deepseek-v4-flash status=200`);
- for each tool call, SSHes to the advertised bridge address; the bridge
  authenticates the session and translates the SSH exec into a `kubectl exec`
  inside the task pod, streaming stdout/stderr/exit back;
- incorporates the result and continues until it finishes or the agent timeout.

The agent's reverse hop works because ARIES advertises its own pod IP and the
agent pod reaches it pod-to-pod on the cluster network.

### 2f. Stop the harness

`Stop` kills the port-forward, deletes the agent Pod and Service, and confirms
their removal. The agent result and gateway logs are written as artifacts.

### 2g. Revoke the bridge

The bridge positively revokes the SSH grant and confirms access is gone. This
gate — together with harness stop — must succeed before evaluation.

### 2h. Evaluate

Only now, with the agent stopped and access revoked, does the benchmark upload
its private verifier material into the still-live task pod (over `kubectl exec`)
and score the final state, independent of the harness outcome.

### 2i. Destroy the sandbox

The Kubernetes sandbox deletes `aries-task-<id>` and confirms its absence.

## Phase 3 — Finish

The Runner writes `run-result.json` and the ARIES pod exits. On success the Job
reports `Completed`; the pods remain as `Completed`/`Succeeded` until deleted.
A validated run shows `harness_status: succeeded`, `evaluation_status:
succeeded`, and `score: 1`.

## Where each failure surfaced (reference)

The order in which this backend was brought up maps to the phases above; each
was a real failure resolved during validation:

| Symptom | Phase | Cause / fix |
| --- | --- | --- |
| `no match for platform in manifest` | 0 | arm64 image on amd64 nodes; cross-compile `linux/amd64`. |
| `ImagePullBackOff` on a private repo | 0 | create the `aries-registry` docker-registry secret. |
| `dial unix /var/run/docker.sock` | 1 | Docker pre-pull; skipped for the Kubernetes sandbox. |
| `label … must be no more than 63 bytes` | 2a | run/task IDs moved from labels to annotations. |
| `tar: run/aries: Cannot mkdir: Permission denied` | 2d | uid-1000 write under root `/`; mount emptyDir volumes. |
| `tar: … Cannot change mode … Operation not permitted` | 2d | root-owned mountpoint; root initContainer chowns to 1000. |
| immutable Job on re-apply | 0 | `kubectl delete job` before `apply`. |

## Cleanup

```sh
kubectl -n aries delete job aries          # removes the ARIES pod
# task/agent pods are deleted by the Runner during teardown; orphans (from a
# hard failure) can be removed by label:
kubectl -n aries delete pod -l app.kubernetes.io/managed-by=aries
kubectl -n aries delete pod -l app.kubernetes.io/name=aries-openclaw
```
