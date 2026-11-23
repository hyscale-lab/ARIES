#!/usr/bin/env bash
# Run each source-bound arm on its reserved, idle worker.
set -euo pipefail
stage=$(realpath "$1")
node=$(cat /var/emulab/boot/nodeid)
variant=$2
kind=$3
case "$node:$variant:$kind" in
    amd183:v1:ordinary)
        runtime=/local/zb-validation/runtime
        unit=fcmod-zepbound-amd183
        ;;
    amd183:v2:ordinary)
        runtime=/local/zb-stock183-v2/runtime
        unit=fcmod-zepbound-amd183-stockv2
        ;;
    amd183:v1:deep)
        runtime=/local/zb-deep-vi-v1-amd183/runtime
        unit=fcmod-zepbound-deep183vi
        ;;
    amd198:v3:ordinary)
        runtime=/local/zb-root-backing-control/v3/runtime
        unit=fcmod-zepbound-amd198-root-v3
        ;;
    amd198:v4:ordinary)
        runtime=/local/zb-imagecircuit-v4/runtime
        unit=fcmod-zepbound-amd198-image-circuit
        ;;
    amd198:v4:deep)
        runtime=/local/zb-amd198-deep-v4/circuit/runtime
        unit=fcmod-zepbound-amd198-deep-circuit
        ;;
    *) exit 2 ;;
esac
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
test "$(sudo -n systemctl show -p ActiveState --value zb-timing17-hf.service)" = inactive
timing_arm=v2
if [[ $node == amd198 ]]; then timing_arm=v4; fi
sudo -n test -s "/tmp/zb-timing17-01/b615a3ec-$timing_arm/driver.exit"
test "$(sudo -n systemctl show -p ActiveState --value "$unit")" = active
arm=93c2fb63-$variant-protein-assembly-$kind-01
run=$stage/runs/$arm
mkdir -p "$stage/runs"
mkdir "$run"
common_root=292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f
deep=()
if [[ $kind == deep ]]; then
    common_root=8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5
    deep=(ZB_GUEST_VSOCK=1 ZB_CONTENT_SAMPLE=8192)
fi
echo "$common_root  $runtime/common-rootfs.ext4" | sudo -n sha256sum --quiet -c -
sudo -n python3 -B - "$runtime/runtime.json" "$variant" <<'PY' > "$run/variant-profile.json"
import json, sys
with open(sys.argv[1]) as stream:
    config = json.load(stream)
variant = sys.argv[2]
assert config["guest_root_mode"] == "disk" and config["smt_enabled"] is False
assert bool(config.get("root_drive_use_pmem")) == (variant == "v4")
assert bool(config.get("guest_reclamation", {}).get("Enabled")) == (variant in {"v3", "v4"})
assert bool(config.get("free_page_reporting")) == (variant == "v2")
print(json.dumps(config, indent=2))
PY
image=$(python3 -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["image"])' "$stage/image.json")
expected=$(python3 -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["sha256_manifest"])' "$stage/image.json")
socket=/run/$unit/containerd.sock
ctr=(sudo -n "$runtime/bin/firecracker-ctr" --address "$socket" --namespace aries)
if ! "${ctr[@]}" images ls -q | grep -Fx -- "$image" >/dev/null; then
    "${ctr[@]}" images import --local --platform linux/amd64 --snapshotter devmapper "$stage/task.oci.tar" > "$run/image-import.log" 2>&1
fi
"${ctr[@]}" images ls | awk -v name="$image" -v sha="$expected" '$1==name && $3==sha {good=1} END{exit !good}'
cni=$stage/cni
test -f "$cni/conf/10-protein.conflist"
sha256sum "$cni/hosts" "$cni/conf/10-protein.conflist" > "$run/network.sha256"
sudo -n python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" > "$run/deployment-before.json"
native=(-native-checkpoint)
checkpoint=()
if [[ $variant == v1 || $variant == v2 ]]; then native=(); fi
if [[ ${#native[@]} == 0 || $kind == deep ]]; then
    checkpoint=("ZB_CHECKPOINT_NAMESPACE_DIR=$runtime/checkpoints/aries")
fi
status=0
sudo -n env ZB_VMM_CENSUS=1 BPFTRACE_PERF_RB_PAGES=1024 "${checkpoint[@]}" "${deep[@]}" \
    python3 -B "$stage/vmm_exclusivity.py" "$run/placement.jsonl" -- \
    bash "$stage/tracer/run-traced.sh" "$stage/aries-protein-assembly" "$stage/tracer" "$run/raw" \
      -guest-task-v3 "${native[@]}" -trajectory "$stage/source/trajectory.json" -rollout "$stage/source/rollout.jsonl" \
      -image "$image" -namespace aries -socket "$socket" -checkpoint-dir "$runtime/checkpoints" \
      -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io" -codex-tools "$stage/codex-harbor-tools.tar.gz" \
      -os-tools "$stage/task-debs.tar.gz" -vcpus 1 -memory-mib 4096 -storage-mib 10240 \
      -cni-network "zbprot${node#amd}" -cni-conf-dir "$cni/conf" -cni-bin-dir "$cni/bin" -cni-cache-dir "$cni/cache" \
    > "$run/driver.log" 2>&1 || status=$?
printf '%s\n' "$status" > "$run/driver.exit"
sudo -n python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" > "$run/deployment-after.json"
