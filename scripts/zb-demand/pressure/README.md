# Controlled regular-file pressure calibration

This calibration compares a 32 MiB and a 512 MiB private ordinary file in
the same optimized Firecracker block-root stack. Each observation begins
with one 2 GiB, one-vCPU VM checkpointed and restored with an idle guest
task. Its diagnostic root contains only fixed vsock operations. The
regular file is created after restore in the idle task's writable 10 GiB
filesystem. This keeps it separate from both the diagnostic root and
the Harbor trajectory.

Use matching isolated native-v2 and v3 runtime profiles and one immutable
kernel and diagnostic root. Provision each with
`scripts/zb-trace/provision_baseline.sh`. Native-v2 requests the same
zero-target reporting balloon as v3 and has guest DAMON disabled. Both
use the optimized native checkpoint implementation and reporting order 9.
The controller checks the restored balloon and DAMON state before fill
and during the cold window.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -o "$STACK/tools/zb-pressure-guest" ./scripts/zb-demand/pressure/guest
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath \
  -o "$STACK/tools/zb-pressure-driver" ./scripts/zb-demand/filecal/driver
scripts/zb-demand/filecal/install-root.sh "$ORIGINAL_ROOT" \
  "$STACK/tools/zb-pressure-guest" "$STACK/common-pressure-root.ext4" zb-pressure
```

Make the diagnostic root available as `$STACK/{native-v2,v3}/runtime/common-rootfs.ext4`.
Make each worker runtime's native stack and matching profile available.
Stage this folder and `scripts/zb-trace` under `$STACK/tools/scripts`.
Run one observation at a time on the same worker. Interleave size, variant
and repeat order. Do not run this calibration over a shared workload VM.

```sh
scripts/zb-demand/pressure/run.sh "$STACK" native-v2 large large-nv2-r1
scripts/zb-demand/pressure/run.sh "$STACK" v3 large large-v3-r1
python3 scripts/zb-demand/pressure/analyze.py \
  "$STACK/evidence/large-nv2-r1" "$STACK/evidence/large-v3-r1" > pressure.json
```

Each run prewarms the shared immutable root file by reading it and uses
a fresh VM, task filesystem, and generated data file. The guest writes a
deterministic byte pattern, runs fsync, and rereads the full file and
validates its hash. After at least 65 seconds without accessing the file
contents, it rereads the file and checks the same hash again. The raw
receipts retain host-monotonic timing for the aligned fill, cold and
reread phases and guest write, sync and read times. The observer also
checks file cache occupancy through mincore at the three phase boundaries.
Mincore observes cache residency without reading file content. The cold
interval includes telemetry about guest memory and DAMON state.

One-second host PSS samples are assigned to the midpoint of each actual
measurement bracket. Integration uses linear interpolation of adjacent
samples at phase boundaries. Peak describes the sampled path and can
miss peaks shorter than one second. The first 50 seconds of cold are
integrated at fixed duration across arms. Guest proc and DAMON telemetry
is sampled at about one-second cadence only during cold, plus before
fill and around reread. Balloon metrics record interval deltas.
`free_page_report_freed` counts successful reporting descriptor bytes,
which may revisit ranges and include already nonresident backing. It
does not measure unique bytes released from host RAM.

See [Firecracker free-page reporting](https://github.com/firecracker-microvm/firecracker/blob/v1.18.0/src/vmm/src/devices/virtio/balloon/device.rs)
and [metric serialization](https://github.com/firecracker-microvm/firecracker/blob/v1.18.0/src/vmm/src/logger/metrics.rs).
