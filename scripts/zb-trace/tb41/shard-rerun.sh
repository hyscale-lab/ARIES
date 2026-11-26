#!/usr/bin/env bash
# Preserve each native first reply and each owned service's post-run state.
set -euo pipefail
stack=$(realpath "$1")
node=$2
source=$3
arm=$4
unit="fcmod-zepbound-$node-shard-repair"
runtime="$stack/runtime"
stage="$stack/replay"
case "$source" in
    1ef39d1f|1d577700|14bdc7ee) ;;
    *) exit 2 ;;
esac
case "$arm" in
    default|omp-two) ;;
    *) exit 2 ;;
esac
out="$stack/runs/pretrain-shard-corruption-$source-repair-$node-$arm"
[[ ! -e "$out" && ! -e "$out.driver" ]]
mkdir -p "$stack/runs"
[[ ${#out} -le 93 ]]
flags=(-codex-tools "$stage/fixtures/codex-harbor-tools.tar.gz" -os-tools "$stage/fixtures/circuit-patch-debs.tar.gz")
if [[ "$source" == 14bdc7ee ]]; then
    flags+=(-shard-scipy-wheel "$stage/fixtures/shard-wave5-scipy.tar.gz")
fi
if [[ "$arm" == omp-two ]]; then
    flags+=(-diagnostic-shard-omp-two)
fi
status=0
ZB_SOURCE_DIR="$stage/sources/$source" ZB_GUEST_PYTHON=/usr/local/bin/python \
    bash "$stage/zb-trace/tb41/pilot.sh" "$stage" "$runtime" "$unit" traced "$out" "${flags[@]}" >"$out.driver" 2>&1 || status=$?
printf '%s\n' "$status" >"$out.exit"
cmp "$out/runtime.before.json" "$runtime/runtime.json"
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock")
for ns in $("${ctr[@]}" namespaces ls -q); do
    test -z "$("${ctr[@]}" -n "$ns" tasks ls -q)"
    test -z "$("${ctr[@]}" -n "$ns" containers ls -q)"
done
printf 'runtime_config_restored=true\nruntime_has_tasks=false\nruntime_has_containers=false\n' >"$out.cleanup"
