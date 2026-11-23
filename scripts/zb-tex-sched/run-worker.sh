#!/usr/bin/env bash
# Run on the exclusively reserved study worker with untouched amd200 input stack.
set -euo pipefail
stage=$(realpath "$1")
cd "$stage"
sha256sum -c SHA256SUMS > stage-verification.txt
test "$(cat /var/emulab/boot/nodeid)" = amd200
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
unit=fcmod-zepbound-tex-sched-v4
donor=/local/zb-filemaps-v4/runtime
assets=/local/zb-v2/tex-pilot
image=docker.io/alexgshaw/overfull-hbox@sha256:7dca952bb6736194a736b5110945f48842eafa8551e24468f1dddeed0daee799
if ps -e -o comm= | grep -Eq '^firecracker$|^[.]firecracker-re$'; then exit 1; fi
mkdir runtime
cp -a --reflink=auto "$donor/bin" "$donor/jailer.json" runtime/
cp tracing.vmlinux runtime/common-vmlinux.bin
cp tracing.config runtime/common-vmlinux.config
gzip -dc diagnostic.ext4.gz | dd of=runtime/common-rootfs.ext4 conv=sparse status=none
chmod 0444 runtime/common-rootfs.ext4
test "$(sha256sum runtime/common-rootfs.ext4 | cut -d' ' -f1)" = "$(awk '$2 == "diagnostic.ext4" {print $1}' root.sha256)"
sha256sum runtime/bin/* runtime/common-vmlinux.bin runtime/common-rootfs.ext4 > runtime.sha256
bash provision_baseline.sh "$stage" "$unit" "$donor/runtime.json" v4 "$image"
sha256sum "$assets"/{trajectory.json,rollout.jsonl,codex-harbor-tools.tar.gz,overfull-hbox-debs.tar.gz} > source-assets.sha256
sudo -n python3 -B audit-idle-runtime.py "$unit" "$stage/runtime" > idle-before.json
set +e
sudo -n python3 -B vmm_exclusivity.py "$stage/placement.jsonl" -- \
    bash "$stage/trace-host.sh" "$stage" "$assets" "$unit" "$image"
status=$?
set -e
printf '%s\n' "$status" > driver.exit
sudo -n python3 -B audit-idle-runtime.py "$unit" "$stage/runtime" > idle-after.json
exit "$status"
