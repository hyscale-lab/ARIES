#!/usr/bin/env bash
set -euo pipefail
stage=/local/zb-claims-shm-20261126
runtime=/local/zb-v5-matched-deep/runtime
unit=/run/fcmod-zepbound-amd183-v5-deep
ns=aries-claims-shm-20261126
cd "$stage"
sha256sum -c execution.sha256
test "$(cat /var/emulab/boot/nodeid)" = amd183
exec 8>/run/lock/aries-tb41-placement.lock
flock -n 8
exec 9>"$runtime/.tb41.lock"
flock -n 9
if pgrep -x 'firecracker|[.]firecracker-re|qemu-system-x86'; then exit 1; fi
base=("$runtime/bin/firecracker-ctr" -a "$unit/containerd.sock")
ctr=("${base[@]}" -n "$ns")
if "${base[@]}" namespaces ls -q | grep -Fx "$ns"; then exit 1; fi
cp "$runtime/runtime.json" runtime.before.json
python3 - <<'PY'
import json
p = json.load(open('runtime.before.json'))
assert not p.get('default_network_interfaces')
assert p['root_drive_sha256']=='4f48362409e1bb93fadd07b503502134cd105b8eddab8916bd78a26f7c8cba20'
PY
finish() {
    rc=$?
    trap - EXIT
    cmp runtime.before.json "$runtime/runtime.json" || rc=1
    if pgrep -x 'firecracker|[.]firecracker-re|qemu-system-x86'; then rc=1; fi
    if [[ -z "$("${ctr[@]}" tasks ls -q)" && -z "$("${ctr[@]}" containers ls -q)" ]]; then
        while IFS= read -r ref; do
            "${ctr[@]}" images rm --sync "$ref" || rc=1
        done < <("${ctr[@]}" images ls -q)
        "${base[@]}" namespaces rm "$ns" || rc=1
        if [[ -d "$runtime/checkpoints/$ns" ]]; then rmdir "$runtime/checkpoints/$ns" || rc=1; fi
    else
        rc=1
    fi
    printf '%s\n' "$rc" > status
    exit "$rc"
}
trap finish EXIT
while IFS=$'\t' read -r role pin; do
    "${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$pin" > "pull-$role.log" 2>&1
    "${ctr[@]}" images tag "$pin" "${pin%:*@*}@${pin##*@}"
    test "$("${ctr[@]}" content get "${pin##*@}" | sha256sum | cut -d' ' -f1)" = "${pin##*sha256:}"
done < images.tsv
"${ctr[@]}" images check --snapshotter devmapper > image-check.txt
sha256sum "$runtime/bin/firecracker" "$runtime/bin/firecracker-containerd" > binary-hashes.txt
export ZB_CLAIMS_SHM_STAGE="$stage"
./claims-shm.test -test.v -test.run '^TestClaimsShm$' -test.timeout 7m > test.log 2>&1
