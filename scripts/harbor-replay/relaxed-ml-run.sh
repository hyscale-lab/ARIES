#!/usr/bin/env bash
# Run the four reviewed ML recordings with real process outcomes and ordinary tracing.
set -euo pipefail
stage=$(realpath "$1")
variant=$2
attempt=${ZB_RELAXED_ML_ATTEMPT:-01}
adapter="$stage/aries-relaxed-ml"
if [[ $attempt != 01 ]]; then
    [[ "$attempt:$variant:${3:-}:$#" == "02:v2:e047ddea:3" ||
       "$attempt:$variant:${3:-}:$#" == "02:v4:e047ddea:3" ]]
    adapter="$stage/aries-relaxed-ml.retry"
    (cd "$stage" && sha256sum --quiet -c retry-SHA256SUMS)
fi
node=$(cat /var/emulab/boot/nodeid)
case "$node:$variant" in
    amd200:v2) runtime=/local/zb-v2/runtime; unit=fcmod-zepbound-amd200 ;;
    amd201:v3) runtime=/local/zb-texpilot/v3/runtime; unit=fcmod-zepbound-tex-v3 ;;
    amd201:v4)
        [[ $attempt == 02 ]]
        runtime=/local/zb-texpilot/v4/runtime
        unit=fcmod-zepbound-tex-v4
        ;;
    amd200:v4|amd201:v1)
        runtime=$stage/stack-$variant/runtime
        unit=fcmod-zepbound-relaxed-ml-$variant
        ;;
    *) exit 2 ;;
esac
source_variant=$([[ $node == amd200 ]] && echo v2 || echo v3)
torch=/local/zb-corpus-torch-cohort-4b58-$source_variant
dataset=/local/zb-corpus-dataset-cohort-02-$source_variant
hf=/local/zb-corpus-hf-cohort-02-$source_variant
cni_hf="$hf/cni"
if [[ $node == amd200 ]]; then
    hf=/local/zb-corpus-hf-cohort-01-v2
    cni_hf="$hf/cni"
fi
torch_image=docker.io/alexgshaw/torch-tensor-parallelism@sha256:c50ac169e11465f41b49749fe87e4487f24d31a0c39c0906cd3242cc5499c690
dataset_image=docker.io/alexgshaw/count-dataset-tokens@sha256:9c513c4ba342bd95be501f6f5eadd4806a2b18749c87c251615825100130232d
hf_image=docker.io/alexgshaw/hf-model-inference@sha256:0ef475aebda8abb8cbf3a1d97aee8ac4b09ca9720550e25a4a46a47930cd3bad
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 1; fi
if [[ ! -f "$runtime/runtime.json" ]]; then
    provision=${ZB_PROVISION_BASELINE:-$(dirname "$(realpath "$0")")/../zb-trace/provision_baseline.sh}
    if [[ ! -f "$provision" && -z "${ZB_PROVISION_BASELINE:-}" ]]; then
        provision="$stage/provision_baseline-quiet.sh"
    fi
    provision=$(realpath -e "$provision")
    case $variant in
        v1)
            donor=/local/zb-torch-gloo-e047-01/runtime
            common="$donor"
            ;;
        v4)
            donor=/local/zb-filemaps-v4/runtime
            common=/local/zb-v2/runtime
            ;;
    esac
    mkdir -p "$runtime"
    cp --reflink=auto -a "$donor/bin" "$donor/jailer.json" "$common/common-vmlinux.bin" \
        "$common/common-vmlinux.config" "$common/common-rootfs.ext4" "$runtime/"
    chmod a-w "$runtime/common-rootfs.ext4"
    bash "$provision" "$(dirname "$runtime")" "$unit" "$donor/runtime.json" "$variant" "$torch_image"
fi
(cd "$runtime"; sha256sum --quiet -c <<'HASHES'
292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f  common-rootfs.ext4
934a078f4b36ba6cfadcc62df0edfbe4b709e6545aa6220f02993eae05267856  common-vmlinux.bin
HASHES
)
if [[ "$variant" =~ ^v[34]$ ]]; then
    native=(-native-checkpoint)
    checkpoint_env=()
else
    native=()
    checkpoint_env=("ZB_CHECKPOINT_NAMESPACE_DIR=$runtime/checkpoints/aries")
fi
socket=/run/$unit/containerd.sock
ctr=(sudo -n "$runtime/bin/firecracker-ctr" --address "$socket" --namespace aries)
mkdir -p "$stage/runs"
shift 2
for task in "$@"; do
    trace=()
    case "$task" in
        e047ddea|ce62b3a5)
            image=$torch_image
            cni="$torch/cni"
            name=zbtorch${node#amd}
            tools="$torch/torch-tensor-parallelism-debs.tar.gz"
            memory=8192
            ;;
        eaa24caa)
            image=$dataset_image
            cni="$dataset/cni"
            name=zbtoken${node#amd}
            tools="$stage/kv-python.tar.gz"
            trace=(-curl-packages "$dataset/curl-debs.tar.gz")
            memory=2048
            ;;
        f0d5e4b5)
            image=$hf_image
            cni="$cni_hf"
            name=zbhfm${node#amd}
            tools="$hf/hf-environment.tar.gz"
            trace=(-hf-harness "$stage/hf-harness")
            memory=2048
            ;;
        *) exit 2 ;;
    esac
    run=$stage/runs/$task-$variant-relaxed-ml-$attempt
    mkdir "$run"
    if ! "${ctr[@]}" images ls -q | grep -Fx -- "$image" >/dev/null; then
        "${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$image" > "$run/image-pull.log"
    fi
    sha256sum "$tools" "$torch/codex-harbor-tools.tar.gz" "$cni"/conf/*.conflist > "$run/assets.sha256"
    sha256sum "$adapter" > "$run/executed-adapter.sha256"
    if [[ $attempt == 02 ]]; then
        cp "$runtime/runtime.json" "$run/runtime-config.json"
    fi
    systemctl show "$unit" -p EffectiveCPUs -p AllowedCPUs -p CPUQuotaPerSecUSec > "$run/host-capacity.txt"
    sudo -n python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" > "$run/deployment-before.json"
    args=(-relaxed-ml -guest-task-v3 "${native[@]}" -trajectory "$stage/sources/$task/trajectory.json"
          -rollout "$stage/sources/$task/rollout.jsonl" -image "$image" -namespace aries -socket "$socket"
          -checkpoint-dir "$runtime/checkpoints" -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io"
          -codex-tools "$torch/codex-harbor-tools.tar.gz" -os-tools "$tools" -vcpus 1 -memory-mib "$memory"
          -storage-mib 10240 -cni-network "$name" -cni-conf-dir "$cni/conf" -cni-cache-dir "$cni/cache"
          -cni-bin-dir "$cni/bin" "${trace[@]}")
    printf '%q ' "${args[@]}" > "$run/argv.shell"
    printf '\n' >> "$run/argv.shell"
    rc=0
    sudo -n env ZB_VMM_CENSUS=1 BPFTRACE_PERF_RB_PAGES=1024 "${checkpoint_env[@]}" \
        python3 -B "$stage/vmm_exclusivity.py" "$run/placement.jsonl" -- \
        bash "$stage/tracer/run-traced.sh" "$adapter" "$stage/tracer" "$run/raw" "${args[@]}" \
        > "$run/driver.log" 2>&1 || rc=$?
    printf '%s\n' "$rc" > "$run/driver.exit"
    sudo -n python3 -B "$stage/audit-idle-runtime.py" "$unit" "$runtime" > "$run/deployment-after.json"
done
