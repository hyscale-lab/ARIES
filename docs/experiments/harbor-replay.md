# Harbor fix-git replay

`cmd/aries-harbor` accepts the original ATIF-v1.7 trajectory and native Codex rollout for a reviewed occurrence on [the Terminal-Bench 2.1 GPT-6 Astra Max row](https://hub.harborframework.com/datasets/terminal-bench/terminal-bench-2-1/6/leaderboards/main/rows/f8d623c8-5f1b-4f9c-8deb-d110fa91e1d2). The five artifact pairs in [reviewed.json](../../pkg/replay/harbor/reviewed.json) came from [Harbor job 17d1a7f6](https://hub.harborframework.com/jobs/17d1a7f6-3339-4670-8b70-3b145979f57f). The source pins bind trial IDs even when the two public artifact filenames repeat across occurrences.

Build and inspect an occurrence locally. Standard output contains the source timeline, including its native line references and modeled waits.

```sh
go build -o bin/aries-harbor ./cmd/aries-harbor
bin/aries-harbor -trajectory trajectory.json -rollout rollout.jsonl
```

On the prepared vHive worker, supply the prepared benchmark image and a new private JSONL path to run. The session uses one VM for all steps. The image must already be available in the selected containerd namespace. A new allocation, common ready point, snapshot, and fresh restore precede each replay. The VM stays running across calls and waits. The optional observer handles tracing freezes. Run with the worker's socket and shim configuration.

```sh
bin/aries-harbor -trajectory trajectory.json -rollout rollout.jsonl \
  -image docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74 \
  -namespace aries -socket "$CONTAINERD_SOCKET" -io-dir "$IO_DIR" \
  -shim-dir "$SHIM_DIR" -checkpoint-dir "$CHECKPOINT_DIR" \
  -ripgrep "$CODEX_RG" -observer "$OBSERVER" -sampler scripts/zb-trace/sample_vmm.py \
  -samples samples.jsonl -log replay.jsonl
```

The benchmark image lacks the ripgrep executable provided by the recorded Codex harness. Extract `package/vendor/x86_64-unknown-linux-musl/codex-path/rg` from the [official 0.151.0 Linux x64 release](https://registry.npmjs.org/@openai/codex/-/codex-0.151.0-linux-x64.tgz) and pass it as `-ripgrep`. ARIES verifies SHA-256 `e62198eb19b136b88c330af83647b5a962cb99b6b1f066758568f12de1974849` on the host and inside the guest before snapshot. The private containerd-v2 guest uses the task v3 service. Its restored shim publishes an adopted task endpoint and manages framed stdio. The default `-guest-task-v3` uses that endpoint. The optimized variant additionally takes `-native-checkpoint`.

For a separate telemetry run, `-guest-probe PATH` stages the [guest memory probe](../../scripts/zb-guest/README.md) before the common snapshot. It runs after initial and post-tool host captures while vCPUs execute. The following wait interval includes the probe's own guest accesses. Each receipt brackets it with host monotonic times and retains its guest clock and coverage. Keep probe-enabled runs identified when comparing access windows.

The adapter uses the ARIES [replay plan and call structures](../../pkg/replay/plan.go) from the [replayer implementation at a26c195](https://github.com/hyscale-lab/ARIES/tree/a26c195/pkg/replay). Its scheduler uses the timestamped native tool request and result, and waits for the live guest reply before advancing. Original user, harness, reasoning and assistant payloads remain in the source files. Timeline receipts retain their rollout line numbers. Harness-only plan updates preserve their source interval and have no guest command. Gaps between remaining events are inferred harness/model time, including before the first tool and after the last. The Codex exports do not distinguish model service time from transport and harness activity during those gaps. The source-defined foreground calls all returned before their requested yield. Additional tool types, detached sessions or concurrent requests require appropriate execution semantics before enabling another source.

The host invokes an observer executable synchronously with `PHASE RUNTIME_ID STEP SOCKET_PATH`. Session start and end use step 0. A pre-tool phase can partition the preceding wait, while post-tool captures the tool interval. Each hook must resume the VM before returning. It owns pause and resume timestamps and initial arming. The separate host sampler starts before session origin and supplies one verified PSS sample before tool admission. A replay without an observer produces tool timing and execution evidence only. Native events and tool receipts use host CLOCK_MONOTONIC nanoseconds. Replay status compares expected and live exit codes, with recorded nonzero outcomes accepted. Private receipts retain live stdout and stderr separately. Source truncation and unavailable mixed-stream order limit exact output comparisons. The comparison ignores trailing newlines. Inspect dynamic Git commit IDs and timestamps when interpreting remaining differences.

Run the deterministic scheduling checks and vHive provider tests with `go test ./pkg/replay/... ./cmd/aries-harbor ./pkg/deployment/vhive`. The private fixture exercises real shell state in a temporary directory using authored commands. Native measurements require the worker and a compatible snapshot restore.
