### About this campaign

This campaign replays the [coldwarm 20261115-e5-2630v3](../../../coldwarm/results/20261115-e5-2630v3/report.md)
recordings on the same workers and hardware, and changes only how the tool
sandbox pod is made. Every request was answered from the recording, so model
time is identical across setups. The plain pod (ARIES K8s) is the baseline;
native and Docker are not run here, and the coldwarm campaign compares them
with ARIES K8s.

All 70 measured runs succeeded, 350 of 350 task runs solved. The 14 left out
are the planned warm-up runs, one per harness and setup.

Each run executes the five tasks in order, `mb-noop` first. A warm-pool run
creates its own pool of one when the run starts, so `mb-noop` claims a pod
that is still starting, and only the later four tasks claim a ready one.

### The sandbox is not on the critical path

The sandbox starts alongside the harness, and the harness start dominates
(about 12 s for Hermes and 56 s for OpenClaw). Every sandbox start, Kata's
6.5 s included, finishes well inside it, so cold latency differs from the
plain pod by at most about 1 s in any setup. Most of those differences are
within one standard deviation.

| Sandbox start (s) | Plain pod | agent-sandbox | Warm pool |
|---|---|---|---|
| runc | 2.7 | 2.3–2.6 | 1.8 |
| gVisor | | 2.6 | 1.8 |
| Kata | | 6.5 | 1.8 |

The warm pool's 1.8 s is for the four tasks that claim a ready pod. When the
pod is still starting (`mb-noop`), the claim takes 2.8 s under runc and gVisor
and 8.7 s under Kata, longer than creating the pod directly.

A pool therefore saves at most 0.9 s (runc) to 4.7 s (Kata) of sandbox start,
and with these harnesses none of it reaches the user. It would matter only for
a harness that starts faster than its sandbox.

The largest cold difference, +1.1 s for OpenClaw on the Kata warm pool, comes
from a longer harness start, not from the sandbox. The likely cause is the
pool booting a replacement Kata VM, which uses CPU on the same worker while the
harness starts. `mb-noop`'s harness start is 2.0 s longer there, and in that
task the claimed VM is also booting.

### Where gVisor and Kata cost time: tool calls

Under gVisor and Kata each tool call is slower, by about the same amount for
both runtimes, whether the pod is plain, from agent-sandbox or from a pool.

The cost per call in warm `mb-ten-calls`, as the harness timed it:

| Harness | runc | gVisor | Kata |
|---|---|---|---|
| OpenClaw | 608–614 ms | 668–674 ms (+60) | 680–681 ms (+70) |
| Hermes | 432–436 ms | 585–588 ms (+150) | 575 ms (+140) |

The per-task reports' bridge rows split this cost:
- **OpenClaw:** each call is about 4 sandbox commands. Under gVisor and Kata
  the bridge's share grows by about 65 ms per call, and the transport doesn't
  change.
- **Hermes:** each call is 5 sandbox commands. The bridge's share grows by
  80–95 ms, and the transport between harness and sandbox grows by about
  60 ms more.

Warm `mb-ten-calls` is 1.5 s slower for Hermes and 0.5 s slower for OpenClaw.
Single-call tasks change by up to 0.5 s.

### agent-sandbox under runc matches the plain pod

Under runc, agent-sandbox matches the plain pod within noise:
- its sandbox starts 0.1–0.4 s faster;
- its cold and warm latency are within ±0.4 s;
- its per-call time is within 6 ms.

The controller's extra step, from the Sandbox to the pod, costs nothing
measurable.

### After the answer

- **Evaluation:** about 0.3 s slower under gVisor and Kata. The verifier runs
  in the sandbox, so it pays the per-command cost.
- **Clean-up:** about 1.2 s faster under gVisor and Kata (2.0–2.3 s) than
  under runc. Under runc it takes 3.3 s for the plain pod, 3.6–4.0 s for
  agent-sandbox and 4.6 s for the warm pool. The same ordering appeared in
  the Phase 1 replays. All three delete in the foreground, so each waits until
  the containers have left the node. Under runc the sandbox's `sleep`
  ignores the stop signal and waits out the grace period, and a claim's
  delete also removes its claim and Sandbox.

### Resources

The sandbox's peak memory, from its pod cgroup:
- **runc:** about 1 MiB;
- **gVisor:** 17–18 MiB, gVisor's Sentry;
- **Kata:** 317–320 MiB, mostly the guest VM.

Sandbox CPU peaks rise with the runtime, but stay well under the 2-CPU limit.
The harness's CPU and memory don't change with the sandbox setup. Its peak CPU
varies by up to ±100% of a core between repetitions, so differences in that
row are noise.
