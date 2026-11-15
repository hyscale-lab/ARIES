### About this campaign

This is the cold and warm experiment on a new cluster built with vHive's setup
tool. The workers are dual-socket Xeon E5-2630 v3 nodes with 16 cores and
62 GiB, SMT and turbo off, and the clock pinned at 2.4 GHz. Under load the
cores run at 2.4 GHz. The cluster runs Kubernetes 1.35.3, Docker 29.8.2 and
kernel 6.8.

Each harness's model replies were recorded afresh, with the cold and the warm
run recorded separately. Every replayed request was answered from its
recording. Model time therefore matches across setups within this campaign,
but not with the earlier campaign's recordings.

Three runs are kept but left out of the analysis:
- **OpenClaw's first recording** failed because the harness did not report
  ready within ARIES's 45 s start deadline. The deadline was raised to 120 s
  and OpenClaw recorded again on a rebuilt pc822 (see `meta.json`).
- **The first Kubernetes repetition of each harness** included pulling the
  harness image into the node's containerd: 47 s for OpenClaw and 101 s for
  Hermes, on `mb-noop`'s pod. Each was rerun with the image cached. Later
  campaigns avoid this with a warm-up run per setup.

### What the results show

- **Cold latency is mostly the harness starting itself.** OpenClaw takes
  51–56 s to report ready, and Hermes 11–12 s. This dwarfs everything ARIES
  adds.
- **ARIES + Docker against native, cold:**
  - **Hermes:** level with native; slightly faster on four of the five tasks.
  - **OpenClaw:** 2.8–5.5 s slower, almost all of it a longer harness start
    (56.2 against 51.3 s). Reserving the sandbox and bridge and creating the
    container cost 0.1 s.
- **Kubernetes adds start-up of its own.** Creating the harness pod takes 2.9 s
  (OpenClaw) and 1.9 s (Hermes). The sandbox pod's start and bridge binding
  run alongside the harness start, partly hidden. Against native, cold latency
  rises by 5.2 s for OpenClaw and 3.4 s for Hermes.
- **Warm, the setups differ only in what a tool call costs.**
  - **No tools:** `mb-noop` is the same everywhere within 0.1 s for Hermes.
    OpenClaw's native harness logic is 0.7 s slower than through ARIES.
  - **Ten calls:** `mb-ten-calls` shows the gap. Per warm call:

    | Harness | Native | ARIES Docker | ARIES Kubernetes |
    |---|---|---|---|
    | OpenClaw | 215 ms | 390 ms | 610 ms |
    | Hermes | 174 ms | 314 ms | 433 ms |
- **Where a call's time goes through ARIES.** The bridge runs several commands
  per agent call: 4.1 for OpenClaw, whose workspace sync runs before each
  command, and 5 for Hermes. Per OpenClaw call, the bridge spends 209 ms on
  Docker and 434 ms on Kubernetes running them. About 180 ms is transport
  between harness, bridge and sandbox, which is about what a whole native call
  costs. Kubernetes's extra cost is in running the commands (exec into a pod),
  not in the transport.
- **Hermes's first request is slow in every setup:** about 10 s of harness
  logic cold, against 0.5 s warm. This is Hermes's first-use set-up (tool
  checks, terminal session), not ARIES; native shows the same.
- **After the answer:**
  - **Docker:** ARIES takes 2 s (OpenClaw) to 4 s (Hermes).
  - **Kubernetes:** 8.5–10 s. Cleaning up the sandbox pod takes 3.4 s, and
    evaluating, which runs in the pod, 1.7 s.
  - **Hermes's artifact export** takes 2.7–3.1 s in both ARIES setups.

  None of this adds to the user's wait, but it limits how quickly a worker
  takes the next task.

### Compared with the E5-2640 v4 campaign

The same setups and tasks ran on Broadwell nodes under Kubernetes 1.34
(`../20261102-e5-2640v4`). On these nodes almost everything is slower:

| Measure | OpenClaw, before | OpenClaw, now | Hermes, before | Hermes, now |
|---|---|---|---|---|
| Native harness start (s) | 27.2 | 51.3 | 8.1 | 10.9 |
| Warm tool call, native / Docker / K8s (ms) | 106 / 183 / 314 | 215 / 390 / 610 | 111 / 140 / 255 | 174 / 314 / 433 |
| `mb-cpu` warm tool execution, native (s) | 3.35 | 3.98 | 3.38 | 3.98 |

The CPU-bound task is 19% slower, but start-up and tool calls take up to about
twice as long. Short, bursty work suffers most. Likely contributors are:
- the older CPU generation, with two sockets;
- deep idle states (C6 is enabled, so idle cores wake slowly);
- the newer Docker, Kubernetes and kernel.

This campaign does not tell them apart. Compare setups within a campaign: the
ranking of setups and the shape of each breakdown hold on both clusters, but
absolute times do not carry over.

### Limits

- Five repetitions per setup. Standard deviations are in the per-task reports
  and are mostly small, but a single slow harness start moves a mean.
- One worker per harness, so a harness's setups share hardware, but the two
  harnesses ran on different nodes of the same type.
- Model replies are replayed with their recorded timing, so model time is
  constant by construction and says nothing about a live model.
- Native baselines record no stop step, and their clean-up does not confirm
  that the harness stopped. Their after-answer times are not strictly
  comparable.
