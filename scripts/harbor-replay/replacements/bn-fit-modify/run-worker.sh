#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1")
trial=$2
variant=$3
mode=$4
runtime=$(realpath "$5")
unit=$6
tools=$(realpath "$7")
[[ $trial =~ ^[a-f0-9]{8}$ && $variant =~ ^v[1-4]$ && $mode =~ ^(ordinary|deep)$ ]]
[[ $unit =~ ^fcmod-zepbound-[a-z0-9-]+$ ]]
out=$stage/runs/$trial-$variant-bn-fit-$mode-01
umask 077
mkdir "$out"
trap 'printf "worker error at line %s\n" "$LINENO" >"$out/controller.error"' ERR
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
test "$(systemctl show "$unit" -p EffectiveCPUs --value)" = "$(cat /sys/devices/system/cpu/online)"
exec 9>"$stage/worker.lock"
flock -n 9
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
sockets=()
while IFS= read -r socket; do
    if [[ $socket != "/run/$unit/containerd.sock" ]]; then sockets+=(--also-socket "$socket"); fi
done < <(find /run -maxdepth 2 -type s -path '/run/fcmod-zepbound-*/containerd.sock' | sort)
audit=(python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" "${sockets[@]}")
"${audit[@]}" >"$out/idle-before.json"
cp "$runtime/runtime.json" "$out/runtime-config.json"
bpftrace --version >"$out/bpftrace-version.txt"
image=$(python3 -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["reviewed"]["image"])' "$stage/source.json")
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
"${ctr[@]}" images ls -q | grep -Fx "$image" >"$out/image.txt"
status=0
python3 -B "$stage/vmm_exclusivity.py" "$out/placement.jsonl" -- \
    python3 -B "$stage/worker.py" "$trial" "$variant" "$mode" "$stage" "$runtime" "$tools" \
    "$unit" "$out/result" >"$out/launcher.log" 2>&1 || status=$?
printf '%d\n' "$status" >"$out/worker.exit"
"${audit[@]}" >"$out/idle-after.json"
exit "$status"
