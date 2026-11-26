#!/usr/bin/env bash
# Inside the stock service network namespace; credentials are read by the stock CLI.
set -euo pipefail
if (($# != 4)); then echo 'usage: prepare-pilot.sh roy|calendar|image-category|tb41 OCI_REF RUNC_BINARY NEW_RUN_DIR' >&2; exit 2; fi
task=$1
image=$2
runc_binary=$3
run=$4
mkdir -m700 "$run"
: "${AENV_CLI:?}" "${STOCK_BASE_IMAGE:?}"
mounts=(--image-mount "/workload/rootfs=$image")
case "$task" in
tb41)
    : "${TB41_FIXTURE_DIR:?}"
    meta="$TB41_FIXTURE_DIR/metadata.json"
    platform=$(jq -er .image_digest "$meta")
    if [[ -s "$TB41_FIXTURE_DIR/image-platform.digest" ]]; then platform=$(cat "$TB41_FIXTURE_DIR/image-platform.digest"); fi
    [[ "${image##*@}" == "$platform" && "$platform" =~ ^sha256:[0-9a-f]{64}$ ]]
    cpus=$(jq -er .cpus "$meta")
    memory=$(jq -er .memory_mib "$meta")
    workdir=$(jq -er .workdir "$meta")
    if [[ -f "${run%-prepared}.deployment-at-launch.txt" ]]; then
        cp "${run%-prepared}.deployment-at-launch.txt" "$run/deployment-at-launch.txt"
    fi
    if [[ "$(jq -er .task "$meta")" == freight-dispatch-shift ]]; then
        freight_helper="$(dirname "$0")/scripts/zb-trace/stock/freight-companion.sh"
        feed_image=$("$freight_helper" image "$run")
        mounts+=(--image-mount "/event-feed/rootfs=$feed_image")
    fi
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
pdf-digest|conflicting-handling)
    [[ "${image##*@}" == sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    cpus=1 memory=2048 workdir=/tmp_workspace
    ;;
social-[2-6]|schedule-edit|trash-fixture|chapter-summary)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    cpus=1 memory=2048 workdir=/tmp_workspace
    ;;
link-pix)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    : "${PUZZLE_FIXTURE:?}"
    (cd "$PUZZLE_FIXTURE" && sha256sum -c <<'SUMS'
673b823a9dcada9e00c516423054ecfbd9d68e970f2ae5af1bf2d65133125b54  environment/.wildclaw/run-warmup.sh
3a841a2e4dfb312eee9ea6673694f1b21f954b435ac69e1167fa0f25dce349d2  wheel-sha256
8ba2717f6c2b841450be6228ec2a6b886ac4db2338500d6099e500665223fce5  environment/input/origin.png
f015393667effe65afcca52e67332eaa5d5c97ee84682043ced8aa177e9d010f  environment/input/puzzle_data.json
SUMS
    )
    cpus=1 memory=2048 workdir=/tmp_workspace
    ;;
image-category)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    : "${WILDCLAW_IMAGES_TAR:?}"
    printf '%s  %s\n' f9ebc183b76d8f2ec58e9c0160a2f07e86fa6460d5febd786a2bf735f3368946 "$WILDCLAW_IMAGES_TAR" | sha256sum -c -
    cpus=1 memory=512 workdir=/tmp_workspace
    ;;
*) exit 2 ;;
esac
echo "50dcc0b257d724fe07f3852c2dda52340223f038227c7572644eec870741223e  $runc_binary" | sha256sum -c -
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
date -u +%FT%T.%NZ >"$run/provision-start"
sandbox=$("$AENV_CLI" start --cold -d --timeout 3600 --cpu "$cpus" --memory "$memory" \
    "${mounts[@]}" "$STOCK_BASE_IMAGE")
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
if task in {'pdf-digest','conflicting-handling'}:
    env.extend(['PIP_NO_INDEX=1','PIP_FIND_LINKS=/tmp/document-wheels'])
if task.startswith('social-') or task in {'tb41', 'image-category', 'link-pix','pdf-digest','conflicting-handling','schedule-edit','trash-fixture','chapter-summary'}:
    # Match the source Docker default capability set in the nested workload container.
    caps=['CAP_'+name for name in 'CHOWN DAC_OVERRIDE FOWNER FSETID KILL SETGID SETUID SETPCAP NET_BIND_SERVICE NET_RAW SYS_CHROOT MKNOD AUDIT_WRITE SETFCAP'.split()]
    c['process']['capabilities'].update(bounding=caps,effective=caps,permitted=caps)
if task=='tb41':
    env=json.loads((Path(os.environ['TB41_FIXTURE_DIR'])/'image-env.json').read_text())
    assert isinstance(env,list) and all(isinstance(item,str) and '=' in item for item in env)
    if json.loads((Path(os.environ['TB41_FIXTURE_DIR'])/'metadata.json').read_text())['task']=='freight-dispatch-shift':
        env.append('DISPATCH_EVENT_API_URL=http://event-feed:8080')
c['process']['env']=env
c['root']['readonly']=False
c['linux']['cgroupsPath']='/zb-workload'
assert 'network' in {ns['type'] for ns in c['linux']['namespaces']}
(p/'config.json').write_text(json.dumps(c))
PY
"$AENV_CLI" upload "$sandbox" "$run/config.json" /workload/config.json
if [[ "$task" == social-[2-6] ]]; then
    "$(dirname "$0")/social-fixture.sh" "$task" upload "$sandbox"
elif [[ "$task" == pdf-digest || "$task" == conflicting-handling ]]; then
    "$(dirname "$0")/document-fixture.sh" "$task" upload "$sandbox"
elif [[ "$task" == calendar ]]; then
    for file in calendar.ics constraints.yaml meeting_requests.json; do
        "$AENV_CLI" upload "$sandbox" "$CALENDAR_INPUTS/$file" "/workload/rootfs/tmp_workspace/$file"
    done
    guest sh -ec 'cd /workload/rootfs; test -d tmp_workspace/.git; test ! -e tmp_workspace/results; if ! test -e root/USER.md; then cp tmp_workspace/USER.md root/USER.md; fi'
elif [[ "$task" == schedule-edit || "$task" == trash-fixture || "$task" == chapter-summary ]]; then
    local_fixture="/local/zb-study3-stock/local-sol/$task"
    (cd "$local_fixture" && sha256sum -c environment.sha256)
    "$AENV_CLI" upload "$sandbox" "$local_fixture/environment.tar" /tmp/local-environment.tar
    guest sh -ec 'cd /workload/rootfs/tmp_workspace; tar --no-same-owner -xf /tmp/local-environment.tar; rm /tmp/local-environment.tar'
    if [[ "$task" == chapter-summary ]]; then
        guest sh -ec 'cd /workload/rootfs; test ! -e tmp_workspace/linux_summary.md; test -r tmp_workspace/chapter_0_introduction_linux_os.md; if ! test -e root/USER.md; then cp tmp_workspace/USER.md root/USER.md; fi'
    fi
elif [[ "$task" == link-pix ]]; then
    guest sh -ec 'cd /workload/rootfs/tmp_workspace; test ! -e results; test ! -e solve.py; mkdir -p input .wildclaw'
    for file in input/origin.png input/puzzle_data.json .wildclaw/run-warmup.sh; do
        "$AENV_CLI" upload "$sandbox" "$PUZZLE_FIXTURE/environment/$file" "/workload/rootfs/tmp_workspace/$file"
    done
    "$AENV_CLI" upload "$sandbox" "$PUZZLE_FIXTURE/wheels.tar" /tmp/puzzle-wheels.tar
    guest sh -ec 'mkdir /workload/rootfs/tmp/puzzle-wheels; tar xf /tmp/puzzle-wheels.tar -C /workload/rootfs/tmp/puzzle-wheels; rm /tmp/puzzle-wheels.tar'
    "$AENV_CLI" upload "$sandbox" "$PUZZLE_FIXTURE/wheel-sha256" /workload/rootfs/tmp/puzzle-wheels/SHA256SUMS
elif [[ "$task" == image-category ]]; then
    guest sh -ec 'cd /workload/rootfs/tmp_workspace; test ! -e extracted; test ! -e results; test ! -e classify.py'
    "$AENV_CLI" upload "$sandbox" "$WILDCLAW_IMAGES_TAR" /workload/rootfs/tmp_workspace/images.tar
fi
guest sh -ec 'runc run -d --bundle /workload zb-workload </dev/null >/tmp/zb-workload-start.log 2>&1'
guest runc state zb-workload >"$run/workload-state.json"
if [[ -n "${freight_helper:-}" ]]; then "$freight_helper" start "$run" "$sandbox"; fi
if [[ "$task" == tb41 && "$(jq -er .task "$meta")" == hof-topology-interpenetration ]]; then
    source "$(dirname "$0")/scripts/zb-trace/stock/hof-network.sh"
    hof_network_guest "$sandbox" "$run"
fi
guest sh -ec 'cat /proc/cmdline; cat /sys/module/page_reporting/parameters/page_reporting_order; cat /sys/module/damon_reclaim/parameters/enabled; cat /proc/sys/kernel/random/boot_id' >"$run/guest-setup.txt"
if [[ "$task" == tb41 ]]; then
    "$(dirname "$0")/tb41-ready.sh" "$sandbox" "$TB41_FIXTURE_DIR"
elif [[ "$task" == roy ]]; then
    guest runc exec zb-workload sh -ec 'cd /app; export LC_ALL=C; test "$(sha256sum data/*.mol data/TB3_Conf_Properties.csv | sha256sum)" = "236aeefd45cca191035b28b385bc8226eca7b4c2bf8fb1d8966bfd7b8f288bb4  -"; test -d /results; test ! -e /results/TB3_Conf_Answers.csv; test ! -e fit_roy.py; ! command -v apply_patch; test "$(command -v python)" = /opt/venv/bin/python; python -c "import sys,importlib.util as u,importlib.metadata as m; assert sys.version_info[:2]==(3,11) and u.find_spec(\"sklearn\") is None; assert [m.version(k) for k in (\"pandas\",\"numpy\",\"scipy\",\"rdkit\")]==[\"3.0.3\",\"2.4.6\",\"1.17.1\",\"2026.3.2\"]"'
elif [[ "$task" == social-[2-6] ]]; then
    "$(dirname "$0")/social-fixture.sh" "$task" ready "$sandbox" >"$run/social-ready.txt"
elif [[ "$task" == pdf-digest || "$task" == conflicting-handling ]]; then
    "$(dirname "$0")/document-fixture.sh" "$task" ready "$sandbox"
elif [[ "$task" == schedule-edit || "$task" == trash-fixture || "$task" == chapter-summary ]]; then
    case "$task" in
        schedule-edit) guest runc exec zb-workload sh -ec 'cd /tmp_workspace; test -d .git; sha256sum scheduler.txt; test ! -e /root/MEMORY.md';;
        chapter-summary) guest runc exec zb-workload sha256sum /tmp_workspace/chapter_0_introduction_linux_os.md /root/USER.md;;
        trash-fixture) guest runc exec zb-workload sh -ec 'cd /tmp_workspace; test -d trash; test ! -L trash; test -n "$(find trash -type f -print -quit)"; du -sh trash';;
    esac >"$run/local-ready.txt"
elif [[ "$task" == link-pix ]]; then
    guest runc exec zb-workload sh -ec 'cd /tmp/puzzle-wheels; sha256sum -c SHA256SUMS'
    guest runc exec --env PIP_NO_INDEX=1 --env PIP_FIND_LINKS=/tmp/puzzle-wheels zb-workload bash /tmp_workspace/.wildclaw/run-warmup.sh
    guest runc exec zb-workload sh -ec 'cd /tmp_workspace; sha256sum input/origin.png input/puzzle_data.json; test -f /tmp/wildclaw-harbor-warmup.done; python3 -c "import openai,PIL,numpy; print(openai.__version__,PIL.__version__,numpy.__version__)"; ! command -v python' >"$run/puzzle-ready.txt"
    guest runc exec zb-workload rm -r /tmp/puzzle-wheels
elif [[ "$task" == calendar ]]; then
    guest runc exec zb-workload sh -ec 'cd /tmp_workspace; sha256sum calendar.ics constraints.yaml meeting_requests.json; test -e /root/USER.md; git rev-parse --git-dir; command -v python3'
else
    guest runc exec zb-workload sh -ec 'test -r /tmp_workspace/images.tar; command -v python3; readlink -f /bin/sh'
    guest runc exec zb-workload sha256sum /tmp_workspace/images.tar >"$run/fixture.sha256"
    printf '%s  /tmp_workspace/images.tar\n' f9ebc183b76d8f2ec58e9c0160a2f07e86fa6460d5febd786a2bf735f3368946 | cmp "$run/fixture.sha256" -
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
