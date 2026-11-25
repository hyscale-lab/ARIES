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

The published fix-git image generated its detached Git commit during an April build. The source session was built in August and its commands use the August commit hash. Before taking a snapshot, the replay reads the image's dangling commit and substitutes the two source-recorded Git timestamps. It computes the object ID in memory and proceeds only if Git returns the exact recorded full hash. The ready-point receipt confirms the source parent and tree, clean master, reachable recovered commit and byte-for-byte matching formatted reflog. The superseded April object can remain unreachable in the object database. Preparation and its timing are recorded outside replay. [Ready-point reconstruction](../../cmd/aries-harbor/fix_git_ready.py)

For a separate telemetry run, `-guest-probe PATH` stages the [guest memory probe](../../scripts/zb-guest/README.md) before the common snapshot. It runs after initial and post-tool host captures while vCPUs execute. The following wait interval includes the probe's own guest accesses. Each receipt brackets it with host monotonic times and retains its guest clock and coverage. Keep probe-enabled runs identified when comparing access windows.

The adapter uses the ARIES [replay plan and call structures](../../pkg/replay/plan.go) from the [replayer implementation at a26c195](https://github.com/hyscale-lab/ARIES/tree/a26c195/pkg/replay). Its scheduler uses the timestamped native tool request and result, and waits for the live guest reply before advancing. Original user, harness, reasoning and assistant payloads remain in the source files. Timeline receipts retain their rollout line numbers. Harness-only plan updates preserve their source interval and have no guest command. Gaps between remaining events are inferred harness/model time, including before the first tool and after the last. The Codex exports do not distinguish model service time from transport and harness activity during those gaps. The source-defined foreground calls all returned before their requested yield. Additional tool types, detached sessions or concurrent requests require appropriate execution semantics before enabling another source.

The host invokes an observer executable synchronously with `PHASE RUNTIME_ID STEP SOCKET_PATH`. Session start and end use step 0. A pre-tool phase can partition the preceding wait, while post-tool captures the tool interval. Each hook must resume the VM before returning. It owns pause and resume timestamps and initial arming. The separate host sampler starts before session origin and supplies one verified PSS sample before tool admission. A replay without an observer produces tool timing and execution evidence only. Native events and tool receipts use host CLOCK_MONOTONIC nanoseconds. Replay status compares expected and live exit codes, with recorded nonzero outcomes accepted. Private receipts retain live stdout and stderr separately. Source truncation and unavailable mixed-stream order limit exact output comparisons. The comparison ignores trailing newlines. Inspect dynamic Git commit IDs and timestamps when interpreting remaining differences.

Run the deterministic scheduling checks and vHive provider tests with `go test ./pkg/replay/... ./cmd/aries-harbor ./pkg/deployment/vhive`. The private fixture exercises real shell state in a temporary directory using authored commands. Native measurements require the worker and a compatible snapshot restore.

For a measured run, [run-traced.sh](../../scripts/harbor-replay/run-traced.sh) starts the KVM slot observer, waits for readiness, invokes the replay with the idle hook and sampler, and collects the named run files under a new directory. Set `ZB_VMM_CENSUS=1` to include the slot-attributed VMA census. For legacy file-backed snapshots, set `ZB_CHECKPOINT_NAMESPACE_DIR` to the operator's checkpoint directory followed by the namespace. Pass the same ordinary replay arguments after the first three wrapper arguments.

```sh
scripts/harbor-replay/run-traced.sh ./bin/aries-harbor scripts/zb-trace /path/to/new-run \
    -trajectory trajectory.json -rollout rollout.jsonl -ripgrep ./rg \
    -image "$BENCHMARK_IMAGE" -socket "$CONTAINERD_SOCKET" \
    -checkpoint-dir "$CHECKPOINT_DIR" -shim-dir "$SHIM_DIR" -io-dir "$IO_DIR"
```

## Terminal-Bench 4.1 replay pilot

The 4.1 cohort pilot uses the [original `roy-polymorph-cn` trial](https://hub.harborframework.com/jobs/a538f69d-7756-444a-9dfe-43a0ab10b3a8/trials/d16a13fe-8381-4594-b6f6-60bb6f3aa660) from the Terminal-Bench 4.0 job that Harbor regraded. ARIES pins the original artifact pair, source job and row, and the OCI manifest rebuilt from the [original task Dockerfile and data](https://github.com/harbor-framework/terminal-bench/tree/452bf305c6daa62fc59061d22133a7cbc7c1572e/tasks/roy-polymorph-cn). Import the digest-qualified image into the selected containerd namespace before starting the pilot. Guest readiness verifies the task inputs, virtual environment, first-run output paths and the absence of the command that produced the recorded exit 2. The five guest replies should be `0, 0, 0, 2, 0` in source order. One recorded web action and the plan updates remain host-side timeline events.

```sh
bin/aries-harbor -trajectory trajectory.json -rollout rollout.jsonl > source-timeline.json
bin/aries-harbor -trajectory trajectory.json -rollout rollout.jsonl \
  -image docker.io/library/zb-tb41-roy@sha256:3ed43bff698735f15fa9db6e020df8e44e1d3ec37e8828ebc53eba054e509a77 \
  -namespace aries -socket "$CONTAINERD_SOCKET" -io-dir "$IO_DIR" \
  -shim-dir "$SHIM_DIR" -checkpoint-dir "$CHECKPOINT_DIR" \
  -native-checkpoint -log replay.jsonl
```

Use `call_id` from the source timeline and `source_call_id` from the run receipts to compare recorded and live dispatch and first-reply times. The original pilot has no process polls. The separate `TestReplaySeparatesLateTerminalPoll` verifies a live retained handle and a first `running` response when the recording expected a terminal poll. The next observation uses the same handle and has separate dispatch and result receipts. For this source family it waits at most five seconds.

Four more original ROY recordings share the same task digest, resource configuration, input files, and guest readiness profile. Use the same image and replay command with their own source pairs. One recording ends with a live process handle. The final replay capture occurs while that handle is retained, and a post-session receipt records its observed state before stream cancellation and sandbox removal. [Approved artifact hashes](../../pkg/replay/harbor/reviewed.json)
