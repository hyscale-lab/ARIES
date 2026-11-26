#!/bin/bash
set -euo pipefail
stage=/local/zb-roy-reclaim-04
original=/local/zb-tb41-roy-d16a13fe
runtime=/local/zb-v5-pilot-01/runtime
unit=fcmod-zepbound-amd191-v5-pilot-01
test "$(cat /var/emulab/boot/nodeid)" = amd191
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
cd "$stage"
sha256sum -c SHA256SUMS
python3 -B - <<'PYIMPORT'
from pathlib import Path
import runpy, sys
p=Path('zb-trace/tb41').resolve(strict=True)
sys.path.insert(0,str(p))
assert callable(runpy.run_path(str(p/'sample.py'))['sample'])
assert callable(runpy.run_path(str(p/'collect.py'))['serve'])
print('native_sampler_and_collector_imports_ok=true')
PYIMPORT
exec 8>/run/lock/aries-tb41-placement.lock
flock -n 8
exec 9>"$runtime/.tb41.lock"
flock -n 9
if pgrep -x 'firecracker|[.]firecracker-re|qemu-system-x86'; then exit 1; fi
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
test -z "$("${ctr[@]}" tasks ls -q)"
test -z "$("${ctr[@]}" containers ls -q)"
cp "$runtime/runtime.json" runtime.original.json
cp "$original/bitmap/firecracker" bitmap-firecracker
cp "$original"/{trajectory,rollout} .
cp "$original/zb-trace/tb41/native-deep-scan" zb-trace/tb41/
cat > assets.sha256 <<'SUMS'
89aa86c826d91294ebfd3a546143ed921bb05c71df32db31955778fd79dde211  trajectory
d37d808db61644ae9c8309ad4a31e5e42406142a457a8b77a059f28a45d68991  rollout
fe76e9eb80f6771751ff560e8df581878c66c21b68c7aef73530bba9b21f9911  bitmap-firecracker
cd0fe64edf4449a99b18ab403314dffc451e9c74f405d926191b4ee6684149bc  zb-trace/tb41/native-deep-scan
4f48362409e1bb93fadd07b503502134cd105b8eddab8916bd78a26f7c8cba20  /local/zb-guest-quiet-roots/8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5.ext4
1f5864f69f679f80804022a46f6010a6753266585a5cea8e868d33f01db718fa  /local/zb-v5-pilot-01/runtime/bin/firecracker-containerd
SUMS
sha256sum -c assets.sha256
for arm in control reclaim; do mkdir -m 700 "$arm"; done
collectors=()
bpf=
finish() {
    local rc=$?
    trap - EXIT
    for pid in "${collectors[@]}" $bpf; do kill -TERM "$pid" 2>/dev/null || true; done
    for pid in "${collectors[@]}" $bpf; do wait "$pid" 2>/dev/null || true; done
    cp -p runtime.original.json "$runtime/runtime.json.reclaim-restore"
    mv "$runtime/runtime.json.reclaim-restore" "$runtime/runtime.json"
    systemctl restart "$unit" || rc=1
    cmp "$runtime/runtime.json" runtime.original.json || rc=1
    printf '%s\n' "$rc" > status
    exit "$rc"
}
trap finish EXIT
python3 - "$original/t10-r2/traced/effective-runtime.json" "$runtime/runtime.json" "$stage/bitmap-firecracker" <<'PY'
import json,os,sys
from pathlib import Path
source,path,bitmap=map(Path,sys.argv[1:])
config=json.loads(source.read_bytes())
assert config['guest_reclamation_profile']=='agentenv' and config['smt_enabled'] is False
config['firecracker_binary_path']=str(bitmap.resolve(strict=True))
tmp=path.with_suffix('.reclaim-next')
with tmp.open('x') as stream:
    json.dump(config,stream)
os.replace(tmp,path)
PY
cp "$runtime/runtime.json" effective-runtime.json
systemctl restart "$unit"
for x in {1..80}; do if "${ctr[@]}" version >/dev/null 2>&1; then break; fi; sleep .1; done
"${ctr[@]}" version
export ZB_ROY_RECLAIM_STAGE="$stage" ZB_CHECKPOINT_NAMESPACE_DIR="$runtime/checkpoints/aries" PYTHONDONTWRITEBYTECODE=1
export ZB_TB41_RUNTIME="$runtime" ZB_TB41_UNIT="/run/$unit" ZB_GUEST_PYTHON=/opt/venv/bin/python
bpftrace -q -B line zb-trace/kvm-slots.bt > slots.log 2>slots.stderr &
bpf=$!
for arm in control reclaim; do
    ln slots.log "$arm/slots.log"
    python3 -B zb-trace/tb41/collect.py "$arm" >"$arm/collector.stdout" 2>"$arm/collector.stderr" &
    collectors+=("$!")
done
for x in {1..200}; do
    kill -0 "$bpf" "${collectors[@]}"
    if grep -q '^ready ' slots.log && test -S control/boundary.sock && test -S reclaim/boundary.sock; then break; fi
    sleep .1
done
grep -q '^ready ' slots.log
test -S control/boundary.sock && test -S reclaim/boundary.sock
./roy-reclaim.test -test.v -test.run '^TestRoyReclaimPair$' -test.timeout 20m
