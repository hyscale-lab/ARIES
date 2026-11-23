#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1")
trial=$2
variant=$3
assets=$(realpath "$4")
tracer=$(realpath "$5")
runtime=$(realpath "$6")
unit=$7
image=$8
shift 8
[[ $(id -u) == 0 && $unit == fcmod-zepbound-* ]]
[[ $(cat /sys/devices/system/cpu/smt/active) == 0 ]]
[[ $(systemctl show "$unit" -p EffectiveCPUs --value) == "$(cat /sys/devices/system/cpu/online)" ]]
umask 077
test ! -e "$stage/idle-before.json"
if [[ -n ${ZB_RECOVERY_BPF_PREFIX:-} ]]; then
  export PATH="$ZB_RECOVERY_BPF_PREFIX/usr/bin:$PATH"
  export LD_LIBRARY_PATH="$ZB_RECOVERY_BPF_PREFIX/usr/lib/x86_64-linux-gnu:$ZB_RECOVERY_BPF_PREFIX/usr/lib/llvm-18/lib"
fi
bpftrace --version >"$stage/bpftrace-version.txt"
sockets=()
while IFS= read -r socket; do
  if [[ $socket != "/run/$unit/containerd.sock" ]]; then sockets+=(--also-socket "$socket"); fi
done < <(find /run -maxdepth 2 -type s -path '/run/fcmod-zepbound-*/containerd.sock' | sort)
python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" "${sockets[@]}" >"$stage/idle-before.json"
cp "$runtime/runtime.json" "$stage/runtime-config.json"
systemctl show "$unit" -p EffectiveCPUs -p AllowedCPUs -p CPUQuotaPerSecUSec >"$stage/service-cpu.txt"
sha256sum "$stage"/{replay.py,worker.sh,trace-contract.py,audit-idle-runtime.py,vmm_exclusivity.py,aries-normal-launchclock-v3} >"$stage/launch-artifacts.sha256"
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
if ! "${ctr[@]}" images ls -q | grep -Fxq "$image"; then
  "${ctr[@]}" images pull "$image" >"$stage/image-pull.log" 2>&1
fi
"${ctr[@]}" images ls -q | grep -Fx "$image" >"$stage/image.txt"
status=0
python3 -B "$stage/vmm_exclusivity.py" "$stage/placement.jsonl" -- \
  python3 -B "$stage/replay.py" "$trial" "$variant" "$stage/aries-normal-launchclock-v3" \
  "$assets" "$tracer" "$stage/trace-contract.py" "$runtime" "$unit" "$stage/result" "$@" \
  >"$stage/launcher.log" 2>&1 || status=$?
printf '%d\n' "$status" >"$stage/worker.exit"
python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" "${sockets[@]}" >"$stage/idle-after.json"
exit "$status"
