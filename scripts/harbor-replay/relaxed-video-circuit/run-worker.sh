#!/usr/bin/env bash
set -euo pipefail
base=$(realpath "$1")
trial=$2
variant=$3
runtime=$(realpath "$4")
unit=$5
fixtures=$(realpath "$6")
cni=$(realpath "$7")
image=$8
stage=$base/runs/$trial-$variant-relaxed-01
umask 077
mkdir "$stage"
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
test "$(systemctl show "$unit" -p EffectiveCPUs --value)" = "$(cat /sys/devices/system/cpu/online)"
exec 9>"$base/worker.lock"
flock -n 9
sockets=()
while IFS= read -r socket; do
  if [[ $socket != "/run/$unit/containerd.sock" ]]; then sockets+=(--also-socket "$socket"); fi
done < <(find /run -maxdepth 2 -type s -path '/run/fcmod-zepbound-*/containerd.sock' | sort)
python3 -B "$base/audit-idle-runtime.py" "$unit" "$runtime" "${sockets[@]}" >"$stage/idle-before.json"
cp "$runtime/runtime.json" "$stage/runtime-config.json"
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
if ! "${ctr[@]}" images ls -q | grep -Fx "$image" >"$stage/image.txt"; then
  "${ctr[@]}" images pull "$image" >"$stage/image-pull.log" 2>&1
  "${ctr[@]}" images ls -q | grep -Fx "$image" >"$stage/image.txt"
fi
status=0
python3 -B "$base/vmm_exclusivity.py" "$stage/placement.jsonl" -- \
    python3 -B "$base/worker.py" "$trial" "$variant" "$base/aries-harbor-relaxed" "$base" "$fixtures" \
    "$base/tracer" "$runtime" "$(cat "$base/adapter.sha256")" "$unit" "$stage/result" --cni "$cni" \
    >"$stage/launcher.log" 2>&1 || status=$?
printf '%d\n' "$status" >"$stage/worker.exit"
python3 -B "$base/audit-idle-runtime.py" "$unit" "$runtime" "${sockets[@]}" >"$stage/idle-after.json"
exit "$status"
