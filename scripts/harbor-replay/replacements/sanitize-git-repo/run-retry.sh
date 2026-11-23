#!/usr/bin/env bash
# One measured cell. The launcher gives this stage exclusive use of the worker.
set -euo pipefail
stage=$(realpath "$1")
old=$(realpath "$2")
trial=$3
variant=$4
mode=$5
node=$(cat /var/emulab/boot/nodeid)
case "$node:$variant:$mode" in
  amd203:v1:ordinary) unit=fcmod-zepbound-demand-amd203; runtime=/users/nehalem/zepbound-demand-amd203/runtime ;;
  amd203:v1:deep) unit=fcmod-zepbound-deep203cf; runtime=/local/zb-deep-cf-v1-amd203/runtime ;;
  amd191:v3:ordinary) unit=fcmod-zepbound-amd191-v3; runtime=/local/zb-fc-v4/v3/runtime ;;
  amd198:v4:ordinary) unit=fcmod-zepbound-amd198-imagecircuit; runtime=/local/zb-imagecircuit-v4/runtime ;;
  amd198:v4:deep) unit=fcmod-zepbound-amd198-deep-circuit; runtime=/local/zb-amd198-deep-v4/circuit/runtime ;;
  amd200:v2:ordinary) unit=fcmod-zepbound-amd200; runtime=/local/zb-v2/runtime ;;
  *) exit 2 ;;
esac
case "$node" in
  amd203)
    tools=/local/zb-corpus-pypi-cohort-v1/codex-harbor-tools.tar.gz
    inherited_net=/tmp/zb-resume-extra-01/marker-cni
    export PATH=/users/nehalem/zepbound-demand-amd203/tools/bpf/usr/bin:$PATH
    export LD_LIBRARY_PATH=/users/nehalem/zepbound-demand-amd203/tools/bpf/usr/lib/x86_64-linux-gnu ;;
  amd191) tools=/local/zb-fc-v4/pilot/pypi-cohort-amd191/codex-harbor-tools.tar.gz
    inherited_net=/tmp/zb-resume-extra-01/marker-cni ;;
  amd198) tools=/local/zb-imagecircuit-v4/video-assigned-assets/codex-harbor-tools.tar.gz
    inherited_net="$old/net" ;;
  amd200) tools=/local/zb-corpus-financeapt-cohort-v2/codex-harbor-tools.tar.gz
    inherited_net=/local/zb-corpus-financeapt-cohort-v2/cni ;;
esac
key=$trial-$variant-sanitize-retry-$mode-02
umask 077
out="$stage/runs/$key"
mkdir "$out"
trap 'printf "controller error at line %s\n" "$LINENO" >"$out/controller.error"' ERR
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
(
  test -d "$old/net/bin" || { mkdir -p "$old/net"; cp -a "$inherited_net/bin" "$inherited_net/SHA256SUMS" "$old/net/"; }
  bash "$stage/network.sh" "$old/net" install "$node" cleanse
) >"$out/network-readback.log" 2>&1
bpftrace --version >"$out/bpftrace-version.txt"
image=$(python3 -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["image"])' "$stage/retry-source.json")
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
if ! "${ctr[@]}" images ls -q | grep -Fx "$image" >"$out/image.txt"; then
    "${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$image" >"$out/image-pull.log" 2>&1
    "${ctr[@]}" images ls -q | grep -Fx "$image" >"$out/image.txt"
fi
extra=()
if [[ $mode == deep ]]; then extra+=(--deep); fi
status=0
python3 -B "$stage/vmm_exclusivity.py" "$out/placement.jsonl" -- \
    python3 -B "$stage/retry-worker.py" "$trial" "$variant" "$stage" "$old" "$runtime" "$tools" \
    "$unit" "$out/result" "${extra[@]}" >"$out/launcher.log" 2>&1 || status=$?
printf '%d\n' "$status" >"$out/worker.exit"
"${audit[@]}" >"$out/idle-after.json"
exit "$status"
