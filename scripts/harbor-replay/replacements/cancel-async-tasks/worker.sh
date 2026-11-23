#!/usr/bin/env bash
# Replay the sealed replacement cohort serially on the reserved workers.
set -euo pipefail
stage=$(realpath "$1")
variant=$2
kind=$3
node=$(cat /var/emulab/boot/nodeid)
case "$node:$variant:$kind" in
    amd201:v1:ordinary)
        runtime=/local/zb-relaxed-ml-01/stack-v1/runtime
        unit=fcmod-zepbound-relaxed-ml-v1
        ;;
    amd200:v2:ordinary)
        runtime=/local/zb-v2/runtime
        unit=fcmod-zepbound-amd200
        ;;
    amd201:v3:ordinary)
        runtime=/local/zb-texpilot/v3/runtime
        unit=fcmod-zepbound-tex-v3
        ;;
    amd200:v4:ordinary)
        runtime=/local/zb-relaxed-ml-01/stack-v4/runtime
        unit=fcmod-zepbound-relaxed-ml-v4
        ;;
    amd201:v1:deep)
        runtime=$stage/deep-v1/runtime
        unit=fcmod-zepbound-cancel-async-deep-v1
        ;;
    amd200:v4:deep)
        runtime=/local/zb-amd200-torch-deep-v4/runtime
        unit=fcmod-zepbound-amd200-torch-deep-v4
        ;;
    *) exit 2 ;;
esac
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 1; fi
python3 -B - "$stage/source-contract.json" "$kind" <<'PYTHON' > "$stage/$variant-$kind.sources"
import json, sys
with open(sys.argv[1]) as stream:
    cohort = json.load(stream)
sources = sorted(t["trial"] for t in cohort["trials"])
if sys.argv[2] == "deep":
    sources = [cohort["deep_source"]]
print(*sources, sep="\n")
PYTHON
source_stage=/local/zb-relaxed-ml-01
(cd "$source_stage" && sha256sum --quiet -c SHA256SUMS)
if [[ "$variant:$kind" == v1:deep && ! -f "$runtime/runtime.json" ]]; then
    donor="$source_stage/stack-v1/runtime"
    diagnostic=/local/zb-torch-deep-v3/runtime/common-rootfs.ext4
    mkdir -p "$runtime"
    cp -a --reflink=auto "$donor/bin" "$donor/jailer.json" "$donor/common-vmlinux.bin" "$donor/common-vmlinux.config" "$runtime/"
    cp --sparse=always --reflink=auto "$diagnostic" "$runtime/common-rootfs.ext4"
    chmod a-w "$runtime/common-rootfs.ext4"
    bootstrap=docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74
    bash "$source_stage/provision_baseline.sh" "$(dirname "$runtime")" "$unit" "$donor/runtime.json" v1 "$bootstrap"
fi
image=$(python3 -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["image"])' "$stage/image.json")
expected=$(python3 -B -c 'import json,sys; print(json.load(open(sys.argv[1]))["sha256_manifest"])' "$stage/image.json")
socket="/run/$unit/containerd.sock"
ctr=(sudo -n "$runtime/bin/firecracker-ctr" --address "$socket" --namespace aries)
if ! "${ctr[@]}" images ls -q | grep -Fx -- "$image" >/dev/null; then
    "${ctr[@]}" images import --local --platform linux/amd64 --snapshotter devmapper "$stage/task.oci.tar"
fi
"${ctr[@]}" images ls | awk -v name="$image" -v sha="$expected" '$1==name && $3==sha {ok=1} END{exit !ok}'
common_root=292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f
deep_env=()
if [[ "$kind" == deep ]]; then
    common_root=8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5
    deep_env=(ZB_GUEST_VSOCK=1 ZB_CONTENT_SAMPLE=8192)
fi
echo "$common_root  $runtime/common-rootfs.ext4" | sudo -n sha256sum --quiet -c -
sudo -n python3 -B - "$runtime/runtime.json" "$variant" <<'PYTHON' > "$stage/$variant-$kind.runtime.json"
import json, sys
with open(sys.argv[1]) as stream:
    config = json.load(stream)
variant = sys.argv[2]
assert config["guest_root_mode"] == "disk" and config["smt_enabled"] is False
assert bool(config.get("root_drive_use_pmem")) == (variant == "v4")
assert bool(config.get("guest_reclamation", {}).get("Enabled")) == (variant in {"v3", "v4"})
assert bool(config.get("free_page_reporting")) == (variant == "v2")
print(json.dumps(config, indent=2))
PYTHON
checkpoint=()
native=()
if [[ "$variant" == v3 || "$variant" == v4 ]]; then
    native=(-native-checkpoint)
fi
if [[ "$kind" == deep || "$variant" == v1 || "$variant" == v2 ]]; then
    checkpoint=("ZB_CHECKPOINT_NAMESPACE_DIR=$runtime/checkpoints/aries")
fi
assets=/local/zb-corpus-torch-cohort-4b58-v3
if [[ $node == amd200 ]]; then assets=/local/zb-corpus-torch-cohort-4b58-v2; fi
codex="$assets/codex-harbor-tools.tar.gz"
echo "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab  $codex" | sha256sum --quiet -c -
mkdir -p "$stage/runs"
while IFS= read -r trial; do
    run="$stage/runs/${trial:0:8}-$variant-cancel-async-$kind-01"
    mkdir "$run"
    source="$stage/sources/$trial"
    sudo -n python3 -B "$source_stage/audit-idle-runtime.py" "$unit" "$runtime" > "$run/deployment-before.json"
    sha256sum "$codex" "$stage/aries-cancel-async-tasks" "$stage/cancel-async-debs.tar.gz" "$stage/task.oci.tar" > "$run/shared-inputs.sha256"
    status=0
    sudo -n env ZB_VMM_CENSUS=1 BPFTRACE_PERF_RB_PAGES=1024 "${checkpoint[@]}" "${deep_env[@]}" \
        python3 -B "$source_stage/vmm_exclusivity.py" "$run/placement.jsonl" -- \
        bash "$stage/tracer/run-traced.sh" "$stage/aries-cancel-async-tasks" "$stage/tracer" "$run/raw" \
        -guest-task-v3 "${native[@]}" -trajectory "$source/trajectory.json" -rollout "$source/rollout.jsonl" \
        -image "$image" -namespace aries -socket "$socket" -checkpoint-dir "$runtime/checkpoints" \
        -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io" -codex-tools "$codex" \
        -os-tools "$stage/cancel-async-debs.tar.gz" -vcpus 1 -memory-mib 2048 -storage-mib 10240 \
        > "$run/driver.log" 2>&1 || status=$?
    printf '%s\n' "$status" > "$run/driver.exit"
    sudo -n python3 -B "$source_stage/audit-idle-runtime.py" "$unit" "$runtime" > "$run/deployment-after.json"
done < "$stage/$variant-$kind.sources"
