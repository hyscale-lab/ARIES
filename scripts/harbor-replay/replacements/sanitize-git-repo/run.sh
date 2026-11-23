#!/bin/bash
set -eu
cd "$(dirname "$0")"
stage=$PWD
node=$1
median=6d0403e0-740e-4104-a684-619cfc8f85bf
image=docker.io/alexgshaw/sanitize-git-repo@sha256:4b5234da5bb0d67f3b0bf8db40a2883c07a5219f62b64c2bf9ff1ac84cd0f672
wait_for() {
    while systemctl is-active --quiet "$1"; do sleep 10; done
}
case "$node" in
203)
    wait_for zb-resume-deep-01
    bundle=/local/zb-corpus-pypi-cohort-v1
    existing_net=/tmp/zb-resume-extra-01/marker-cni
    export PATH=/users/nehalem/zepbound-demand-amd203/tools/bpf/usr/bin:$PATH
    export LD_LIBRARY_PATH=/users/nehalem/zepbound-demand-amd203/tools/bpf/usr/lib/x86_64-linux-gnu
    variants=(v1 deep-v1);;
191)
    wait_for zb-191-ocr-git-recovery
    bundle=/local/zb-fc-v4/pilot/pypi-cohort-amd191
    existing_net=/tmp/zb-resume-extra-01/marker-cni
    variants=(v4 v3 deep-v4);;
200)
    wait_for zb-relaxed-ml-e047-v2-retry-02
    bundle=/local/zb-corpus-financeapt-cohort-v2
    existing_net=$bundle/cni
    variants=(v2);;
*) exit 2;;
esac
mkdir -p net results
cp -an "$existing_net/bin" "$existing_net/SHA256SUMS" net/
bash network.sh "$stage/net" install "amd$node" cleanse >network-receipt.log
for mode in "${variants[@]}"; do
    tracer=$bundle/tracer
    native=()
    case "$mode" in
    v1) unit=fcmod-zepbound-demand-amd203; runtime=/users/nehalem/zepbound-demand-amd203/runtime;;
    v2) unit=fcmod-zepbound-amd200; runtime=/local/zb-v2/runtime;;
    v3|v4) unit=fcmod-zepbound-amd191-$mode; runtime=/local/zb-fc-v4/$mode/runtime; native=(-native-checkpoint);;
    deep-v1) unit=fcmod-zepbound-deep203cf; runtime=/local/zb-deep-cf-v1-amd203/runtime; tracer=/local/zb-deep-hf-v1-amd203/trace;;
    deep-v4) unit=fcmod-zepbound-amd191-expdeep; runtime=/local/zb-expdeep-runtime-amd191/runtime; tracer=/local/zb-grpc69ba-v4/cohort/tracer; native=(-native-checkpoint);;
    esac
    export ZB_VMM_CENSUS=1 ZB_CHECKPOINT_NAMESPACE_DIR=$runtime/checkpoints/aries
    sources=("$stage/sources/$median/trajectory.json")
    if [[ "$mode" == deep-* ]]; then
        export ZB_GUEST_VSOCK=1 ZB_CONTENT_SAMPLE=8192
    else
        for file in "$stage"/sources/*/trajectory.json; do
            [[ "$file" == "${sources[0]}" ]] || sources+=("$file")
        done
    fi
    sock=/run/$unit/containerd.sock
    "$runtime/bin/firecracker-ctr" -a "$sock" -n aries images pull --local --platform linux/amd64 --snapshotter devmapper "$image" >"image-$mode.log" 2>&1
    for file in "${sources[@]}"; do
        src=$(dirname "$file"); id=$(basename "$src")
        out=$stage/results/${id:0:8}-$mode
        [[ ! -e "$out" ]] || continue
        helper=()
        [[ "$id" != "$median" ]] || helper=(-image-helper "$stage/aries-harbor-image")
        cp "$runtime/runtime.json" "$out-runtime.json"
        rc=0
        python3 -B vmm_exclusivity.py "$out-placement.jsonl" -- "$tracer/run-traced.sh" "$stage/aries" "$tracer" "$out" -trajectory "$file" -rollout "$src/rollout.jsonl" -namespace aries -socket "$sock" -checkpoint-dir "$runtime/checkpoints" -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io" "${native[@]}" -image "$image" -codex-tools "$bundle/codex-harbor-tools.tar.gz" -curl-packages "$stage/curl-debs.tar.gz" "${helper[@]}" -cni-network "zbcleanse$node" -cni-conf-dir "$stage/net/conf" -cni-cache-dir "$stage/net/cache" -cni-bin-dir "$stage/net/bin" >"$out.log" 2>&1 || rc=$?
        mkdir -p "$out"
        echo "$rc" > "$out/driver.exit"
    done
done
