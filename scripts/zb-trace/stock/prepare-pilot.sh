#!/usr/bin/env bash
# Inside the stock service network namespace; credentials are read by the stock CLI.
set -euo pipefail
if (($# != 4)); then echo 'usage: prepare-pilot.sh roy|calendar|tb41 OCI_REF RUNC_BINARY NEW_RUN_DIR' >&2; exit 2; fi
task=$1
image=$2
runc_binary=$3
run=$4
mkdir -m700 "$run"
: "${AENV_CLI:?}" "${STOCK_BASE_IMAGE:?}"
case "$task" in
tb41)
    : "${TB41_FIXTURE_DIR:?}"
    meta="$TB41_FIXTURE_DIR/metadata.json"
    [[ "${image##*@}" == "$(jq -er .image_digest "$meta")" ]]
    cpus=$(jq -er .cpus "$meta")
    memory=$(jq -er .memory_mib "$meta")
    workdir=$(jq -er .workdir "$meta")
    ;;
roy)
    [[ "$image" == *@sha256:3ed43bff698735f15fa9db6e020df8e44e1d3ec37e8828ebc53eba054e509a77 ]]
    cpus=2 memory=4096 workdir=/app
    ;;
calendar)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    : "${CALENDAR_INPUTS:?}"
    cpus=1 memory=2048 workdir=/tmp_workspace
    ;;
*) exit 2 ;;
esac
echo "50dcc0b257d724fe07f3852c2dda52340223f038227c7572644eec870741223e  $runc_binary" | sha256sum -c -
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
date -u +%FT%T.%NZ >"$run/provision-start"
sandbox=$("$AENV_CLI" start --cold -d --timeout 3600 --cpu "$cpus" --memory "$memory" \
    --image-mount "/workload/rootfs=$image" "$STOCK_BASE_IMAGE")
echo "$sandbox" >"$run/original-id"
"$AENV_CLI" upload "$sandbox" "$runc_binary" /usr/local/bin/runc
guest sh -ec 'chmod 755 /usr/local/bin/runc; cd /workload; runc spec; cat config.json' >"$run/default-spec.json"
python3 - "$task" "$workdir" "$run" <<'PY'
import json,os,sys
from pathlib import Path
task,cwd,run=sys.argv[1:];p=Path(run)
c=json.loads((p/'default-spec.json').read_text())
c['process'].update(args=['/bin/sleep','infinity'],cwd=cwd,terminal=False)
env=['HOME=/root','PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin']
if task=='roy':
    env[1]='PATH=/opt/venv/bin:/root/.local/bin:'+env[1][5:]
    env.extend(['VIRTUAL_ENV=/opt/venv','DEBIAN_FRONTEND=noninteractive'])
if task=='tb41':
    # Match the source Docker default capability set in the nested workload container.
    caps=['CAP_'+name for name in 'CHOWN DAC_OVERRIDE FOWNER FSETID KILL SETGID SETUID SETPCAP NET_BIND_SERVICE NET_RAW SYS_CHROOT MKNOD AUDIT_WRITE SETFCAP'.split()]
    c['process']['capabilities'].update(bounding=caps,effective=caps,permitted=caps)
    env=json.loads((Path(os.environ['TB41_FIXTURE_DIR'])/'image-env.json').read_text())
    assert isinstance(env,list) and all(isinstance(item,str) and '=' in item for item in env)
c['process']['env']=env
c['root']['readonly']=False
c['linux']['cgroupsPath']='/zb-workload'
assert 'network' in {ns['type'] for ns in c['linux']['namespaces']}
(p/'config.json').write_text(json.dumps(c))
PY
"$AENV_CLI" upload "$sandbox" "$run/config.json" /workload/config.json
if [[ "$task" == calendar ]]; then
    for file in calendar.ics constraints.yaml meeting_requests.json; do
        "$AENV_CLI" upload "$sandbox" "$CALENDAR_INPUTS/$file" "/workload/rootfs/tmp_workspace/$file"
    done
    guest sh -ec 'cd /workload/rootfs; test -d tmp_workspace/.git; test ! -e tmp_workspace/results; if ! test -e root/USER.md; then cp tmp_workspace/USER.md root/USER.md; fi'
fi
guest sh -ec 'runc run -d --bundle /workload zb-workload </dev/null >/tmp/zb-workload-start.log 2>&1'
guest runc state zb-workload >"$run/workload-state.json"
guest sh -ec 'cat /proc/cmdline; cat /sys/module/page_reporting/parameters/page_reporting_order; cat /sys/module/damon_reclaim/parameters/enabled; cat /proc/sys/kernel/random/boot_id' >"$run/guest-setup.txt"
if [[ "$task" == tb41 ]]; then
    "$(dirname "$0")/tb41-ready.sh" "$sandbox" "$TB41_FIXTURE_DIR"
elif [[ "$task" == roy ]]; then
    guest runc exec zb-workload sh -ec 'cd /app; export LC_ALL=C; test "$(sha256sum data/*.mol data/TB3_Conf_Properties.csv | sha256sum)" = "236aeefd45cca191035b28b385bc8226eca7b4c2bf8fb1d8966bfd7b8f288bb4  -"; test -d /results; test ! -e /results/TB3_Conf_Answers.csv; test ! -e fit_roy.py; ! command -v apply_patch; test "$(command -v python)" = /opt/venv/bin/python; python -c "import sys,importlib.util as u,importlib.metadata as m; assert sys.version_info[:2]==(3,11) and u.find_spec(\"sklearn\") is None; assert [m.version(k) for k in (\"pandas\",\"numpy\",\"scipy\",\"rdkit\")]==[\"3.0.3\",\"2.4.6\",\"1.17.1\",\"2026.3.2\"]"'
else
    guest runc exec zb-workload sh -ec 'cd /tmp_workspace; sha256sum calendar.ics constraints.yaml meeting_requests.json; test -e /root/USER.md; git rev-parse --git-dir; command -v python3'
fi
date -u +%FT%T.%NZ >"$run/ready"
guest runc exec zb-workload cat /proc/sys/kernel/random/boot_id >"$run/boot-id"
"$AENV_CLI" snapshot create "$sandbox" --name "prepared-$sandbox" >"$run/snapshot.txt"
"$AENV_CLI" delete "$sandbox"
if [[ -n "${STOCK_PACK_STORE:-}" ]]; then
    snapshot_id=$(awk '/^Created snapshot / {print $3}' "$run/snapshot.txt")
    for _ in $(seq 900); do
        if [[ -s "$STOCK_PACK_STORE/snapshots/$snapshot_id/memory-startup.pack" ]] &&
           jq -e '.committed.memory_startup != null' "$STOCK_PACK_STORE/catalog/records/$snapshot_id.json" >/dev/null; then break; fi
        sleep 0.1
    done
    jq -e '.committed.memory_startup != null' "$STOCK_PACK_STORE/catalog/records/$snapshot_id.json" >/dev/null
    date -u +%FT%T.%NZ >"$run/initial-pack-ready"
fi
date -u +%FT%T.%NZ >"$run/restore-start"
sandbox=$("$AENV_CLI" start -d --timeout 3600 "prepared-$(cat "$run/original-id")")
echo "$sandbox" >"$run/restored-id"
date -u +%FT%T.%NZ >"$run/restore-end"
guest runc state zb-workload >"$run/restored-workload-state.json"
guest runc exec zb-workload cat /proc/sys/kernel/random/boot_id | cmp "$run/boot-id" -
