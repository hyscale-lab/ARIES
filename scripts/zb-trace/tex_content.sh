#!/usr/bin/env bash
# Sample original TeX trial 337f0b12 against its private snapshot on a diagnostic root.
set -euo pipefail
assets=$(realpath "$1")
runtime=$(realpath "$2")
unit=$3
tracer=$(realpath "$4")
output=$(realpath -m "$5")
[[ $unit == fcmod-zepbound-filemapsv1 || $unit == fcmod-zepbound-filemapsv4 ]]
[[ ! -e $output ]]
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then
    echo 'Another VMM is active on this worker' >&2
    exit 1
fi
image=docker.io/alexgshaw/overfull-hbox@sha256:7dca952bb6736194a736b5110945f48842eafa8551e24468f1dddeed0daee799
sock=/run/$unit/containerd.sock
(cd "$assets" && sha256sum -c - <<'CHECK'
e8ae973a7a954dce0bc3bb300bbd81e2093b307739ff4e3327f4b78977449add  aries-tex
1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab  codex-harbor-tools.tar.gz
a63ab01c6a16caeec664ffbc10890ce597e05948b0a378434f5c48b9266a8f9a  overfull-hbox-debs.tar.gz
CHECK
)
"$runtime/bin/firecracker-ctr" -a "$sock" -n aries images pull --local --platform linux/amd64 --snapshotter devmapper "$image" >/dev/null
export ZB_VMM_CENSUS=1 ZB_GUEST_VSOCK=1 ZB_CONTENT_SAMPLE=8192
export ZB_CHECKPOINT_NAMESPACE_DIR="$runtime/checkpoints/aries"
native=()
if [[ $unit == fcmod-zepbound-filemapsv4 ]]; then native=(-native-checkpoint); fi
rc=0
"$tracer/run-traced.sh" "$assets/aries-tex" "$tracer" "$output" -trajectory "$assets/trajectory.json" -rollout "$assets/rollout.jsonl" \
    -image "$image" -namespace aries -socket "$sock" -checkpoint-dir "$runtime/checkpoints" -shim-dir "$runtime/shim" \
    -io-dir "/run/$unit/aries-io" -codex-tools "$assets/codex-harbor-tools.tar.gz" -os-tools "$assets/overfull-hbox-debs.tar.gz" "${native[@]}" || rc=$?
echo "$rc" > "$output/driver.exit"
exit "$rc"
