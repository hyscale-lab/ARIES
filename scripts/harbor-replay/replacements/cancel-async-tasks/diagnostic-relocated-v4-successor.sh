#!/usr/bin/env bash
# Keep the two source-bound v4 diagnostics behind all original v4 executions.
set -euo pipefail
scripts=$(realpath "$(dirname "$0")")
host=nehalem@amd201.utah.cloudlab.us
stage=/local/zb-cancel-async-02
predecessor=zb-cancel-async-ordinary-v4-amd201.service
remote=/local/zb-cancel-async-ordinary-v4-diagnostic
ssh_cmd=(ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host")
until "${ssh_cmd[@]}" "test \"\$(systemctl show '$predecessor' -p ActiveState --value)\" = inactive &&
       test -s /local/zb-cancel-async-01/runs/f265ccca-v4-cancel-async-ordinary-01/deployment-after.json &&
       ! sudo -n pgrep -x 'firecracker|[.]firecracker-re'"; do sleep 20; done
"${ssh_cmd[@]}" "umask 077; mkdir '$remote'"
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
cp "$scripts"/{diagnostic-queue.sh,worker.sh} "$tmp/"
(cd "$tmp" && sha256sum diagnostic-queue.sh worker.sh > SHA256SUMS)
tar -cf - -C "$tmp" . | ssh -o BatchMode=yes -o ConnectTimeout=6 "$host" "umask 077; tar -xf - -C '$remote'"
"${ssh_cmd[@]}" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
 sudo -n systemd-run --unit=zb-cancel-async-diagnostic-v4-amd201 --uid=nehalem --working-directory='$remote' \
   /usr/bin/bash '$remote/diagnostic-queue.sh' '$stage' relocated-v4 '$remote/worker.sh'"
