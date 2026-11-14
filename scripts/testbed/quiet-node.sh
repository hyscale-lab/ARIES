#!/usr/bin/env bash
# Removes sources of timing noise on a cluster node, as Invitro's
# scripts/setup/stabilize.sh did on the clusters measured before ARIES built
# its own: simultaneous multithreading, transparent huge pages, automatic
# NUMA balancing and swap are turned off. Turbo is left to cpu-frequency.sh.
# A reboot undoes all of it. Safe to run again.
#
#   sudo scripts/testbed/quiet-node.sh
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run as root" >&2; exit 1; }

smt=/sys/devices/system/cpu/smt/control
# "notsupported" or "forceoff" cannot be written; either way there is no SMT.
if [ -w "$smt" ] && [ "$(cat "$smt")" = on ]; then
  echo off >"$smt"
fi
for file in /sys/kernel/mm/transparent_hugepage/enabled /sys/kernel/mm/transparent_hugepage/defrag; do
  [ ! -w "$file" ] || echo never >"$file"
done
[ ! -w /proc/sys/kernel/numa_balancing ] || echo 0 >/proc/sys/kernel/numa_balancing
swapoff --all

echo "smt $(cat "$smt" 2>/dev/null || echo unknown), $(nproc) CPUs online," \
  "transparent hugepages $(sed -n 's/.*\[\(.*\)\].*/\1/p' /sys/kernel/mm/transparent_hugepage/enabled 2>/dev/null)," \
  "numa balancing $(cat /proc/sys/kernel/numa_balancing 2>/dev/null || echo n/a)," \
  "swap $(swapon --show --noheadings | wc -l) devices"
