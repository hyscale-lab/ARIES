#!/usr/bin/env bash
# Called only after the predecessor owner explicitly releases amd183.
set -euo pipefail
stage=$(realpath "$1")
archive=$(realpath "$2")
released_at=$3
expected=ad5c88ee-v2-replacement-02-worker.tar.gz
[[ "$(basename "$archive")" == "$expected" && "$released_at" =~ ^[0-9T:Z-]+$ ]]
test -s "$archive"
! systemctl is-active --quiet zb-openssl-ad5-patch-amd183.service
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
host=nehalem@amd183.utah.cloudlab.us
remote=/local/zb-protein-assembly-93c2-04
ssh_cmd=(ssh -n -o BatchMode=yes -o ConnectTimeout=6 "$host")
"${ssh_cmd[@]}" 'test "$(wc -l < /local/zb-protein-assembly-93c2-03/queue.tsv)" = 3 &&
  test "$(systemctl show zb-protein-93c2-03.service -p ActiveState --value)" = inactive &&
  ! sudo -n pgrep -x "firecracker|[.]firecracker-re"'
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
python3 -B - "$archive" "$released_at" "$tmp/harbor-release.json" <<'PY'
import hashlib,json,sys
from pathlib import Path
archive=Path(sys.argv[1])
with archive.open("rb") as stream:
 digest=hashlib.file_digest(stream,"sha256").hexdigest()
Path(sys.argv[3]).write_text(json.dumps(dict(worker="amd183",released_utc=sys.argv[2],
  predecessor="zb-openssl-ad5-patch-amd183.service", terminal_archive=archive.name, archive_sha256=digest),
  indent=2)+"\n")
PY
"${ssh_cmd[@]}" "umask 077; mkdir '$remote'; chmod 0700 '$remote'"
nice -n 10 tar -cf - -C "$stage" . -C "$tmp" harbor-release.json |
  ssh -o BatchMode=yes -o ConnectTimeout=6 "$host" "umask 077; tar -xf - -C '$remote'"
"${ssh_cmd[@]}" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
   sudo -n systemd-run --unit=zb-protein-93c2-04 --uid=nehalem --working-directory='$remote' \
      /usr/bin/bash '$remote/queue.sh' '$remote'"
"${ssh_cmd[@]}" "systemctl show zb-protein-93c2-04.service -p ActiveState,SubState,MainPID,InvocationID"
