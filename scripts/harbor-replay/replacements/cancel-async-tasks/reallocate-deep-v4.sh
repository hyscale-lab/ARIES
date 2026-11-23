#!/usr/bin/env bash
# Move only the still-unstarted median v4 deep slot to the v1 worker.
set -euo pipefail
[[ "$(cat /var/emulab/boot/nodeid)" == amd200 ]]
old_unit=zb-cancel-async-amd200-01.service
old_invocation=$1
old_pid=$2
stage=/local/zb-cancel-async-01
handoff=/local/zb-cancel-async-deep-v4-handoff
[[ "$old_invocation" =~ ^[a-f0-9]{32}$ && "$old_pid" =~ ^[0-9]+$ ]]
test "$(systemctl show "$old_unit" -p InvocationID --value)" = "$old_invocation"
test "$(systemctl show "$old_unit" -p MainPID --value)" = "$old_pid"
test "$(systemctl show "$old_unit" -p ActiveState --value)" = active
systemctl is-active --quiet zb-sanitize-v2.service
test ! -d "$stage/runs"
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
mkdir -m 0700 "$handoff"
sha256sum "$stage/queue.sh" "$stage/worker.sh" "$stage/SHA256SUMS" > "$handoff/original.sha256"
systemctl show "$old_unit" -p MainPID,InvocationID,ExecStart,ActiveState,SubState > "$handoff/original-unit.txt"
systemctl show zb-relaxed-ml-v2-01 zb-relaxed-ml-v4-01 zb-resume-ocr23-01 \
  zb-relaxed-ml-e047-v2-retry-02 zb-sanitize-v2 -p Id,ActiveState,SubState > "$handoff/predecessors-before.txt"
python3 -B - "$stage/queue.sh" "$handoff" "$old_invocation" <<'PY'
import json
from pathlib import Path
import sys

queue, destination = map(Path, sys.argv[1:3])
original = queue.read_text()
tail = 'for arm in v2 v4; do bash "$stage/worker.sh" "$stage" "$arm" ordinary; done\nbash "$stage/worker.sh" "$stage" v4 deep\n'
if not original.endswith(tail):
    raise ValueError("the installed controller differs from the reviewed queue")
assignment = {
    "run_id": "7be2f099-v4-cancel-async-deep-01",
    "variant": "v4", "kind": "deep",
    "previous_worker": "amd200", "assigned_worker": "amd201",
    "old_invocation": sys.argv[3], "preserved_ordinary_variants": ["v2", "v4"],
    "preserved_predecessors": ["zb-relaxed-ml-v2-01", "zb-relaxed-ml-v4-01", "zb-resume-ocr23-01",
                               "zb-relaxed-ml-e047-v2-retry-02", "zb-sanitize-v2"],
}
(destination/"assignment.json").write_text(json.dumps(assignment, indent=2) + "\n")
marker = """handoff=/local/zb-cancel-async-deep-v4-handoff
test -f "$handoff/retired-invocation.txt"
( set -o noclobber
  printf '%s\\t%s\\t%s\\t%s\\n' "$(date -u +%FT%TZ)" "$INVOCATION_ID" \
    "7be2f099-v4-cancel-async-deep-01" "skipped_on_amd200_assigned_to_amd201" > "$handoff/skip-ack.tsv"
)
"""
head = original[:-len(tail)].replace("while true; do", marker+"while true; do", 1)
(destination/"queue-ordinary.sh").write_text(head + tail.splitlines()[0] + "\n")
PY
bash -n "$handoff/queue-ordinary.sh"
systemctl is-active --quiet zb-sanitize-v2.service
test ! -d "$stage/runs"
sudo -n systemctl stop "$old_unit"
test "$(systemctl show "$old_unit" -p MainPID --value)" = 0
test "$(systemctl show "$old_unit" -p ActiveState --value)" = inactive
test ! -d "/proc/$old_pid"
test ! -d "$stage/runs"
printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$old_invocation" "$old_pid" "$(cat /proc/sys/kernel/random/boot_id)" \
  > "$handoff/retired-invocation.txt"
systemctl show zb-relaxed-ml-v2-01 zb-relaxed-ml-v4-01 zb-resume-ocr23-01 \
  zb-relaxed-ml-e047-v2-retry-02 zb-sanitize-v2 -p Id,ActiveState,SubState > "$handoff/predecessors-after.txt"
diff -u "$handoff/predecessors-before.txt" "$handoff/predecessors-after.txt"
sha256sum --quiet -c "$handoff/original.sha256"
sudo -n systemd-run --unit="$old_unit" --uid=nehalem --working-directory="$stage" \
    /usr/bin/bash "$handoff/queue-ordinary.sh"
for i in 1 2 3 4 5; do
    if [[ -s $handoff/skip-ack.tsv ]]; then break; fi
    sleep 1
done
test -s "$handoff/skip-ack.tsv"
test "$(cut -f2 "$handoff/skip-ack.tsv")" = "$(systemctl show "$old_unit" -p InvocationID --value)"
cat "$handoff/skip-ack.tsv"
