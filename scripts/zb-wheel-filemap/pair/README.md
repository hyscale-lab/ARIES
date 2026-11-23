# Matched wheel advice and sham sessions

The four serial periods run the original PyTorch trial `4b588047` on the same stock v3 worker, with reporting enabled throughout. The order is treated, sham, sham, treated. Every restored workload starts independently from the same image and diagnostic root. At the end of source step 12 both arms audit the same ten wheel inodes, check their mappings and clean cache state, and capture the guest filemap deletion trace. The treated arm issues real file advice. The sham skips the advice syscall. Both arms capture the full one-second VMM PSS series, source-boundary guest-RAM PSS, VMM placement, KVM slots, stock reporting callbacks and a packed present-bit map for each sampled boundary. [Sealed endpoints and pairing](protocol.json)

Build the same guest twice with the same tool versions to check its digest if needed. Use the parent fixture's kernel build, source assets, corrected adapter and clean ext4. [Prerequisites and kernel build](../README.md)

```bash
scripts/zb-wheel-filemap/build-probe.sh "$study/paired-guest" pair
scripts/zb-wheel-filemap/build-fixture.sh "$CLEAN_EXT4" "$study/paired-guest" "$study/pair-fixture"
read -r root_digest _ < "$study/pair-fixture/fixture.sha256"
python3 -B scripts/zb-wheel-filemap/pair/prepare-stage.py "$study/pair-stage" \
    --kernel-dir "$study/kernel" --root-gz "$study/pair-fixture/diagnostic.ext4.gz" \
    --root-sha "$root_digest" --adapter "$CLOCK_ADAPTER"
```

On the prepared amd201 study worker, transfer the sealed stage and run the following command from a service with enough time to complete all periods. It verifies the source and deployment digests, provisions one private runtime, launches one fresh VM at a time and records the actual source and observer statuses.

```bash
bash "$STAGE/run-series.sh" "$STAGE"
```

The optional second argument `reuse` requires a new stage with a `runtime` symlink to this same private service's already verified runtime. The preflight checked the numeric kernel start ticks supplied by the immutable trace helper. The four measured periods use that correction from their first observation. Result files are in `periods/{1-treated,2-sham,3-sham,4-treated}/`. Each `boundaries.jsonl` entry supplies the read brackets, slot layout and SHA-256 for the corresponding files under `presence/`. Bits use least-significant-bit-first order within each byte. The host opens only `/proc/PID/pagemap` for the slot entries, without touching guest memory. Each slot read has a separate host monotonic bracket. Whole-RAM scans and live VMA PSS observations take finite time while the VM runs.

The publication uses Zepbound's wheel-advice-sham reducer and compares the observed 180-second interval and its 30-second pre-advice baseline. [Offline analysis](https://github.com/hyscale-lab/Zepbound/blob/main/proj/residency/wheel-pfn/reduce-pair.py)
