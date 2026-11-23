#!/usr/bin/env bash
# Execute literal CompCert recordings on the selected genuine deployment.
set -euo pipefail
stage=$(realpath "$1")
variant=$2
unit=$3
rt=$(realpath "$4")
plugins=$(realpath "$5")
tools=$(realpath "$6")
shift 6
cd "$stage"
node=$(cat /var/emulab/boot/nodeid)
out="$stage/$variant"
mkdir "$out"
sha256sum --quiet -c inputs.sha256
echo "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab  $tools" | sha256sum -c -
echo "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f  $rt/common-rootfs.ext4" | sha256sum -c -
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
python3 - "$rt/runtime.json" "$variant" <<'PY'
import json,sys
p=json.load(open(sys.argv[1]));v=sys.argv[2]
assert v in ('v1','v2','v3','v4')
assert bool(p.get('root_drive_use_pmem'))==(v=='v4')
assert bool(p.get('free_page_reporting'))==(v=='v2')
assert bool(p.get('guest_reclamation',{}).get('Enabled'))==(v in ('v3','v4'))
assert not p.get('smt_enabled')
PY
cp "$rt/runtime.json" "$out/profile.json"
image=docker.io/alexgshaw/compile-compcert@sha256:f60236a4fe55bbb3c573210fe0fcd91551f4ee97c417453cd4985be074b35411
mode=()
case "$variant" in v3|v4) mode=(-native-checkpoint -guest-task-v3);; esac
ctr=("$rt/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
if ! "${ctr[@]}" image ls -q | grep -Fxq "$image"; then
    "${ctr[@]}" images pull --platform linux/amd64 "$image" > "$out/image-pull.log" 2>&1
fi
mkdir -p "$out/cni/bin"
cp -a "$plugins/bin/." "$out/cni/bin/"
cp "$plugins/SHA256SUMS" "$out/cni/"
test ! -e /sys/class/net/zbccsnap183
if systemctl is-active --quiet zb-ccsnap-dns-amd183 || iptables -t mangle -S ZB183CCSNAP >/dev/null 2>&1; then exit 2; fi
trap 'bash "$stage/provision-pypi-cni.sh" "$out/cni" remove compiler-snapshot-amd183' EXIT
ZB_TLS_CANDIDATES="$stage/hosts.json" bash provision-pypi-cni.sh "$out/cni" install compiler-snapshot-amd183 > "$out/network.log"
export ZB_VMM_CENSUS=1 ZB_CHECKPOINT_NAMESPACE_DIR="$rt/checkpoints/aries"
for trial in "$@"; do
    src=$(realpath "$stage"/sources/"$trial"*)
    id=$(basename "$src"); p="$out/$id"; mkdir "$p"
    python3 -B audit-idle-runtime.py "$unit" "$rt" > "$p/idle-before.json"
    python3 -B - "$p/idle-before.json" <<'PY'
import json,sys
a=json.load(open(sys.argv[1]))
assert all(not a[k]['vmm_processes'] and not any(a[k]['tasks_by_socket'].values()) for k in ('before','after'))
PY
    rc=0
    python3 -B vmm_exclusivity.py "$p/placement.jsonl" -- "$stage/tracer/run-traced.sh" "$stage/aries-relaxed-compcert" "$stage/tracer" "$p/memory" \
       -diagnostic-process-drift -trajectory "$src/trajectory.json" -rollout "$src/rollout.jsonl" \
       -namespace aries -socket "/run/$unit/containerd.sock" -checkpoint-dir "$rt/checkpoints" -shim-dir "$rt/shim" -io-dir "/run/$unit/aries-io" \
       -image "$image" "${mode[@]}" -codex-tools "$tools" -os-tools "$stage/compile-compcert-ca.tar.gz" \
       -cni-network zbccsnap183 -cni-bin-dir "$out/cni/bin" -cni-conf-dir "$out/cni/conf" -cni-cache-dir "$out/cni/cache" > "$p/driver.log" 2>&1 || rc=$?
    printf '%s\n' "$rc" > "$p/driver.exit"
    python3 -B audit-idle-runtime.py "$unit" "$rt" > "$p/idle-after.json"
    printf '%s %s %s %s\n' "$node" "$variant" "$id" "$rc" >> "$out/series-status.txt"
done
