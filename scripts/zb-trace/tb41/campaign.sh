#!/usr/bin/env bash
# Run sources sequentially on one solo worker and preserve every attempt.
set -euo pipefail
stage=$(realpath "$1")
runtime=$(realpath "$2")
unit=$3
queue=$(realpath "$4")
prefix=$5
[[ "$prefix" =~ ^[A-Za-z0-9-]+$ ]]
mkdir -p "$stage/runs"
while IFS=$'\t' read -r task original source python codex os image_helper; do
    [[ "$task" =~ ^[a-z0-9-]+$ && "$original" =~ ^[a-f0-9-]{36}$ && "$source" =~ ^[a-zA-Z0-9_-]+$ ]]
    out="$stage/runs/$task-${original:0:8}-$prefix"
    [[ ! -e "$out" && ! -e "$out.driver" ]]
    flags=()
    [[ "$codex" == - ]] || flags+=(-codex-tools "$stage/fixtures/$codex")
    [[ "$os" == - ]] || flags+=(-os-tools "$stage/fixtures/$os")
    [[ -z "$image_helper" || "$image_helper" == - ]] || flags+=(-image-helper "$stage/fixtures/$image_helper")
    if [[ "$original" == 27c84b6a-af43-4332-9ba4-0f70ab1e160d ]]; then
        flags+=(-cni-network zbhof183 -cni-conf-dir "$stage/cni/conf" -cni-cache-dir "$stage/cni/cache" -cni-bin-dir "$stage/cni/bin")
    fi
    if [[ "$original" == be1d1d11-8af3-404e-9238-e4597b6552e2 ]]; then
        flags+=(-companion-image localhost/zb-nextjs-warehouse@sha256:decefdfc46bbed17ebdc58a58d0b220ae52d4c71b363ce42b58ecde17d309ba8
                -cni-network zbnext-be1d1d11-amd191 -cni-conf-dir "$stage/cni/conf"
                -cni-cache-dir "$stage/cni/cache" -cni-bin-dir "$stage/cni/bin")
    fi
    status=0
    ZB_SOURCE_DIR="$stage/sources/$source" ZB_GUEST_PYTHON="$python" \
        bash "$stage/zb-trace/tb41/pilot.sh" "$stage" "$runtime" "$unit" traced "$out" "${flags[@]}" >"$out.driver" 2>&1 || status=$?
    printf '%s\n' "$status" >"$out.exit"
    printf '%s %s %s\n' "$task" "$original" "$status"
done < "$queue"
