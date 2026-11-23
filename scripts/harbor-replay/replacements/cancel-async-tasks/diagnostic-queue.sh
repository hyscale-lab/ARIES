#!/usr/bin/env bash
# Append only the exact known partials after both existing study tails.
set -euo pipefail
stage=$(realpath "$1")
node=$(cat /var/emulab/boot/nodeid)
role=${2:-original}
worker_entry=${3:-"$stage/worker.sh"}
decision_log=$stage/retry-decisions.tsv
case "$node:$role" in
    amd201:relocated-v4)
        variants=(v4)
        units=(zb-cancel-async-ordinary-v4-amd201 zb-cancel-async-deep-v4-amd201 zb-cancel-async-diagnostic-02)
        handoff=/local/zb-cancel-async-ordinary-v4-handoff
        test "$(wc -l < "$handoff/amd201-accepted.tsv")" = 5
        diff -u <(cut -f3 "$handoff/skip-ack.tsv") <(cut -f3 "$handoff/amd201-accepted.tsv")
        decision_log=$stage/retry-decisions-v4-relocated.tsv
        ;;
    amd201:original)
        variants=(v1 v3)
        units=(zb-cancel-async-amd201-01 zb-relaxed-ml-e047-v4-retry-02)
        ;;
    amd200:original)
        variants=(v2 v4)
        units=(zb-cancel-async-amd200-01 zb-sanitize-retry-02 zb-ocr-v2-recovery zb-marker-rest-01)
        ;;
    *) exit 2 ;;
esac
predecessor_idle() {
    local unit state
    for unit in "${units[@]}"; do
        state=$(systemctl show -p ActiveState --value "$unit")
        [[ $state == inactive || $state == failed ]] || return 1
    done
    ! sudo -n pgrep -x 'firecracker|[.]firecracker-re'
}
until predecessor_idle; do sleep 15; done
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
for variant in "${variants[@]}"; do
    for trial in b4e5be65-a253-4086-bc35-819d746f9549 f265ccca-0b2f-47e1-aaa5-b5e17e5e2950; do
        strict=/local/zb-cancel-async-01/runs/${trial:0:8}-$variant-cancel-async-ordinary-01
        decision=$(sudo -n python3 -B - "$strict" "${trial:0:8}" <<'PY'
from pathlib import Path
import json, sys

root, trial = Path(sys.argv[1]), sys.argv[2]
event_path = root/"raw/events.jsonl"
summary = root/"driver.exit"
if not all(p.is_file() for p in (event_path, summary, root/"deployment-after.json")):
    print("missing_terminal_strict_evidence")
    raise SystemExit
if int(summary.read_text()) == 0:
    print("skip_delivered")
    raise SystemExit
replies = [row for row in map(json.loads, event_path.open()) if row["event"] == "tool_reply"]
binding = {
    "b4e5be65": ("call_IVFjbCkSg5L8zT6Yi0788wdK", 6, -1, 0),
    "f265ccca": ("call_TKcunO6gZG5qPc1QGeSvjWme", 9, 0, 1),
}[trial]
observed = tuple(replies[-1].get(k) for k in ("source_call_id", "step_id", "exit_code", "expected_exit_code")) if replies else ()
print("retry" if observed == binding else "other_strict_discrepancy")
PY
)
        status=0
        if [[ $decision == retry ]]; then
            bash "$worker_entry" "$stage" "$variant" ordinary "$trial" || status=$?
        fi
        printf '%s\t%s\t%s\t%s\n' "$trial" "$variant" "$decision" "$status" >> "$decision_log"
    done
done
