#!/usr/bin/env bash
set -euo pipefail
stage=$(realpath "$1")
host=nehalem@amd183.utah.cloudlab.us
remote=/local/zb-cancel-async-v2-diag-02
test "$(cut -f4 "$stage/skip-ack.tsv")" = amd200_skip_no_dispatch_assign_amd183
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host" "
  test \"\$(systemctl show zb-protein-93c2-04.service -p ActiveState --value)\" = inactive &&
  ! sudo -n pgrep -x 'firecracker|[.]firecracker-re' &&
  umask 077 && mkdir '$remote'"
nice -n 10 tar -cf - -C "$stage" . | ssh -o BatchMode=yes -o ConnectTimeout=6 "$host" "umask 077; tar -xf - -C '$remote'"
ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
  sudo -n systemd-run --unit=zb-cancel-async-v2-diag-02 --uid=nehalem --working-directory='$remote' \
    /usr/bin/bash '$remote/run-relocated-v2-diagnostic.sh' '$remote'"
ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host"   "systemctl show zb-cancel-async-v2-diag-02.service -p ActiveState,SubState,MainPID,InvocationID"
