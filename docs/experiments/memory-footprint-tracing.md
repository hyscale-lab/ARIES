# Memory-footprint tracer source and setup

This repository preserves the latest recovered ARIES tracer integration and the
source changes needed to rebuild its native dependencies. It does not contain
experiment outputs, model credentials, guest kernels, VM snapshots, or compiled
tracer binaries. Rebuilding the code does not require the deleted collection-node
run directories.

The current backend is **VMM-owned tracing**: Firecracker supplies guest RAM
regions, access/unknown masks, KVM and conservative device dirty masks, residency,
and bounded immutable metadata. The external Python companion interprets Linux
metadata. The harness plugin defines complete LLM tool-response batches and
pauses the guest during LLM intervals. This pause policy changes asynchronous
guest progress and is an explicit experimental intervention.

## Preserved source

| Source | Purpose |
| --- | --- |
| [`scripts/tsb-firecracker/`](../../scripts/tsb-firecracker) | Exact Firecracker source delta and dependency manifest |
| [`scripts/tsb-companion/`](../../scripts/tsb-companion) | VMM API client, decoder, capture/replay, tests and pinned kernel layouts |
| [`scripts/tsb-companion/fixtures/`](../../scripts/tsb-companion/fixtures) | Current known-object guest fixture and native companion validation driver |
| [`scripts/tsb-pages/`](../../scripts/tsb-pages) | Observer server, VMM coordinator, suite preparation, boundary audits and comparisons |
| [`scripts/tsb-access-dirty-plugin/`](../../scripts/tsb-access-dirty-plugin) | Native batch, model-occurrence and terminal lifecycle instrumentation |
| [`scripts/tsb-steps/`](../../scripts/tsb-steps) | Exact OpenClaw source patch, dependency manifest and native build instructions |
| [`pkg/harness/openclaw/step_observation.go`](../../pkg/harness/openclaw/step_observation.go) | Plugin staging and host-owned post-stop observer-release barrier |
| [`pkg/deployment/vhive/memory.go`](../../pkg/deployment/vhive/memory.go) | Separate process-mapping observations for native VMs |
| [`profiles/experiments/tsb-tool-pages/fix-git.json`](../../profiles/experiments/tsb-tool-pages/fix-git.json) | Current profile template with native CPU/RAM; adapt its worker paths, image and observer address |

The maintained collection path is the VMM-owned backend. Earlier standalone
host-memory, idle-page and restore-fault collectors and their study guides were
removed; their sources remain in Git history at `b0c358a`. They are not required
to build, collect or replay the current tracer.

## Firecracker dependency

The published fork is
[`JooyoungPark73/firecracker`, `firecracker-tracer`](https://github.com/JooyoungPark73/firecracker/tree/firecracker-tracer).
The recovered revision is `209428517ba3f92388e31961183aedea089f6662`, based on
Firecracker v1.13.1 commit `f8d253a0750f2cbad678c74a1153caa7596475c0`.
Its expected Git tree is `ec7add22f40c9bf66610a05dcab55e474046d240`.
[`versions.json`](../../scripts/tsb-firecracker/versions.json) records the pin;
[`firecracker-tracer.patch`](../../scripts/tsb-firecracker/firecracker-tracer.patch)
preserves the complete delta, including VMM code, companion, layouts and fixtures.

The preparation helper reconstructs the fork from the pinned upstream base and
preserved patch, checks the patch SHA-256 and verifies the resulting Git tree:

```sh
python3 scripts/tsb-firecracker/prepare.py .cache/firecracker-tracer
```

The destination must be new. `--source LOCAL_REPO_OR_BUNDLE` permits offline
preparation from an existing Git repository or bundle containing the base.
Preparation also applies the separately hashed
[`overhead.patch`](../../scripts/tsb-firecracker/overhead.patch) optimization
overlay and verifies tree `e8c57989a5ba7022e36283399ca367f61aa1761e`. The
original patch/revision/tree above remain the recovery reference; `--baseline`
selects that exact original source without the overlay. The overlay does not
extend the original access-coverage or device-consistency guarantees.

The overlay indexes immutable dirty bitmaps so later export chunks do not
rescan every earlier byte. Read-only `/proc/kpageflags` and `/proc/kpagecount`
requests coalesce selected PFNs separated by at most four words, within a
64 KiB span. Unselected words are discarded; this trades up to four times the
metadata bytes for fewer reads/seeks. Large gaps remain separate and short/error
reads remain failures. Page-idle bitmap reads/writes retain their sparse 4 KiB
limits and the original finite sampling boundaries. The overlay's tests include
compiled regular-file and dirty-export microbenchmarks; their timings do not
establish live procfs or worker performance.

Adding `--build` to the initial preparation command invokes the fork's native
`./tools/devtool build --release`, using its pinned build container. Preparation
alone does not build, launch a VM or install a worker binary.

Use the reconstructed fork's `docs/firecracker-tracer.md` and native build tools
for prerequisites and fixtures. The source pin selects Rust 1.87.0 and the
recovered target was x86-64 Linux, `x86_64-unknown-linux-musl`. A direct native
build, after installing that toolchain and the required native development
packages, is also possible:

```sh
cd .cache/firecracker-tracer
rustup target add x86_64-unknown-linux-musl
cargo build --release --bin firecracker --target x86_64-unknown-linux-musl
```

On the original Ubuntu/musl build, the C compiler additionally needed
`CFLAGS_x86_64_unknown_linux_musl="-idirafter /usr/include -idirafter /usr/include/x86_64-linux-gnu"`.
The repository's Cargo configuration writes beneath `build/cargo_target/`.
Building does not install a binary into the worker's running daemon.

Dirty tracking must be enabled before boot: set `ARIES_DIRTY_LOG=1` in the actual
Firecracker process environment or configure `track_dirty_pages: true`. Confirm
it through the native machine-config API. Use a fresh VM and the supported HTTP
Pause/Resume contract; snapshot-restored VMs and arbitrary device combinations
are not established by this recovery. The native APIs and their limits are
specified in the fork's tracer documentation.

The separate firecracker-containerd checkout inspected on `amd218` was clean at
`be68640a5d2237f5b427c37c1f5809ec154126c5`. That records worker provenance; it does
not replace ARIES's Go module pin or the ordinary
[vHive worker setup contract](../implementation/vhive.md#prepare-the-worker).

[`stock-firecracker-sdk.patch`](../../scripts/tsb-firecracker/stock-firecracker-sdk.patch)
is a separate optional snapshot-load serialization fix for the native Go SDK:
it omits the container-specific snapshot field from Firecracker's JSON request.
It is not needed for fresh-boot tracing and preparation does not install it.

## OpenClaw dependency

The native patch is based on upstream OpenClaw commit
`2d2ddc43d0dcf71f31283d780f9fe9ff4cc04fe4` (`v2026.7.1`).
[`scripts/tsb-steps/versions.json`](../../scripts/tsb-steps/versions.json) records
its provenance. The exact regenerated
[`native-openclaw.patch`](../../scripts/tsb-steps/native-openclaw.patch) preserves
eight modified TypeScript source/test files from the worker checkout, including
the native diagnostic and background-process metadata changes.

Apply it once to a clean checkout of that base, using ordinary `git apply
--check` followed by `git apply`. Use that checkout's pinned Node and package
manager, frozen lockfile, authored tests and upstream Dockerfile. The existing
[OpenClaw patch build guide](../../scripts/tsb-steps/README.md) documents the native
commands and instrumentation limits. Select the rebuilt image explicitly in
the experiment's versions file; the unmodified published OpenClaw image does
not gain this patch from ARIES plugin staging.

Source restoration and patch checks do not certify a rebuilt native image.
Before new measurements, validate the configured native hooks, concurrent and
sequential tool batches, terminal release, cancellation and evaluation isolation
with deterministic fixtures on the chosen worker.

## Configuration

Use a dedicated KVM-capable worker with the native
[vHive prerequisites](../implementation/vhive.md#prepare-the-worker). The
observer needs permission to reach the selected VMM API socket and discover its
PID/start identity. The VMM needs the host metadata/page-idle permissions required
by the selected metrics. This backend does not attach `strace` or read guest RAM
through an external `/proc/PID/mem` handle.

Prepare an access/dirty JSON configuration with:

```json
{
  "backend": "vmm_owned",
  "companion_path": "scripts/tsb-companion",
  "agent_timeout_overrides": {}
}
```

The empty timeout map keeps native agent deadlines. Omitting that field retains
the recovered suite's historical RStan-specific 5400-second override. The suite
requires attribution configuration for its five detailed tasks. Generate one
from the **actual guest kernel ELF** and committed layouts:

```sh
python3 scripts/tsb-companion/make_config.py \
  --kernel-elf .cache/guest-kernel/vmlinux-5.10.239 \
  --attribution-detail category --resident-sample-pages 0 \
  --output .cache/tracer-attribution.json
```

The ELF must hash to the pinned layout profile; a release filename is
insufficient. The kernel binary is not included. A different kernel requires
fresh validated layouts. See the
[layout provenance](../../scripts/tsb-companion/layouts/linux-5.10.239/README.md).
The retained [layout tools](../../scripts/tsb-companion/README.md#kernel-layout-inputs)
generate and decode base offsets and validate VMCOREINFO overlap. The recovered
probe does not regenerate the owner/name extensions; those pinned profiles
retain their separate validation provenance. No payload compression is enabled
by the generated configuration.

`scripts/tsb-pages/suite.py --prepare` accepts `--selection-file`,
`--access-dirty-config` and `--attribution-config` and freezes their source/config
hashes in a new artifact root. Preparation also needs the pinned task catalog
and built ARIES helper binaries (`aries`, `aries-bridge`, `aries-echo`, and
`aries-ssh-client`). Pass `--owner` for the actual local artifact owner; the
recovered driver default `nehalem` is worker-specific. It is offline manifest generation; `--run`
performs setup and executes the configured model workload. Native image setup,
model credentials and workload execution are separate from source recovery.

For direct integration, [native step observation](../configuration.md#native-step-observation)
stages `scripts/tsb-access-dirty-plugin` using a clean absolute `plugin_dir` and
an observer URL reachable from the harness container. `serve.py` owns the
observer endpoint; ARIES does not start it automatically. The suite manages this
ordering and retains source hashes and lifecycle outcomes. Observer API
responses are accepted only while the selected VMM PID/start identity still
matches; native acknowledgement identities and expected-result progress must
also match the active session and batch. The Go harness retains observer
cleanup identity after failed startup, and observer signal shutdown uses the
bounded cancellation/release path instead of completing an abandoned epoch.

See [access/dirty epochs](tsb-access-dirty.md) for the complete protocol and
[observer timeouts](tsb-observer-timeouts.md) for task-specific waiting behavior.

## Attribution detail and cost

`attribution_detail` in the companion configuration selects the semantic work
done at each boundary. `make_config.py --attribution-detail` accepts:

| Detail | Collected interpretation | Deliberately omitted |
| --- | --- | --- |
| `category` | Page metadata, anonymous/file-cache/shmem/slab/page-table/reserved categories and validated inode identity | Per-boundary task/VMA/PTE walks, owner enrichment, full buddy-list expansion and file paths |
| `process` | Categories plus process/VMA/PTE and owner associations | VMA path and inode/dentry/mount filename resolution |
| `file` (default) | The process-level evidence plus file identity/path resolution | No automatic claim that every backing page has a reachable pathname |

Category mode reports only directly recognized buddy heads as `free_buddy_head`;
compound tails and other unresolved pages stay explicit. Skipped process/owner
measurements are unavailable, not zero. Owner enrichment can refine categories,
so a change in category counts between modes need not indicate changed guest
RAM. File and process associations describe boundary state, not which process
caused an access. Bootstrap still validates the pinned kernel and process/VMA
layout once in all modes.

`--resident-sample-pages 0` disables the optional attribution sample; the default
is 1024. A positive value still needs the native residency census and sampling
work. Record the selected/sample GPA hashes, detail, kernel/layout and source
identity when comparing costs.

The companion publishes `attribution/decode-metrics.json` with stage wall/CPU
time, provider-read counts, closure bytes and interpretation coverage.
`companion-metrics.json` separately times bootstrap or capture/decode, seal, and
complete child publication. Skipped stages have unavailable durations. Failed
phases retain elapsed work and failure status. The child publication phase
includes its attribution artifacts and raw VMM exports; later controller
consistency checks and record acknowledgements remain separate.

A local synthetic comparison is reproducible without a worker or model:

```sh
python3 scripts/tsb-companion/benchmark_detail.py \
  --output .cache/attribution-detail-benchmark.json --repeats 11 \
  --processes 8 --pages-per-category 16 --resident-sample-pages 0
```

This uses synthetic x86-64 metadata and an in-memory provider with the same
selected pages across modes. It measures Python decoder work and metadata
closure on that fixture. It does not measure native HTTP, bit tracking, pauses,
real workload delay or universal speedup. Optional synthetic resident sampling
uses its declared prefix fixture and cannot establish native sampling cost.

## Overhead reports

Suite analysis writes `overhead.json` and `overhead.html` beside each task's
comparison report. A retained observer directory can also be analyzed directly:

```sh
python3 scripts/tsb-pages/overhead.py \
  --observer runs/TRACER/TASK/observer \
  --output runs/TRACER/TASK/overhead.json
```

Add `--plugin-log PATH` to include requested-to-confirmed waits measured within
one native plugin process. No absolute plugin/host clock subtraction is used.
The JSON records consumed artifact hashes, missing timing and input errors.
Headline cost statistics use complete, nonfailed, noncensored records only.
Raw attempts and separate completion-state tables retain failed, censored and
incomplete costs, including partial elapsed work. These states describe the
recorded operation or capture, not hardware coverage or benchmark success.
Native timing deduplication is performed within each selected aggregate, so an
earlier incomplete attempt cannot suppress a later complete measurement.
New append-only `api-calls.jsonl` receipts are preferred; older `api-calls.json`
histories remain readable without counting both.

The report separates host boundary envelopes and nested components, native
idle/dirty/census work, companion detail/stage costs, and API counts/bytes.
Shared immutable metadata and repeated native timing receipts are counted once.
Interval unions expose nested work and residual unattributed boundary time;
component totals must not be added to their parent envelopes. The host boundary
includes API-journal persistence but excludes its own boundary-line write,
network and helper delay.

A metadata barrier is only the remaining wait exposed at the next boundary.
The decoder can already have worked during useful LLM time, and sealed
publication can overlap later work. The full paused LLM interval is policy time,
not automatic tracer overhead. Native tool-window timing already begins after
arm and ends before final collection, so those costs must not be subtracted
again. No adjusted or counterfactual uninstrumented latency is calculated.

Compare detail modes only with their selection/sample, layout, epoch and source
bindings visible. Aggregate mode tables describe observed records and do not
calculate unmatched mode speedups. Suite summaries keep host collection, native
bit work and attribution costs separate; VMM residency denominators use validated
initial RAM regions and immutable epoch census.
The window-footprint/endpoint-residency ratio can exceed one when pages disappear
during the window; it is not clamped or interpreted as simultaneous residency.

## Verification and recovery provenance

Deterministic source checks include:

```sh
make build
make test
python3 -m unittest discover -s scripts/tsb-pages -p 'test_*.py'
python3 -m unittest discover -s scripts/tsb-companion -p 'test_*.py'
python3 -m unittest discover -s scripts/tsb-firecracker -p 'test_*.py'
node --test scripts/tsb-access-dirty-plugin/test*.mjs
```

The local fixtures use simulated APIs, native hook events and disposable files.
They cover wrong/stale identities, incomplete or contradictory acknowledgements,
failed-start ownership, signal cancellation and compact evidence consistency.
Compact readers reject overlapping access states, incomplete VMM partitions,
contradictory unknown reasons, dirty unions or retained summary counts even when
individual file hashes match. Suite progress uses the initial VMM arm record
for authoritative RAM regions and generation. The compact producer reconstructs
the native export's omitted all-samples-absent pages per RAM region, without including GPA holes or bitmap
padding. This absence mask is distinct from final census residency. Suite
session unions remain unavailable across different VM incarnations.
These checks do not change the original Firecracker source pin or extend
its access-coverage guarantees.

Native Firecracker tests additionally require the documented KVM/device
prerequisites and guest fixture inputs. Exact replay requires retained captures
from a later run. Neither unit checks nor recovery of code establishes access
coverage, low overhead, completed benchmark traces or safe page eviction.

With the native Rust prerequisites installed, the optimized checkout also has
pure VMM tests and explicitly ignored local microbenchmarks:

```sh
cargo test -p vmm --release --target x86_64-unknown-linux-gnu page_tracking::host_pages::tests
cargo test -p vmm --release --target x86_64-unknown-linux-gnu page_tracking::records::tests
cargo test -p vmm --release --target x86_64-unknown-linux-gnu benchmark_sparse_metadata_coalescing -- --ignored --nocapture
cargo test -p vmm --release --target x86_64-unknown-linux-gnu benchmark_dirty_export_prefix_index -- --ignored --nocapture
```

These commands run inside the reconstructed Firecracker source directory. They
exercise compiled code with synthetic inputs and regular files; they do not
boot a guest or replace native read/write/restore and lifecycle validation. For
actual overhead, use matched fresh-VM runs with tracing off, a pause-only
control, idle/dirty tracing, and category/process/file attribution, keeping the workload, source/configuration,
pause policy, GPA/sample selection and native runtime identity explicit. Report
workload delay, boundary waits, CPU and memory separately. Compare the baseline
and optimized native trees explicitly when assessing the overlay.

The recovered baseline came from CloudLab workers `amd205` and `amd218`;
their principal observer/plugin/suite source hashes matched. The final worker
source snapshot supplied the harness/runner/lifecycle tests. The OpenClaw patch
was regenerated from its eight modified files, and the clean Firecracker
checkout matched the published pin. The original 29 companion files matched
that commit's `tools/aries-attribution` blobs. Current host identity, lifecycle,
compact-data validation, attribution detail and cost reporting include later
repository changes. The exact original Firecracker delta and dependency pins
remain the recovery reference; the separately versioned optimization overlay
is explicit in its manifest. No private worker artifact is required as a build
or collection input.
