#!/usr/bin/env bash
# Run one new native checkpoint and diagnostics on the dedicated worker.
set -euo pipefail
stack=$(realpath "$1")
variant=$2
size=$3
label=$4
[[ "$label" =~ ^[a-z0-9-]+$ && "$variant" =~ ^(native-v2|v3)$ && "$size" =~ ^(small|large)$ ]]
unit=fcmod-zepbound-pressure-$variant
work="$stack/$variant/runtime"
run="$stack/evidence/$label"
mkdir -m 700 "$run"
mkdir -m 700 "$run/io"
cp "$work/runtime.json" "$run/runtime.json"
(
    uname -r
    cat /sys/devices/system/cpu/smt/{active,control}
    cat /proc/swaps
    ps -C firecracker -o pid=,comm= || true
    cat /proc/meminfo
) > "$run/host-before.txt"
sha256sum "$stack"/tools/zb-pressure-{guest,driver} "$work"/{common-rootfs.ext4,common-vmlinux.bin,bin/firecracker,bin/containerd-shim-aws-firecracker} > "$run/artifact-sha256.txt"
# Warm the shared immutable root inode before each fresh VM.
dd if="$work/common-rootfs.ext4" of=/dev/null bs=8M status=none
options=(-native-checkpoint)
if [[ "$variant" == native-v2 ]]; then options+=(-native-reporting-only); fi
# Optional stock metric FIFO sampler. It identifies the VMM after PSS starts.
sudo -n python3 "$stack/tools/scripts/zb-trace/balloon_metrics.py" "$run" "$work/shim"     > "$run/balloon-metrics.jsonl" 2> "$run/balloon-metrics.stderr" &
metrics=$!
cleanup() {
    kill "$metrics" 2>/dev/null || true
    wait "$metrics" || true
}
trap cleanup EXIT
set +e
sudo -n env "ZB_PRESSURE_RUN=$run" "ZB_PRESSURE_VARIANT=$variant" "ZB_PRESSURE_SIZE=$size"     "$stack/tools/zb-pressure-driver" "${options[@]}"     -socket "/run/$unit/containerd.sock" -namespace aries     -shim-dir "$work/shim" -checkpoint-dir "$work/checkpoints" -io-dir "$run/io"     -image docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74     -hook "$stack/tools/scripts/zb-demand/pressure/observe.py" > "$run/driver.stdout" 2> "$run/driver.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$run/driver.status"
sudo -n chmod a+r "$run"/phases.jsonl "$run"/samples.jsonl 2>/dev/null || true
cat "$run/driver.stderr"
exit "$status"
