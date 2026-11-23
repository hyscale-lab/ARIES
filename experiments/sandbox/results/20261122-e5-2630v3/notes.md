### About this campaign

This campaign replays the [coldwarm 20261115-e5-2630v3](../../../coldwarm/results/20261115-e5-2630v3/report.md)
recordings on the same workers and hardware, and changes only how the tool
sandbox pod is made. Every request was answered from the recording, so model
time is identical across setups. The plain pod (ARIES K8s) is the baseline;
native and Docker are not run here, and the coldwarm campaign compares them
with ARIES K8s.

All 70 measured runs succeeded, 350 of 350 task runs solved. Left out are the
planned warm-up runs, one per harness and setup, and the warm-pool runs of the
first pass (below).

Each run executes the five tasks in order, `mb-noop` first.

### The warm-pool setups were run again

In the first pass (2026-11-22), each warm-pool run created its pool on its
first task's claim. A claim on a pool with no ready pod does not wait for it:
agent-sandbox makes the claim a pod of its own about 2 s later, while the
pool's pod keeps booting. `mb-noop` therefore started its sandbox in 2.8 s
under runc and gVisor and 8.7 s under Kata, slower than without a pool.

ARIES now readies a run's pools before its first task (commit `9f9deac`). The
six warm-pool setups were run again on 2026-11-23, after the other setups had
finished, with their own warm-ups. The first pass's 30 runs are kept and
marked superseded. The bridge's code is unchanged, but the commit gave it a
new revision, so these runs used the bridge image
`aries-bridge:v1-9f9deaca3645`. Readying the pool before the first task takes
1.9 s under runc, 2.3 s under gVisor and 5.3–6.3 s under Kata. This happens
once per run and is not part of any task's time.

The rerun was not interleaved with the other setups, so differences between
the warm-pool setups and the rest also include anything that changed on the
workers in between.

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

With the pool readied first, every task claims a ready pod, `mb-noop`
included: 1.8–1.9 s on every runtime. A pool therefore saves 0.9 s (runc) to
4.7 s (Kata) of sandbox start. With these harnesses none of it reaches the
user. It would matter only for a harness that starts faster than its sandbox.

OpenClaw's cold latency on the warm-pool setups is 1.1–1.2 s above the plain
pod's, on every runtime. That comes from a longer harness start (+1.0–1.5 s),
not from the sandbox. Hermes shows nothing similar. This campaign can't tell
whether the pool causes it or the reruns ran under different conditions:
- One possible cause is the pool starting a replacement pod beside the
  harness, which is busy starting on the same worker.
- The other is the day between the two passes. In the first pass the effect
  was smaller and uneven, from 0 to +2 s depending on the task.

### Where gVisor and Kata cost time: tool calls

Under gVisor and Kata each tool call is slower, by about the same amount for
both runtimes, whether the pod is plain, from agent-sandbox or from a pool.

The cost per call in warm `mb-ten-calls`, as the harness timed it:

| Harness | runc | gVisor | Kata |
|---|---|---|---|
| OpenClaw | 608–614 ms | 667–674 ms (+60) | 680–682 ms (+70) |
| Hermes | 432–436 ms | 585–593 ms (+155) | 574–575 ms (+140) |

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
  agent-sandbox and 4.5–4.6 s for the warm pool. The same ordering appeared in
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
