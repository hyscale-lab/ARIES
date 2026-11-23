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
attempt=${3:-01}
[[ $attempt == 01 || $attempt == 02 || $attempt == 03 ]]
remote=/local/zb-protein-assembly-93c2-$attempt
worker_unit=zb-protein-93c2-$attempt
ssh_cmd=(ssh -o BatchMode=yes -o ConnectTimeout=6 "$host")
until ! systemctl is-active --quiet "zb-timing17-hf-$node.service" &&
      "${ssh_cmd[@]}" "sudo -n test -s /tmp/zb-timing17-01/b615a3ec-$gate/driver.exit &&
      test \"\$(systemctl show -p ActiveState --value zb-timing17-hf.service)\" = inactive &&
      ! sudo -n pgrep -x 'firecracker|[.]firecracker-re'"; do
    sleep 30
done
if [[ $attempt != 01 && $node == amd183 ]]; then
    "${ssh_cmd[@]}" "sudo -n python3 -B - '$attempt'" <<'PYTHON'
from pathlib import Path
import json
import sys
expected={"v1:ordinary","v2:ordinary","v1:deep"}
for i in range(1,int(sys.argv[1])):
    old=Path(f"/local/zb-protein-assembly-93c2-{i:02d}")
    claims=(old/"queue.tsv").read_text().splitlines()
    assert {line.split("\t")[0] for line in claims} == expected
    arms=list((old/"runs").iterdir())
    assert len(arms)==3
    for arm in arms:
        assert (arm/"deployment-after.json").is_file() and int((arm/"driver.exit").read_text())==1
        rows=list(map(json.loads,(arm/"raw/events.jsonl").open()))
        events=[row["event"] for row in rows]
        assert events[:2]==["create_start","source_task_ready"] and events[-1]=="removed"
        assert set(events) <= {"create_start","source_task_ready","protein_dependency_probe_failure","removed"}
PYTHON
fi
printf '%s %s gate satisfied\n' "$(date -u +%FT%TZ)" "$node"
(cd "$stage" && sha256sum --quiet -c SHA256SUMS)
"${ssh_cmd[@]}" "umask 077; mkdir '$remote'; chmod 0700 '$remote'"
nice -n 10 tar -cf - -C "$stage" . | "${ssh_cmd[@]}" "umask 077; tar -xf - -C '$remote'"
"${ssh_cmd[@]}" "cd '$remote'; sha256sum --quiet -c SHA256SUMS &&
    sudo -n systemd-run --unit=$worker_unit --uid=nehalem --working-directory='$remote' \
        /usr/bin/bash '$remote/queue.sh' '$remote'"
"${ssh_cmd[@]}" "systemctl show $worker_unit.service \
    -p ActiveState -p SubState -p MainPID -p InvocationID -p ExecMainStartTimestamp"
