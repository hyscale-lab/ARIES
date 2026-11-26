#!/usr/bin/env bash
# Run one pilot from its prepared image in an isolated AgentENV service.
set -euo pipefail
stage=$(realpath "$1")
runtime=$(realpath "$2")
unit=$3
arm=$4
out=$(realpath -m "$5")
shift 5
[[ "$unit" =~ ^fcmod-zepbound-[a-z0-9-]+$ && "$arm" =~ ^(light|traced)$ ]]
trace=$(dirname "$(realpath "$0")")
socket=/run/$unit/containerd.sock
ctr=("$runtime/bin/firecracker-ctr" -a "$socket" -n aries)
mkdir -m700 "$out"
exec 9>"$runtime/.tb41.lock"
flock -n 9
exec 8>/run/lock/aries-tb41-placement.lock
peer=${ZB_TB41_PEER_UNIT:-}
sampler=${ZB_TB41_SAMPLER:-$trace/sample.py}
if [[ -n "$peer" ]]; then
    flock -sn 8
    export ZB_TB41_PLACEMENT=colocated ZB_TB41_PLACEMENT_SAMPLER="$sampler"
    sampler="$trace/placement-sampler"
    python3 -B "$trace/placement.py" "$unit" "$runtime" "$peer" >"$out/placement.json"
else
    flock -n 8
fi
wait_for_service() {
    for _ in {1..100}; do
        "${ctr[@]}" version >/dev/null 2>&1 && return 0
        sleep .1
    done
    "${ctr[@]}" version >/dev/null
}
wait_for_service
namespaces=$("${ctr[@]}" namespaces ls -q)
for ns in $namespaces; do
    tasks=$("$runtime/bin/firecracker-ctr" -a "$socket" -n "$ns" tasks ls -q)
    [[ -z "$tasks" ]]
done
if [[ -z "$peer" ]] && { pgrep -x 'firecracker|[.]firecracker-re|qemu-system-x86' >/dev/null; }; then
    echo "another VM is active on the assigned solo worker" >&2
    exit 1
fi
cp "$runtime/runtime.json" "$out/runtime.before.json"
collector=
slots=
changed=
finish() {
    [[ -z "$collector" ]] || { kill -TERM "$collector" 2>/dev/null || true; wait "$collector" || true; }
    [[ -z "$slots" ]] || { kill -INT "$slots" 2>/dev/null || true; wait "$slots" || true; }
    if [[ -n "$changed" ]]; then
        cp -p "$out/runtime.before.json" "$runtime/runtime.json.tb41-restore"
        mv "$runtime/runtime.json.tb41-restore" "$runtime/runtime.json"
        systemctl restart "$unit"
    fi
}
trap finish EXIT
args=(-periodic-socket observer-light)
export ZB_CHECKPOINT_NAMESPACE_DIR="$runtime/checkpoints/aries"
export PYTHONDONTWRITEBYTECODE=1
export ZB_TB41_RUNTIME="$runtime" ZB_TB41_UNIT="/run/$unit" ZB_CGROUP_EVENTS="$out/events.jsonl"
export ZB_GUEST_PYTHON="${ZB_GUEST_PYTHON:-/opt/venv/bin/python}"
if [[ "$arm" == traced ]]; then
    [[ -x "$trace/native-deep-scan" ]] || { echo "missing executable $trace/native-deep-scan" >&2; exit 1; }
    python3 - "$runtime/runtime.json" "$stage/bitmap/firecracker" <<'PY'
import json, os, shutil, sys
from pathlib import Path
p, binary = map(Path, sys.argv[1:])
cfg = json.loads(p.read_bytes())
assert cfg['guest_reclamation_profile'] == 'agentenv' and cfg['smt_enabled'] is False
cfg['firecracker_binary_path'] = str(binary.resolve(strict=True))
tmp = p.with_suffix('.tb41-next')
with tmp.open('x') as stream:
    json.dump(cfg, stream)
shutil.copymode(p, tmp)
os.replace(tmp, p)
PY
    changed=1
    systemctl restart "$unit"
    wait_for_service
    bpftrace -q -B line "$trace/../kvm-slots.bt" >"$out/slots.log" 2>"$out/slots.stderr" &
    slots=$!
    python3 -B "$trace/collect.py" "$out" >"$out/collector.stdout" 2>"$out/collector.stderr" &
    collector=$!
    for _ in {1..200}; do
        kill -0 "$slots" && kill -0 "$collector" || exit 1
        [[ -S "$out/boundary.sock" ]] && grep -q '^ready ' "$out/slots.log" && break
        sleep .1
    done
    [[ -S "$out/boundary.sock" ]] && grep -q '^ready ' "$out/slots.log"
    args=(-periodic-socket "$out/boundary.sock")
fi
cp "$runtime/runtime.json" "$out/effective-runtime.json"
source_dir=${ZB_SOURCE_DIR:-$stage}
sha256sum "$stage/aries-harbor" "$source_dir"/{trajectory,rollout} "$runtime/bin/firecracker-containerd" "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["firecracker_binary_path"])' "$runtime/runtime.json")" >"$out/provenance.txt"
"$stage/aries-harbor" -trajectory "$source_dir/trajectory" -rollout "$source_dir/rollout" >"$out/source.json"
read -r image cpus mem disk < <(python3 - "$out/source.json" <<'PYREAD'
import json, sys
source = json.load(open(sys.argv[1]))
print(source["image"], source["vcpus"], source["memory_mib"], source["storage_mib"])
PYREAD
)
"$stage/aries-harbor" -native-checkpoint -socket "$socket" -namespace aries -checkpoint-dir "$runtime/checkpoints" -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io" -trajectory "$source_dir/trajectory" -rollout "$source_dir/rollout" -image "$image" -vcpus "$cpus" -memory-mib "$mem" -storage-mib "$disk" -sampler "$sampler" -log "$out/events.jsonl" -samples "$out/samples.jsonl" "${args[@]}" "$@"
