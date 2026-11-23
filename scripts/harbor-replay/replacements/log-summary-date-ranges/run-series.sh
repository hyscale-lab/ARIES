#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1");variant=$2;level=$3;unit=$4;rt=$(realpath "$5");shift 5
cd "$stage"
node=$(cat /var/emulab/boot/nodeid)
[[ $node =~ ^amd(182|189)$ && $variant =~ ^v[1-4]$ && $level =~ ^(ordinary|deep)$ ]]
sha256sum --quiet -c inputs.sha256
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
out="$stage/$level-$variant"; mkdir "$out"
root=292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f
if [[ $level == deep ]]; then
    [[ $variant == v1 || $variant == v4 ]]
    [[ $# == 1 && $1 == aa46 ]]
    root=8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5
    export ZB_GUEST_VSOCK=1 ZB_CONTENT_SAMPLE=8192
fi
echo "$root  $rt/common-rootfs.ext4" | sha256sum -c -
cp "$rt/runtime.json" "$out/profile.json"
python3 - "$out/profile.json" "$variant" <<'PY'
import json,sys
p=json.load(open(sys.argv[1]));v=sys.argv[2]
assert bool(p.get('root_drive_use_pmem'))==(v=='v4')
assert bool(p.get('free_page_reporting'))==(v=='v2')
assert bool(p.get('guest_reclamation',{}).get('Enabled'))==(v in ('v3','v4'))
assert not p.get('smt_enabled')
PY
image=docker.io/alexgshaw/log-summary-date-ranges@sha256:cbeb6ba905c2fec294f16cd5e16e3ea7f2e04d38ac2484d51a11de262aa7dc51
mode=()
case "$variant" in v3|v4) mode=(-native-checkpoint -guest-task-v3);; esac
ctr=("$rt/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
if ! "${ctr[@]}" images ls -q | grep -Fxq "$image"; then
    "${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$image" > "$out/image-pull.log" 2>&1
fi
export ZB_VMM_CENSUS=1 ZB_CHECKPOINT_NAMESPACE_DIR="$rt/checkpoints/aries"
for prefix in "$@"; do
    src=$(realpath "$stage"/sources/"$prefix"*)
    id=$(basename "$src"); p="$out/$id"; mkdir "$p"
    python3 -B audit-idle-runtime.py "$unit" "$rt" > "$p/idle-before.json"
    python3 -B - "$p/idle-before.json" <<'PY'
import json,sys
a=json.load(open(sys.argv[1]))
assert all(not a[k]['vmm_processes'] and not any(a[k]['tasks_by_socket'].values()) for k in ('before','after'))
PY
    patch=()
    if [[ $prefix == 56d7 ]]; then patch=(-os-tools "$stage/circuit-patch-debs.tar.gz"); fi
    rc=0
    python3 -B vmm_exclusivity.py "$p/placement.jsonl" -- "$stage/tracer/run-traced.sh" "$stage/aries-log-summary" "$stage/tracer" "$p/memory" \
      -diagnostic-process-drift -trajectory "$src/trajectory.json" -rollout "$src/rollout.jsonl" \
      -namespace aries -socket "/run/$unit/containerd.sock" -checkpoint-dir "$rt/checkpoints" -shim-dir "$rt/shim" -io-dir "/run/$unit/aries-io" \
      -image "$image" "${mode[@]}" -codex-tools "$stage/codex-harbor-tools.tar.gz" "${patch[@]}" > "$p/driver.log" 2>&1 || rc=$?
    printf '%s\n' "$rc" > "$p/driver.exit"
    python3 -B audit-idle-runtime.py "$unit" "$rt" > "$p/idle-after.json"
    printf '%s %s %s %s %s\n' "$node" "$level" "$variant" "$id" "$rc" >> "$out/series-status.txt"
done
