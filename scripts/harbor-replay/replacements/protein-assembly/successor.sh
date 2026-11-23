#!/usr/bin/env bash
# Keep the successor wait on the runner so no assets move during a measured job.
set -euo pipefail
stage=$(realpath "$1")
node=$2
case "$node" in
    amd183) gate=v2 ;;
    amd198) gate=v4 ;;
    *) exit 2 ;;
esac
host=nehalem@$node.utah.cloudlab.us
remote=/local/zb-protein-assembly-93c2-01
ssh_cmd=(ssh -o BatchMode=yes -o ConnectTimeout=6 "$host")
until ! systemctl is-active --quiet "zb-timing17-hf-$node.service" &&
      "${ssh_cmd[@]}" "test -s /tmp/zb-timing17-01/b615a3ec-$gate/driver.exit &&
      test \"\$(systemctl show -p ActiveState --value zb-timing17-hf.service)\" = inactive &&
      ! sudo -n pgrep -x 'firecracker|[.]firecracker-re'"; do
    sleep 30
done
printf '%s %s gate satisfied\n' "$(date -u +%FT%TZ)" "$node"
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
"${ssh_cmd[@]}" "umask 077; mkdir '$remote'; chmod 0700 '$remote'"
nice -n 10 tar -cf - -C "$stage" . | "${ssh_cmd[@]}" "umask 077; tar -xf - -C '$remote'"
"${ssh_cmd[@]}" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
    sudo -n systemd-run --unit=zb-protein-93c2-01 --uid=nehalem --working-directory='$remote' \
        /usr/bin/bash '$remote/queue.sh' '$remote'"
"${ssh_cmd[@]}" "systemctl show zb-protein-93c2-01.service \
    -p ActiveState -p SubState -p MainPID -p InvocationID -p ExecMainStartTimestamp"
