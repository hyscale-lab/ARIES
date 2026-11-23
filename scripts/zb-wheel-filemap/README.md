# Guest wheel-cache deletion tracing

This fixture replays the reviewed PyTorch source `4b588047-22af-4dc2-a705-6b8d24ff88fe` on stock Firecracker v3. After source step 12 it measures which clean, unmapped wheel-cache pages the guest discards. A private tracefs instance records the real filemap deletion event, filtered to the device and ten verified inodes. Each event supplies the file offset, guest PFN and folio order. In parallel, the stock reporting observer records successful mapping replacements and the KVM slot registrations needed to translate them to guest-physical addresses.

The collector checks the original file descriptors, file identity, process mappings, per-file mincore offsets, and zero dirty/writeback page counts before issuing the single controlled advice operation. The source files remain untouched by the PFN observer. The diagnostic kernel uses the original vendor revision with event tracing and `cachestat` enabled. The ordinary ARIES guest is built independently from `scripts/zb-guest`. [Original release helper](../zb-guest/torch_wheel_release.go) · [Reporting observer](../zb-demand/reported-replacements.bt)

## Build and stage

Use Linux x86-64, Go 1.26.8, `patch`, `debugfs`, `e2fsck`, and the toolchain for the study guest kernel. All output paths below must be new. Run from the root of this repository, using absolute paths for the kernel source, configuration, clean ext4, workspace and corrected adapter.

```bash
export study=/path/to/build-directory
mkdir -p "$study"
scripts/zb-wheel-filemap/build-probe.sh "$study/zb-guest"
scripts/zb-wheel-filemap/build-kernel.sh "$KERNEL_SOURCE" "$study/kernel" "$STOCK_CONFIG"
scripts/zb-wheel-filemap/build-fixture.sh "$CLEAN_EXT4" "$study/zb-guest" "$study/fixture"
read -r root_digest _ < "$study/fixture/fixture.sha256"
python3 -B scripts/zb-wheel-filemap/prepare-stage.py "$study/stage" \
  --kernel-dir "$study/kernel" --root-gz "$study/fixture/diagnostic.ext4.gz" \
  --root-sha "$root_digest" --adapter "$CLOCK_ADAPTER"
```

The vendor source revision is `7483450dec64b4f923037de31d8c28298a7c3359`. The stock config digest is `99d0950a178b92a1db3d5ee233dc6a320c8892334b99ba6fea11bd7be3745067`. The clean ext4 digest is `292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f`. The experiment uses the corrected ARIES adapter from `c0a6633`, SHA-256 `557158ab56d79ee3f6fca0300f6b0dc423724d4b795adecb5d738a6cb5155541`. Use the archived diagnostic kernel and ext4 inputs when verifying the historical stage digest. Filesystem metadata and kernel build metadata can change the bytes of newly created images.

## Worker entry point

Copy the complete stage to the reserved amd201 worker. Its stock v3 donor runtime is `/local/zb-torch-deep-v3/runtime`, and its sealed workload, tracer and scoped CNI are in `/local/zb-corpus-torch-cohort-4b58-v3`. The pinned source has one vCPU, 8 GiB RAM and 10 GiB writable storage. The wrapper checks hardware placement, provisions an isolated v3 unit, validates every staged input, captures both guest and host evidence, executes the normal source verifier, and checks the final worker state.

```bash
bash "$STAGE/run-worker.sh" "$STAGE"
```

The raw stage includes `events.jsonl`, `guest-cache.jsonl`, `handler/events`, `slots.log`, `samples.jsonl`, the verification/status files and the worker placement records. The compressed guest trace is in the post-tool-12 response under `wheel_release.guest.filemap_deletions.trace_gzip`. Guest and host timestamps use separate clock domains. The physical-address reduction belongs to Zepbound. [Published result and per-page evidence](https://github.com/hyscale-lab/Zepbound/tree/main/plan/result/1_opportunity/guest-memory-deep/wheel-filemap-pfn)

## Executed probe and stage

| Recorded artifact | SHA-256 |
|---|---|
| Guest binary | `56f7c4f3b211a8f96058ccba1c73cd810c52a35d598860fac6e184baa7a295fa` |
| Trace-enabled guest kernel | `6b0bf116f7baad32304742d9dc43e97f6541635feee6f16377cab817e0e563a1` |
| Diagnostic ext4 | `00e12514b78c566591f95aec8896bb4fbc6684ac2bd94bdb98c707af9bfb69f3` |
| Complete stage SHA256SUMS file | `ad78d51eb575f9c4b428fb3dd7ef73517d2f27d56db5e5af9b2ed2f87cd061a8` |
