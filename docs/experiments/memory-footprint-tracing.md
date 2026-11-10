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
| [`scripts/tsb-firecracker/fixtures/legacy-native/`](../../scripts/tsb-firecracker/fixtures/legacy-native) | Historical native validation helpers, mock servers and source checksums recovered before worker reset |
| [`scripts/tsb-companion/`](../../scripts/tsb-companion) | VMM API client, decoder, capture/replay, tests and pinned kernel layouts |
| [`scripts/tsb-pages/`](../../scripts/tsb-pages) | Observer server, VMM coordinator, suite preparation, boundary audits and comparisons |
| [`scripts/tsb-access-dirty-plugin/`](../../scripts/tsb-access-dirty-plugin) | Native batch, model-occurrence and terminal lifecycle instrumentation |
| [`scripts/tsb-steps/`](../../scripts/tsb-steps) | Exact OpenClaw source patch, dependency manifest and metadata diagnostics |
| [`pkg/harness/openclaw/step_observation.go`](../../pkg/harness/openclaw/step_observation.go) | Plugin staging and host-owned post-stop observer-release barrier |
| [`pkg/deployment/vhive/memory.go`](../../pkg/deployment/vhive/memory.go) | Separate process-mapping observations for native VMs |
| [`profiles/experiments/tsb-memory-footprint/`](../../profiles/experiments/tsb-memory-footprint), [`tsb-tool-pages/`](../../profiles/experiments/tsb-tool-pages) | Earlier research profile templates; adapt worker paths and runtime images before reuse |

`scripts/tsb-attribution`, `tsb-access`, `tsb-pages-plugin` and `tsb-uffd` preserve
earlier external host-memory, idle-page and restore-fault approaches. They have
different consistency and measurement contracts. They are not substitutes for
the VMM-owned backend's generation-bound capture protocol.

The historical idle-only observer lacks the current host stop protocol's
`/session/abort` acknowledgement. Its old profiles therefore block isolation with
the recovered harness. Adapt that release contract before reusing those profiles;
the VMM-owned configuration implements the required bounded release barrier.

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
[Step diagnostics README](../../scripts/tsb-steps/README.md) documents the native
commands and instrumentation limits. Select the rebuilt image explicitly in
the experiment's versions file; the unmodified published OpenClaw image does
not gain this patch from ARIES plugin staging.

Source restoration and patch checks do not certify a rebuilt native image.
Before new measurements, validate the configured native hooks, concurrent and
sequential tool batches, terminal release, cancellation and evaluation isolation
with deterministic fixtures on the chosen worker.

## Configuration without historical run directories

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
  --output .cache/tracer-attribution.json
```

The ELF must hash to the pinned layout profile; a release filename is
insufficient. The kernel binary is not included. A different kernel requires
fresh validated layouts. See the
[layout provenance](../../scripts/tsb-companion/layouts/linux-5.10.239/README.md).
No payload compression is enabled by the generated configuration.

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
ordering and retains source hashes and lifecycle outcomes.

See [access/dirty epochs](tsb-access-dirty.md) for the complete protocol and
[observer timeouts](tsb-observer-timeouts.md) for task-specific waiting behavior.
The earlier [idle-page suite](tsb-tool-pages-suite.md),
[host-side attribution](tsb-page-attribution.md) and
[memory investigation](tsb-memory-footprint.md) are historical method references.
Their private evidence paths and past paid-run decisions are not prerequisites
or authorization for a new run.

## Verification and recovery provenance

Deterministic source checks include:

```sh
make build
make test
python3 -m unittest discover -s scripts/tsb-pages -p 'test_*.py'
python3 -m unittest discover -s scripts/tsb-companion -p 'test_*.py'
python3 -m unittest discover -s scripts/tsb-attribution -p 'test_*.py'
node --test scripts/tsb-access-dirty-plugin/test*.mjs
node --test scripts/tsb-pages-plugin/test.mjs
node --test scripts/tsb-steps/test.mjs
```

Native Firecracker tests additionally require the documented KVM/device
prerequisites and guest fixture inputs. Exact replay requires retained captures
from a later run. Neither unit checks nor recovery of code establishes access
coverage, low overhead, completed benchmark traces or safe page eviction.

The current integration came from `/home/nehalem/aries` on CloudLab workers
`amd205` and `amd218`; their latest principal observer/plugin/suite source hashes
matched. The ignored worker artifact
`runs/tsb-access-dirty-001/rerun-lifecycle-implementation-001/source-final-001.tar.gz`
provided the final harness/runner/lifecycle test snapshot. The OpenClaw patch was
regenerated from the eight live modified files because the older saved patch
was not an exact restoration. The Firecracker checkout was clean at the
published pin, and all 29 released companion files matched that commit's
`tools/aries-attribution` blobs. Older worker variants remain distinct provenance;
they are not silently mixed into the selected current implementation.
