#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1")
node=$2
case "$node" in
    amd201) units=(zb-cancel-async-amd201-01 zb-relaxed-ml-e047-v4-retry-02) ;;
    amd200) units=(zb-cancel-async-amd200-01 zb-sanitize-retry-02 zb-ocr-v2-recovery zb-marker-rest-01) ;;
    *) exit 2 ;;
esac
host=nehalem@$node.utah.cloudlab.us
remote=/local/zb-cancel-async-02
ssh_cmd=(ssh -o BatchMode=yes -o ConnectTimeout=6 "$host")
remote_gates() {
    "${ssh_cmd[@]}" /usr/bin/bash -s -- "${units[@]}" <<'CHECK'
for unit in "$@"; do
    state=$(systemctl show -p ActiveState --value "$unit")
    [[ $state == inactive || $state == failed ]] || exit 1
done
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
CHECK
}
until remote_gates; do sleep 20; done
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
"${ssh_cmd[@]}" "umask 077; mkdir '$remote'; chmod 0700 '$remote'"
nice -n 10 tar -cf - -C "$stage" . | "${ssh_cmd[@]}" "umask 077; tar -xf - -C '$remote'"
"${ssh_cmd[@]}" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
    sudo -n systemd-run --unit=zb-cancel-async-diagnostic-02 --uid=nehalem --working-directory='$remote' \
        /usr/bin/bash '$remote/diagnostic-queue.sh' '$remote'"
"${ssh_cmd[@]}" "systemctl show zb-cancel-async-diagnostic-02 -p ActiveState,SubState,MainPID,InvocationID"
