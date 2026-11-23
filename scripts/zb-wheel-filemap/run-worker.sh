#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1")
cd "$stage"
sha256sum -c SHA256SUMS > stage-verification.txt
test "$(cat /var/emulab/boot/nodeid)" = amd201
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
unit=fcmod-zepbound-wheel-pfn-v3
donor=/local/zb-torch-deep-v3/runtime
assets=/local/zb-corpus-torch-cohort-4b58-v3
image=docker.io/alexgshaw/torch-tensor-parallelism@sha256:c50ac169e11465f41b49749fe87e4487f24d31a0c39c0906cd3242cc5499c690
if ps -e -o comm= | grep -Eq '^firecracker$|^[.]firecracker-re$'; then exit 1; fi
mkdir runtime
cp -a --reflink=auto "$donor/bin" "$donor/jailer.json" runtime/
cp tracing.vmlinux runtime/common-vmlinux.bin
cp tracing.config runtime/common-vmlinux.config
gzip -dc diagnostic.ext4.gz | dd of=runtime/common-rootfs.ext4 conv=sparse status=none
chmod 0444 runtime/common-rootfs.ext4
(cd runtime && sha256sum -c ../runtime-source.sha256)
bash provision_baseline.sh "$stage" "$unit" "$donor/runtime.json" v3 "$image"
sudo -n python3 -B audit-idle-runtime.py "$unit" "$stage/runtime" > idle-before.json
set +e
sudo -n python3 -B vmm_exclusivity.py "$stage/placement.jsonl" -- \
    bash "$stage/trace-release.sh" "$stage" "$assets" -- python3 -B "$stage/replay.py" "$stage" "$assets" "$unit"
status=$?
set -e
printf '%s\n' "$status" > driver.exit
sudo -n python3 -B audit-idle-runtime.py "$unit" "$stage/runtime" > idle-after.json
exit "$status"
