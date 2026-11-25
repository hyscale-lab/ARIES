#!/bin/bash
set -euo pipefail
stage=$1
runtime=/local/zb-v5-matched-ordinary/runtime
unit=fcmod-zepbound-amd198-v5-ordinary
socket=/run/$unit/containerd.sock
collector="$runtime/bin/firecracker-ctr"
image=docker.io/alexgshaw/compile-compcert@sha256:f60236a4fe55bbb3c573210fe0fcd91551f4ee97c417453cd4985be074b35411
exec 9>"$runtime/.tb41.lock"
flock -n 9
idle() {
    local sock namespace
    for sock in /run/fcmod-zepbound-*/containerd.sock; do
        [[ -S "$sock" ]] || continue
        local ns
        ns=$("$collector" -a "$sock" namespaces ls -q)
        for namespace in $ns; do
            [[ -z $("$collector" -a "$sock" -n "$namespace" tasks ls -q) ]] || return 1
        done
    done
    ! ps -eo stat=,comm= | awk '$1 !~ /^Z/ && ($2 == "firecracker" || $2 == "fc_vcpu" || $2 == "jailer") {found=1} END {exit !found}'
}
idle
date -u +%FT%TZ > "$stage/idle-before.txt"
hostname >> "$stage/idle-before.txt"
lscpu | awk -F: '/^Model name:|^Thread\(s\) per core:|^Core\(s\) per socket:/ {gsub(/^ +/,"",$2); print $1 ":" $2}' >> "$stage/idle-before.txt"
"$collector" -a "$socket" -n aries images ls -q | grep -Fx "$image"
python3 - "$runtime/runtime.json" > "$stage/runtime.json" <<'PY'
import hashlib,json,sys
from pathlib import Path
p=Path(sys.argv[1])
r=json.loads(p.read_text())
assert r['guest_reclamation_profile']=='agentenv'
assert r['smt_enabled'] is False
r['vmm_sha256']=hashlib.sha256(Path(r['firecracker_binary_path']).read_bytes()).hexdigest()
r['config_sha256']=hashlib.sha256(p.read_bytes()).hexdigest()
print(json.dumps(r,indent=2))
PY
sha256sum "$stage/clock-fixture" "$runtime/bin/firecracker-containerd" > "$stage/digests.txt"
for arm in light traced; do
    extra=()
    [[ "$arm" != traced ]] || extra+=(-traced)
    "$stage/clock-fixture" -socket "$socket" -checkpoint-dir "$runtime/checkpoints" -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io" -image "$image" "${extra[@]}" > "$stage/$arm.jsonl" 2>"$stage/$arm.stderr"
    idle
done
date -u +%FT%TZ > "$stage/idle-after.txt"
