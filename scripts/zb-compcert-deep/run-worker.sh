#!/usr/bin/env bash
# Capture the strict CompCert source through completion or its first divergent reply.
set -euo pipefail
cd "$(dirname "$0")"
stage=$PWD
node=$(cat /var/emulab/boot/nodeid)
case "$node" in
    amd182) variant=v1; host=/local/zb-torch-deep-v1-amd182; unit=fcmod-zepbound-tdeep182v1
        plugins=/local/zb-deep-kv-v1-amd182/cni; trace=/local/zb-deep-kv-v1-amd182/trace
        tools=/local/zb-torch-deep-v1-amd182/assets/codex-harbor-tools.tar.gz; mode=() ;;
    amd189) variant=v4; host=/local/zb-video-deep-v4-amd189; unit=fcmod-zepbound-amd189-video-deep
        plugins=/local/zb-compcert-network/opam-cni; trace=$host/tracer
        tools=/local/zb-compcert-network/replay/codex-harbor-tools.tar.gz; mode=(-native-checkpoint -guest-task-v3) ;;
    *) exit 2 ;;
esac
rt=$host/runtime
image=docker.io/alexgshaw/compile-compcert@sha256:f60236a4fe55bbb3c573210fe0fcd91551f4ee97c417453cd4985be074b35411
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
test ! -e memory
sha256sum -c stage.sha256
echo "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab  $tools" | sha256sum -c -
echo "8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5  $rt/common-rootfs.ext4" | sha256sum -c -
python3 -B "$host/audit-idle-runtime.py" "$unit" "$rt" > before.json
cp "$rt/runtime.json" profile.json
ctr=("$rt/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries)
if ! "${ctr[@]}" images ls -q | grep -Fxq "$image"; then
    "${ctr[@]}" images pull --platform linux/amd64 "$image" > image-pull.log 2>&1
fi
mkdir -p cni/bin
cp -a "$plugins/bin/." cni/bin/
cp "$plugins/SHA256SUMS" cni/
test ! -e /sys/class/net/zbccsnap183
if systemctl is-active --quiet zb-ccsnap-dns-amd183 || iptables -t mangle -S ZB183CCSNAP >/dev/null 2>&1; then exit 2; fi
trap 'bash provision-pypi-cni.sh "$stage/cni" remove compiler-snapshot-amd183' EXIT
ZB_TLS_CANDIDATES="$stage/hosts.json" bash provision-pypi-cni.sh "$stage/cni" install compiler-snapshot-amd183 > network.log
export ZB_VMM_CENSUS=1 ZB_GUEST_VSOCK=1 ZB_CONTENT_SAMPLE=8192 ZB_CHECKPOINT_NAMESPACE_DIR="$rt/checkpoints/aries"
rc=0
python3 -B "$host/vmm_exclusivity.py" placement.jsonl -- "$trace/run-traced.sh" "$stage/aries-normal-launchclock-v3" "$trace" "$stage/memory" \
    -trajectory "$stage/trajectory.json" -rollout "$stage/rollout.jsonl" -namespace aries -socket "/run/$unit/containerd.sock" \
    -checkpoint-dir "$rt/checkpoints" -shim-dir "$rt/shim" -io-dir "/run/$unit/aries-io" -image "$image" "${mode[@]}" \
    -codex-tools "$tools" -os-tools "$stage/compile-compcert-ca.tar.gz" -cni-network zbccsnap183 \
    -cni-bin-dir "$stage/cni/bin" -cni-conf-dir "$stage/cni/conf" -cni-cache-dir "$stage/cni/cache" || rc=$?
echo "$rc" > driver.exit
python3 -B "$host/audit-idle-runtime.py" "$unit" "$rt" > after.json
exit "$rc"
