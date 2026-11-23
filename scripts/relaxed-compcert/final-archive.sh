#!/usr/bin/env bash
set -euo pipefail
variant=$1
root=$(realpath "$2")
case $variant in v3) node=amd189 ;; v4) node=amd182 ;; *) exit 2 ;; esac
base=/local/zb-relaxed-compcert-01
host="nehalem@$node.utah.cloudlab.us"
while ! ssh -o ConnectTimeout=15 "$host" "sudo -n bash -c 'test -s $base/$variant/series-status.txt && test \$(wc -l < $base/$variant/series-status.txt) = 5 && ! systemctl is-active --quiet zb-relaxed-compcert-$variant'"; do
    sleep 45
done
exec "$root/repo/ARIES/scripts/relaxed-compcert/collect-series.sh" "$node" "$variant" "$root"
