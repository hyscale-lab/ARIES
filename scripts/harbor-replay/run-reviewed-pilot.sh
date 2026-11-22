#!/usr/bin/env bash
# Run a reviewed TeX recording on an already provisioned study stack.
set -euo pipefail
work=$(realpath "$1")
unit=$2
asset=$(realpath "$3")
variant=$4
name=$5
[[ "$variant" =~ ^v[1-4]$ && "$name" =~ ^[A-Za-z0-9-]+$ && "$unit" =~ ^fcmod-zepbound-[a-z0-9-]+$ ]]
sock="/run/$unit/containerd.sock"
image=docker.io/alexgshaw/overfull-hbox@sha256:7dca952bb6736194a736b5110945f48842eafa8551e24468f1dddeed0daee799
test -S "$sock" && test ! -e "$asset/runs/$name"
args=()
if [[ "$variant" == v3 || "$variant" == v4 ]]; then args+=(-native-checkpoint); fi
export ZB_CHECKPOINT_NAMESPACE_DIR="$work/checkpoints/aries" ZB_VMM_CENSUS=1
bash "$asset/tracer/run-traced.sh" "$asset/aries-tex" "$asset/tracer" "$asset/runs/$name" \
    "${args[@]}" -trajectory "$asset/trajectory.json" -rollout "$asset/rollout.jsonl" \
    -codex-tools "$asset/codex-harbor-tools.tar.gz" -os-tools "$asset/overfull-hbox-debs.tar.gz" \
    -image "$image" -namespace aries -socket "$sock" -checkpoint-dir "$work/checkpoints" \
    -shim-dir "$work/shim" -io-dir "/run/$unit/aries-io" -vcpus 2 -memory-mib 4096 -storage-mib 10240
