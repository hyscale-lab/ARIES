#!/usr/bin/env bash
# Use the companion images, users, command overrides and readiness probe from the reviewed fixture.
set -euo pipefail
action=${1:?action}
run=${2:?prepared directory}
base=/local/zb-study3-stock
meta="$TB41_FIXTURE_DIR/metadata.json"
task=$(jq -er .task "$meta")
case "$task" in
medical-claims-processing) roles=(workspace playwright-mcp); prefix=claims;;
nextjs-performance) roles=(warehouse); prefix=nextjs;;
intrastat-meldung) roles=(idev odoo services dms compliance-hub); prefix=intrastat;;
*) exit 2;;
esac
case "$action" in
image)
    role=${3:?role}
    [[ " ${roles[*]} " == *" $role "* && "$role" =~ ^[a-z-]+$ ]]
    digest=$(jq -er --arg role "$role" '.companions[] | select(.Role==$role) | .Image | split("@")[1]' "$meta")
    image="127.0.0.1:18762/library/$prefix-$role@$digest"
    fixture="$run/$role"
    mkdir "$fixture"
    reg="$base/server-bundle/deps/regctl/v0.11.5/regctl"
    if [[ "$("$reg" image digest "$image" 2>/dev/null || true)" != "$digest" ]]; then
        jq -n --arg task "$prefix-$role" --arg digest "$digest" '{task:$task,image_digest:$digest}' >"$fixture/metadata.json"
        python3 "$base/scripts/zb-trace/stock/stock-store-archive.py" "$TB41_IMAGE_STORE" "$digest" "$fixture/image.tar"
        python3 "$base/scripts/zb-trace/stock/tb41-image.py" "$fixture/image.tar" "$fixture" "$reg" 127.0.0.1:18762 >&2
        rm "$fixture/image.tar"
    fi
    "$reg" image config "$image" --format raw-body >"$fixture/config.json"
    echo "$image"
    ;;
start)
    sandbox=${3:?sandbox}
    guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
    pid=$(jq -er '.pid | select(. > 0)' "$run/workload-state.json")
    for role in "${roles[@]}"; do
        if [[ "$role" == playwright-mcp ]]; then
            guest sh -ec 'grep "^pwuser:" /playwright-mcp/rootfs/etc/passwd | cut -d: -f3,4,6'
        fi >"$run/$role/user.txt"
        python3 - "$role" "$run" "$meta" "$pid" <<'PY'
import json,sys
from pathlib import Path
role, directory, metadata, pid = sys.argv[1:]
run = Path(directory)
spec = json.loads((run/'config.json').read_text())
image = json.loads((run/role/'config.json').read_text())['config']
fixture, = [v for v in json.loads(Path(metadata).read_text())['companions'] if v['Role'] == role]
process = spec['process']
process.update(args=fixture['Command'] or (image.get('Entrypoint') or []) + (image.get('Cmd') or []),
               cwd=image.get('WorkingDir') or '/',env=list(image['Env']))
home = '/root'
if role == 'playwright-mcp':
    uid,gid,home = (run/role/'user.txt').read_text().strip().split(':')
    assert image['User'] == 'pwuser' and int(uid)>0 and int(gid)>0
    process['user'] = dict(uid=int(uid),gid=int(gid))
    process['capabilities'].update(effective=[],permitted=[],ambient=[],inheritable=[])
    spec['hostname'] = fixture['Hostname']
    shm, = [m for m in spec['mounts'] if m['destination']=='/dev/shm']
    shm['options'] = [v for v in shm['options'] if not v.startswith('size=')] + [f"size={fixture['ShmSizeMiB']*1024}k"]
if not any(e.startswith('HOME=') for e in process['env']):
    process['env'].append('HOME='+home)
net, = [ns for ns in spec['linux']['namespaces'] if ns['type']=='network']
net['path'] = f'/proc/{int(pid)}/ns/net'
spec['linux']['cgroupsPath'] = '/zb-'+role
(run/role/'spec.json').write_text(json.dumps(spec)+'\n')
PY
        "$AENV_CLI" upload "$sandbox" "$run/$role/spec.json" "/$role/config.json"
        if [[ "$prefix" == claims ]]; then guest sh -ec 'printf "\n127.0.0.1 localhost workspace playwright-mcp\n" >> "/$1/rootfs/etc/hosts"' sh "$role"; fi
        guest sh -ec 'runc run -d --bundle "/$1" "zb-$1" </dev/null >"/tmp/zb-$1-start.log" 2>&1' sh "$role"
    done
    ;;
health)
    sandbox=${3:?sandbox}
    phase=${4:?phase}
    for role in "${roles[@]}"; do
        "$AENV_CLI" exec "$sandbox" runc state "zb-$role" >"$run/$role/$phase.json"
        jq -e '.status=="running"' "$run/$role/$phase.json" >/dev/null
    done
    if [[ "$prefix" == nextjs ]]; then
        "$AENV_CLI" exec "$sandbox" runc exec zb-workload /usr/local/bin/node -e "$(jq -er .nextjs_health "$meta")" >"$run/$phase-health.json"
        exit
    fi
    if [[ "$prefix" == intrastat ]]; then
        guest_health=$(jq -er .intrastat_health "$meta")
        "$AENV_CLI" exec "$sandbox" runc exec zb-workload sh -ec "$guest_health" >"$run/$phase-health.txt"
        exit
    fi
    "$AENV_CLI" exec "$sandbox" runc exec zb-playwright-mcp sh -ec 'test "$(id -u)" != 0; set -- $(stat -f -c "%S %b" /dev/shm); test "$(( $1 * $2 ))" -eq 1073741824; id; grep -E "^(CapEff|CapPrm|CapBnd):" /proc/self/status; df -k /dev/shm' >"$run/playwright-mcp/$phase-runtime.txt"
    "$AENV_CLI" exec "$sandbox" runc exec --user 65534:65534 zb-workload /usr/local/bin/python3 -c "$(jq -er .claims_health "$meta")" >"$run/$phase-health.json"
    ;;
*) exit 2;;
esac
