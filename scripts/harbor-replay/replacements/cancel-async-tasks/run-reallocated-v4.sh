#!/usr/bin/env bash
# Boot the same deep fixture on the v1 worker after the original queue skips it.
set -euo pipefail
[[ "$(cat /var/emulab/boot/nodeid)" == amd201 ]]
handoff=$(realpath "$1")
stage=/local/zb-cancel-async-01
source_runtime=/local/zb-texpilot/v4/runtime
diagnostic=/local/zb-torch-deep-v3/runtime/common-rootfs.ext4
stack=/local/zb-cancel-async-deep-v4-amd201
run=7be2f099-v4-cancel-async-deep-01
unit=fcmod-zepbound-cancel-async-deep-v4
(cd "$handoff" && sha256sum --quiet -c SENT.sha256)
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
test ! -e "$stage/runs/$run"
test ! -e "$stack"
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
python3 -B - "$handoff" "$source_runtime/runtime.json" "$run" <<'PY'
import json
from pathlib import Path
import sys
p=Path(sys.argv[1])
plan=json.loads((p/"assignment.json").read_bytes())
assert plan["run_id"] == sys.argv[3] and plan["previous_worker"] == "amd200" and plan["assigned_worker"] == "amd201"
_, invocation, name, action=(p/"skip-ack.tsv").read_text().strip().split("\t")
assert name == plan["run_id"] and action == "skipped_on_amd200_assigned_to_amd201"
_, original, old_pid, _=(p/"retired-invocation.txt").read_text().strip().split("\t")
assert original==plan["old_invocation"] and invocation!=original and old_pid.isdecimal()
profile=json.loads(Path(sys.argv[2]).read_bytes())
assert profile["guest_root_mode"]=="disk" and profile["smt_enabled"] is False
assert profile["root_drive_use_pmem"] is True and profile["guest_reclamation"] == {"Enabled":True}
assert not profile.get("free_page_reporting", False)
PY
for u in zb-cancel-async-amd201-01 zb-relaxed-ml-e047-v4-retry-02 zb-cancel-async-diagnostic-02; do
    test "$(systemctl show "$u" -p ActiveState --value)" = inactive
done
echo "8674edc4b2578075ca405699ea23339559f76bae7aea9038c6d96511e8963cf5  $diagnostic" | sha256sum --quiet -c -
( set -o noclobber
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$INVOCATION_ID" "$run" "accepted_on_amd201" > "$handoff/amd201-accepted.tsv"
)
mkdir -p "$stack/runtime"
cp -a --reflink=auto "$source_runtime/bin" "$source_runtime/jailer.json" \
    "$source_runtime/common-vmlinux.bin" "$source_runtime/common-vmlinux.config" "$stack/runtime/"
cp --sparse=always --reflink=auto "$diagnostic" "$stack/runtime/common-rootfs.ext4"
chmod a-w "$stack/runtime/common-rootfs.ext4"
bash /local/zb-relaxed-ml-01/provision_baseline.sh "$stack" "$unit" "$source_runtime/runtime.json" v4
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
bash "$handoff/worker.sh" "$stage" v4 deep
