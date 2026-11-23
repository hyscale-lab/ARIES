#!/usr/bin/env bash
# Called only after the predecessor owner explicitly releases the selected worker.
set -euo pipefail
stage=$(realpath "$1")
archive=$(realpath "$2")
released_at=$3
role=$4
case "$role" in
    retained-v1)
        node=amd183
        attempt=04
        arm=v2
        ;;
    ordinary-v4-repair)
        node=amd198
        attempt=05
        arm=v4
        ! systemctl is-active --quiet zb-openssl-deep-v4-repair.service
        ;;
    *) exit 2 ;;
esac
predecessor=zb-openssl-ad5-patch-$node.service
expected=ad5c88ee-$arm-replacement-02-worker.tar.gz
[[ "$(basename "$archive")" == "$expected" && "$released_at" =~ ^[0-9T:Z-]+$ ]]
test -s "$archive"
! systemctl is-active --quiet "$predecessor"
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
host=nehalem@$node.utah.cloudlab.us
remote=/local/zb-protein-assembly-93c2-$attempt
ssh_cmd=(ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host")
"${ssh_cmd[@]}" 'test "$(wc -l < /local/zb-protein-assembly-93c2-03/queue.tsv)" = 3 &&
  test "$(systemctl show zb-protein-93c2-03.service -p ActiveState --value)" = inactive &&
  ! sudo -n pgrep -x "firecracker|[.]firecracker-re"'
if [[ "$role" == ordinary-v4-repair ]]; then
    "${ssh_cmd[@]}" 'test ! -e /local/zb-protein-assembly-93c2-03/runs/93c2fb63-v4-protein-assembly-ordinary-03 &&
        grep -q $'"'"'^v4:ordinary\t1$'"'"' /local/zb-protein-assembly-93c2-03/queue.tsv &&
        systemctl is-active --quiet fcmod-zepbound-amd198-imagecircuit.service'
fi
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
python3 -B - "$archive" "$released_at" "$node" "$predecessor" "$tmp/harbor-release.json" <<'PY'
import hashlib,json,sys
from pathlib import Path
archive=Path(sys.argv[1])
with archive.open("rb") as stream:
 digest=hashlib.file_digest(stream,"sha256").hexdigest()
Path(sys.argv[5]).write_text(json.dumps(dict(worker=sys.argv[3],released_utc=sys.argv[2],
  predecessor=sys.argv[4], terminal_archive=archive.name, archive_sha256=digest),indent=2)+"\n")
PY
"${ssh_cmd[@]}" "umask 077; mkdir '$remote'; chmod 0700 '$remote'"
nice -n 10 tar -cf - -C "$stage" . -C "$tmp" harbor-release.json |
  ssh -o BatchMode=yes -o ConnectTimeout=6 "$host" "umask 077; tar -xf - -C '$remote'"
"${ssh_cmd[@]}" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
   sudo -n systemd-run --unit=zb-protein-93c2-$attempt --uid=nehalem --working-directory='$remote' \
      /usr/bin/bash '$remote/queue.sh' '$remote'"
"${ssh_cmd[@]}" "systemctl show zb-protein-93c2-$attempt.service -p ActiveState,SubState,MainPID,InvocationID"
