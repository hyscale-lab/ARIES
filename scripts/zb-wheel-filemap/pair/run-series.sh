#!/usr/bin/env bash
set -euo pipefail
d=$(realpath "$1")
cd "$d"
sha256sum --quiet -c SHA256SUMS
test "$(cat /var/emulab/boot/nodeid)" = amd201
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
if ps -e -o comm= | grep -Eq '^firecracker$|^[.]firecracker-re$'; then exit 1; fi
unit=fcmod-zepbound-wheel-pair-v3
donor=/local/zb-torch-deep-v3/runtime
assets=/local/zb-corpus-torch-cohort-4b58-v3
image=docker.io/alexgshaw/torch-tensor-parallelism@sha256:c50ac169e11465f41b49749fe87e4487f24d31a0c39c0906cd3242cc5499c690
mkdir runtime periods
cp -a --reflink=auto "$donor/bin" "$donor/jailer.json" runtime/
cp tracing.vmlinux runtime/common-vmlinux.bin
cp tracing.config runtime/common-vmlinux.config
gzip -dc diagnostic.ext4.gz | dd of=runtime/common-rootfs.ext4 conv=sparse status=none
chmod 0444 runtime/common-rootfs.ext4
(cd runtime && sha256sum -c ../runtime-source.sha256)
bash provision_baseline.sh "$d" "$unit" "$donor/runtime.json" v3 "$image"
mapfile -t arms < <(python3 -c 'import json; p=json.load(open("protocol.json")); assert p["order"]==["treated","sham","sham","treated"]; print(*p["order"],sep="\n")')
test "${#arms[@]}" = 4
for ix in 0 1 2 3; do
    arm=${arms[$ix]}
    n=$((ix+1))
    p="$d/periods/$n-$arm"
    mkdir -p "$p/presence"
    for name in guest_sample.py watch-report-replacements.py reported-replacements.bt; do ln -s "$d/$name" "$p/$name"; done
    sudo -n python3 -B audit-idle-runtime.py "$unit" "$d/runtime" > "$p/idle-before.json"
    set +e
    sudo -n env "ZB_PAIR_ARM=$arm" python3 -B "$d/vmm_exclusivity.py" "$p/placement.jsonl" -- \
      bash "$d/trace-release.sh" "$p" "$assets" -- python3 -B "$d/run-source.py" "$p" "$assets" "$unit" > "$p/driver.log" 2>&1
    rc=$?
    set -e
    printf '%s\n' "$rc" > "$p/driver.exit"
    sudo -n python3 -B audit-idle-runtime.py "$unit" "$d/runtime" > "$p/idle-after.json"
    python3 - "$p/idle-after.json" <<'PY'
import json,sys
r=json.load(open(sys.argv[1]))
assert all(not r[s]['vmm_processes'] and not any(r[s]['tasks_by_socket'].values()) for s in ('before','after'))
PY
    printf '%s %s %s\n' "$n" "$arm" "$rc" >> series-status.txt
done
