#!/usr/bin/env bash
set -euo pipefail
[[ "$(cat /var/emulab/boot/nodeid)" == amd201 ]]
handoff=$(realpath "$1")
stage=/local/zb-cancel-async-01
runtime=/local/zb-texpilot/v4/runtime
(cd "$handoff" && sha256sum --quiet -c SENT.sha256)
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
for unit in zb-cancel-async-deep-v4-amd201 zb-cancel-async-diagnostic-02 zb-relaxed-ml-e047-v4-retry-02; do
    test "$(systemctl show "$unit" -p ActiveState --value)" = inactive
done
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
python3 -B - "$handoff" "$runtime" "$stage" <<'PY'
import json
from pathlib import Path
import sys
p, runtime, stage=map(Path,sys.argv[1:])
plan=json.loads((p/"assignment.json").read_bytes())
source=json.loads((stage/"source-contract.json").read_bytes())
expected=sorted(f'{row["trial"][:8]}-v4-cancel-async-ordinary-01' for row in source["trials"])
assert plan["runs"] == expected and plan["previous_worker"] == "amd200" and plan["assigned_worker"] == "amd201"
_, original, old_pid, _=(p/"retired-invocation.txt").read_text().strip().split("\t")
assert original==plan["old_invocation"] and old_pid.isdecimal()
skip=[line.split("\t") for line in (p/"skip-ack.tsv").read_text().splitlines()]
assert sorted(row[2] for row in skip)==expected and all(row[3]=="skipped_on_amd200_assigned_to_amd201" for row in skip)
assert len({row[1] for row in skip}) == 1 and skip[0][1] != original
assert all(not (stage/"runs"/run).exists() for run in expected)
profile=json.loads((runtime/"runtime.json").read_bytes())
expected_profile=json.loads((p/"validated-profile.json").read_bytes())
assert {key:profile.get(key) for key in expected_profile} == expected_profile
PY
sudo -n python3 -B /local/zb-relaxed-ml-01/audit-idle-runtime.py fcmod-zepbound-tex-v4 "$runtime" > "$handoff/stack-before.json"
python3 -B - "$handoff" <<'PY'
import json,sys
from pathlib import Path
p=Path(sys.argv[1])
known=json.loads((p/"expected-stack-sha256.json").read_bytes())
live=json.loads((p/"stack-before.json").read_bytes())
assert live["node"]=="amd201"
assert known == {k:live["files"][k]["sha256"] for k in known}
PY
( set -o noclobber
  while read -r run; do
    printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$INVOCATION_ID" "$run" "accepted_on_amd201"
  done < "$handoff/runs.txt" > "$handoff/amd201-accepted.tsv"
)
bash "$handoff/worker.sh" "$stage" v4 ordinary
