#!/usr/bin/env bash
# Capture KVM demand while the original TeX session runs on a provisioned stack.
set -euo pipefail
work=$(realpath "$1")
unit=$2
asset=$(realpath "$3")
variant=$4
name=$5
out=$(realpath -m "$6")
probe=$(realpath "$7")
test ! -e "$out"
test ! -e "$asset/runs/$name"
if pgrep -x 'firecracker|[.]firecracker-re'; then exit 2; fi
mkdir -m 0700 "$out"
sha256sum "$work/bin/firecracker" "$work/common-vmlinux.bin" "$work/common-rootfs.ext4" \
    "$work/runtime.json" "$asset/aries-tex" "$asset/trajectory.json" "$asset/rollout.jsonl" \
    "$asset/run-reviewed-pilot.sh" "$asset/tracer/"{idle_hook.py,idle.py,kvm-slots.bt,run-traced.sh} "$probe" > "$out/inputs.sha256"
cp "$work/runtime.json" "$out/runtime.json"
cat /proc/sys/kernel/random/boot_id > "$out/boot-id"
bpftrace --version > "$out/bpftrace-version"
bpftrace -q -B line "$probe" > "$out/demand.events" 2> "$out/demand.stderr" &
pid=$!
finish() {
    if kill -0 "$pid" 2>/dev/null; then kill -INT "$pid"; fi
    wait "$pid" || true
}
trap finish EXIT
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$out/demand.events"; then break; fi
    kill -0 "$pid"
    sleep 0.1
done
grep -q '^ready ' "$out/demand.events"
rc=0
bash "$asset/run-reviewed-pilot.sh" "$work" "$unit" "$asset" "$variant" "$name" || rc=$?
finish
trap - EXIT
printf '%s\n' "$rc" > "$out/driver.exit"
exit "$rc"
