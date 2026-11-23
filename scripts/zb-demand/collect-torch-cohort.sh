#!/usr/bin/env bash
# Retain the sealed PyTorch diagnostic without copying source bundle symlinks.
set -euo pipefail
shopt -s dotglob nullglob
worker=$1
remote=$2
out=$(realpath -m "$3")
variant=$4
[[ "$remote" =~ ^/local/[a-zA-Z0-9/_-]+$ ]]
[[ "$variant" == v3 || "$variant" == v4 ]]
[[ ! -e "$out" && ! -e "$out.tar.gz" ]]
ssh -o BatchMode=yes "$worker" "sudo -n bash -s -- '$remote' '$variant'" > "$out.tar.gz.partial" <<'WORKER'
set -euo pipefail
shopt -s dotglob nullglob
cd "$1"
files=()
for path in ./*; do
    if [[ "$path" != ./bundle ]]; then files+=("$path"); fi
done
run="./bundle/runs/4b588047-$2-cohort-01"
if [[ -d "$run" ]]; then files+=("$run"); fi
nice -n 10 ionice -c 3 tar -czf - -- "${files[@]}"
WORKER
mv "$out.tar.gz.partial" "$out.tar.gz"
mkdir "$out"
tar -xzf "$out.tar.gz" -C "$out"
run="$out/bundle/runs/4b588047-$variant-cohort-01"
if [[ -d "$run" ]]; then
    for path in "$run"/*; do
        [[ ! -e "$out/${path##*/}" ]]
        mv -- "$path" "$out/"
    done
    rmdir -- "$run" "$out/bundle/runs" "$out/bundle"
fi
