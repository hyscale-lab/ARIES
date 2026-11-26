#!/bin/bash
set -euo pipefail
stage=/local/zb-nextjs-three-01
runtime=/local/zb-nextjs-be1d1d11/runtime
unit=fcmod-zepbound-nextjs191
test "$(cat /var/emulab/boot/nodeid)" = amd191
cd "$stage"
sha256sum --check SHA256SUMS
exec 8>/run/lock/aries-tb41-placement.lock
flock -n 8
exec 9>"$runtime/.tb41.lock"
flock -n 9
if pgrep -x 'firecracker|[.]firecracker-re|qemu-system-x86'; then exit 1; fi
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
cp "$runtime/runtime.json" runtime.before.json
python3 - <<'PY'
import json
p=json.load(open("runtime.before.json"))
assert not p.get("default_network_interfaces")
assert p["root_drive_sha256"]=="4f48362409e1bb93fadd07b503502134cd105b8eddab8916bd78a26f7c8cba20"
PY
finish() {
    rc=$?
    trap - EXIT
    cmp runtime.before.json "$runtime/runtime.json" || rc=1
    if pgrep -x 'firecracker|[.]firecracker-re|qemu-system-x86'; then rc=1; fi
    if ! test -z "$("${ctr[@]}" tasks ls -q)" || ! test -z "$("${ctr[@]}" containers ls -q)"; then rc=1; fi
    printf '%s\n' "$rc" > status
    exit "$rc"
}
trap finish EXIT
test -z "$("${ctr[@]}" tasks ls -q)"
test -z "$("${ctr[@]}" containers ls -q)"
printf '%s\n' "node=amd191" "smt_active=$(cat /sys/devices/system/cpu/smt/active)" > preflight.txt
sha256sum "$runtime/bin/firecracker" "$runtime/bin/firecracker-containerd" >> preflight.txt
export ZB_NEXTJS_THREE_STAGE="$stage"
./nextjs-three.test -test.v -test.run '^TestNativeNextJSThreeTasks$' -test.timeout 9m > test.log 2>&1
