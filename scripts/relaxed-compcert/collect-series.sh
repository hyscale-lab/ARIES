#!/usr/bin/env bash
# Archive all five raw cells while the worker is between measured variants.
set -euo pipefail
node=$1
variant=$2
root=$(realpath "$3")
case "$node:$variant" in amd182:v1|amd182:v4|amd189:v2|amd189:v3) ;; *) exit 2;; esac
local_dir="$root/repo/ARIES/.cache/zb-raw/relaxed-compcert/$variant"
mkdir -p "$local_dir"
exec 9>"$local_dir/collect.lock"
flock 9
if [[ -s "$local_dir/worker.sha256" ]]; then
    (cd "$local_dir" && sha256sum --quiet -c worker.sha256)
    if [[ ! -e $local_dir/extracted ]]; then
        (cd "$local_dir" && tar -xzf worker.tar.gz && touch extracted)
    fi
    exit 0
fi
ssh -o ConnectTimeout=15 "nehalem@$node.utah.cloudlab.us" "sudo -n bash -c 'set -eu; cd /local/zb-relaxed-compcert-01; test \$(wc -l < $variant/series-status.txt) = 5; tar --exclude=$variant/cni/bin --exclude=$variant/cni/cache --exclude=$variant/cni/ipam -I \"gzip -1\" -cf - inputs.sha256 $variant'" > "$local_dir/worker.partial.tar.gz"
tar -tzf "$local_dir/worker.partial.tar.gz" >/dev/null
mv "$local_dir/worker.partial.tar.gz" "$local_dir/worker.tar.gz"
(cd "$local_dir" && sha256sum worker.tar.gz > worker.sha256 && tar -xzf worker.tar.gz && touch extracted)
