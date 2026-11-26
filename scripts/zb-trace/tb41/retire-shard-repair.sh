#!/usr/bin/env bash
# Retire the newly provisioned shard stage after the runner verifies its archive.
set -euo pipefail
node=$1
name=$2
digest=$3
case "$node" in amd182|amd183|amd189|amd200) ;; *) exit 1 ;; esac
[[ "$digest" =~ ^[a-f0-9]{64}$ && "$name" =~ ^pretrain-shard-corruption-[a-f0-9]{8}-repair-$node-omp-two(-2)?$ ]]
stack=/local/zb-tb41-shard-repair
runtime="$stack/runtime"
unit="fcmod-zepbound-$node-shard-repair"
[[ ! -L "$stack" && "$(realpath -e "$stack")" == "$stack" ]]
printf '%s  %s\n' "$digest" "$stack/local-transfer/$name.tar.gz" | sha256sum --quiet -c -
test -f "$stack/runs/$name.cleanup"
ctr=("$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock")
if systemctl is-active --quiet "$unit"; then
    for ns in $("${ctr[@]}" namespaces ls -q); do
        test -z "$("${ctr[@]}" -n "$ns" tasks ls -q)"
        test -z "$("${ctr[@]}" -n "$ns" containers ls -q)"
    done
else
    grep -Fxq 'runtime_has_tasks=false' "$stack/runs/$name.cleanup"
    grep -Fxq 'runtime_has_containers=false' "$stack/runs/$name.cleanup"
fi
if pgrep -f "^$stack/replay/bitmap/firecracker " >/dev/null; then exit 1; fi
if [[ "$node" == amd200 ]]; then
    iptables -w -t mangle -L -v -n -x ZBS3HIST200
    "$stack/replay/shard-history-network.sh" "$stack/replay/cni" remove
    if ip link show dev zbs3hist200 >/dev/null 2>&1; then exit 1; fi
    if systemctl is-active --quiet zb-shard-history-dns-amd200 zb-shard-history-https-amd200; then exit 1; fi
    if iptables -w -t mangle -S ZBS3HIST200 >/dev/null 2>&1; then exit 1; fi
    echo 'historical_network_removed=true'
fi
if systemctl is-active --quiet "$unit"; then systemctl stop "$unit"; fi
if systemctl is-active --quiet "$unit"; then exit 1; fi
if findmnt -rn -o TARGET | grep -E "^$stack(/|$)"; then exit 1; fi
for device in $(dmsetup ls --target thin | awk -v prefix="$unit-snap-" 'index($1,prefix)==1 {print $1}'); do
    [[ "$device" =~ ^$unit-snap-[0-9]+$ ]]
    [[ "$(dmsetup info -c --noheadings -o open "$device" | tr -d ' ')" == 0 ]]
    expected=$(dmsetup info -c --noheadings --separator ',' -o major,minor "$unit" | tr -d ' ')
    dmsetup deps "$device" | tr -d ' ' | grep -Fq "($expected)"
    dmsetup remove "$device"
    printf 'owned_snapshot_removed=%s\n' "$device"
done
dmsetup remove "$unit"
if dmsetup info "$unit" >/dev/null 2>&1; then exit 1; fi
for backing in data metadata; do
    loop=$(losetup -j "$runtime/thinpool/$backing" -n -O NAME)
    [[ "$loop" =~ ^/dev/loop[0-9]+$ ]]
    losetup -d "$loop"
    [[ -z "$(losetup -j "$runtime/thinpool/$backing" -n -O NAME)" ]]
done
find "$stack" -xdev -depth -delete
test ! -e "$stack"
printf 'owned_service_inactive=true\nowned_thin_pool_removed=true\nowned_loops_detached=true\nowned_stage_removed=true\narchive_sha256_verified=%s\n' "$digest"
