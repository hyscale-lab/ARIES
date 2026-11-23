#!/usr/bin/env bash
# Receive exactly the one audited v2 helper-search diagnostic.
set -euo pipefail
stage=$(realpath "$1")
[[ "$(cat /var/emulab/boot/nodeid)" == amd183 ]]
run=f265ccca-v2-cancel-async-ordinary-02
[[ "$(cut -f3 "$stage/skip-ack.tsv")" == "$run" ]]
[[ "$(cut -f4 "$stage/skip-ack.tsv")" == amd200_skip_no_dispatch_assign_amd183 ]]
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
test "$(systemctl show zb-protein-93c2-04.service -p ActiveState --value)" = inactive
! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
runtime=/local/zb-stock183-v2/runtime
sudo -n python3 -B "$stage/audit-idle-runtime.py" fcmod-zepbound-amd183-stockv2 "$runtime" > "$stage/stack-before.json"
python3 -B - "$stage" "$runtime" <<'PY'
import json,sys
from pathlib import Path
p, runtime=map(Path,sys.argv[1:])
expected=json.loads((p/"stock-components.json").read_bytes())
before=json.loads((p/"stack-before.json").read_bytes())
assert before["node"]=="amd183" and expected["config"]==before["config"]
assert expected["components"]=={key:before["files"][key]["sha256"] for key in expected["components"]}
profile=json.loads((runtime/"runtime.json").read_bytes())
assert profile["free_page_reporting"] and not profile.get("root_drive_use_pmem")
assert not profile.get("guest_reclamation",{}).get("Enabled")
PY
( set -o noclobber
  printf '%s\t%s\t%s\t%s\n' "$(date -u +%FT%TZ)" "$INVOCATION_ID" "$run" "accepted_on_amd183" > "$stage/amd183-accepted.tsv"
)
bash "$stage/worker.sh" "$stage" v2 ordinary f265ccca-0b2f-47e1-aaa5-b5e17e5e2950
