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
cpus=2 memory=4096 workdir=/tmp_workspace
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
    elif [[ "$(jq -er .task "$meta")" == payments-pipeline-fix ]]; then
        payments_helper="$(dirname "$0")/scripts/zb-trace/stock/payments-companions.sh"
    elif jq -e '.task == "medical-claims-processing" or .task == "intrastat-meldung" or .task == "nextjs-performance"' "$meta" >/dev/null; then
        claims_helper="$(dirname "$0")/scripts/zb-trace/stock/claims-companions.sh"
        mapfile -t companion_roles < <(jq -er '.companions[].Role' "$meta")
        for role in "${companion_roles[@]}"; do
            companion_image=$("$claims_helper" image "$run" "$role")
            mounts+=(--image-mount "/$role/rootfs=$companion_image")
        done
    fi
    ;;
roy)
    [[ "$image" == *@sha256:3ed43bff698735f15fa9db6e020df8e44e1d3ec37e8828ebc53eba054e509a77 ]]
    workdir=/app
    ;;
calendar)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    : "${CALENDAR_INPUTS:?}"
    ;;
pdf-digest|conflicting-handling)
    [[ "${image##*@}" == sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    ;;
sam3-inference|sam3-debug)
    [[ "${image##*@}" == sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    ;;
arxiv-digest|excel-search|video-skill|repo-homepage|scp-crawl|wiki-biography|paper-to-poster|arxiv-tables|social-[2-6]|schedule-edit|trash-fixture|chapter-summary)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
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
    ;;
image-category)
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    : "${WILDCLAW_IMAGES_TAR:?}"
    printf '%s  %s\n' f9ebc183b76d8f2ec58e9c0160a2f07e86fa6460d5febd786a2bf735f3368946 "$WILDCLAW_IMAGES_TAR" | sha256sum -c -
    ;;
*)
    source "$(dirname "$0")/retrieval-config.sh"
    retrieval_task "$task" || exit 2
    retrieval_fixture=$(dirname "$0")/retrieval-fixture.sh
    [[ "$image" == *@sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
    ;;
esac
echo "50dcc0b257d724fe07f3852c2dda52340223f038227c7572644eec870741223e  $runc_binary" | sha256sum -c -
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
date -u +%FT%T.%NZ >"$run/provision-start"
if [[ -n "${COW_PREPARE_SANDBOX:-}" ]]; then
    [[ "$cpus" == 2 && "$memory" == 4096 && -z "${STOCK_PACK_STORE:-}" ]]
    sandbox=$COW_PREPARE_SANDBOX
else
    sandbox=$("$AENV_CLI" start --cold -d --timeout 3600 --cpu "$cpus" --memory "$memory" \
        "${mounts[@]}" "$STOCK_BASE_IMAGE")
fi
echo "$sandbox" >"$run/original-id"
"$AENV_CLI" upload "$sandbox" "$runc_binary" /usr/local/bin/runc
guest sh -ec 'chmod 755 /usr/local/bin/runc; cd /workload; runc spec; cat config.json' >"$run/default-spec.json"
python3 - "$task" "$workdir" "$run" "${retrieval_fixture:-}" <<'PY'
import json,os,sys
from pathlib import Path
task,cwd,run,retrieval_fixture=sys.argv[1:];p=Path(run)
c=json.loads((p/'default-spec.json').read_text())
c['process'].update(args=['/bin/sleep','infinity'],cwd=cwd,terminal=False)
env=['HOME=/root','PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin']
if task=='roy':
    env[1]='PATH=/opt/venv/bin:/root/.local/bin:'+env[1][5:]
    env.extend(['VIRTUAL_ENV=/opt/venv','DEBIAN_FRONTEND=noninteractive'])
if task in {'pdf-digest','conflicting-handling'}:
    env.extend(['PIP_NO_INDEX=1','PIP_FIND_LINKS=/tmp/document-wheels'])
if retrieval_fixture or task.startswith(('social-','sam3-')) or task in {'arxiv-digest','excel-search','video-skill','repo-homepage','scp-crawl','wiki-biography','paper-to-poster','arxiv-tables','tb41', 'image-category', 'link-pix','pdf-digest','conflicting-handling','schedule-edit','trash-fixture','chapter-summary'}:
    # Match the source Docker default capability set in the nested workload container.
    caps=['CAP_'+name for name in 'CHOWN DAC_OVERRIDE FOWNER FSETID KILL SETGID SETUID SETPCAP NET_BIND_SERVICE NET_RAW SYS_CHROOT MKNOD AUDIT_WRITE SETFCAP'.split()]
    c['process']['capabilities'].update(bounding=caps,effective=caps,permitted=caps)
if task=='tb41':
    env=json.loads((Path(os.environ['TB41_FIXTURE_DIR'])/'image-env.json').read_text())
    assert isinstance(env,list) and all(isinstance(item,str) and '=' in item for item in env)
    task_name=json.loads((Path(os.environ['TB41_FIXTURE_DIR'])/'metadata.json').read_text())['task']
    if task_name=='payments-pipeline-fix':
        env.extend(['KAFKA_BROKERS=kafka:9092', 'KAFKA_TOPIC=transactions', 'KAFKA_CONSUMER_GROUP=transaction-workers', 'CUSTOMER_BASE=http://customer:9000'])
    if task_name=='freight-dispatch-shift':
        env.append('DISPATCH_EVENT_API_URL=http://event-feed:8080')
c['process']['env']=env
c['root']['readonly']=False
c['linux']['cgroupsPath']='/zb-workload'
assert 'network' in {ns['type'] for ns in c['linux']['namespaces']}
(p/'config.json').write_text(json.dumps(c))
PY
"$AENV_CLI" upload "$sandbox" "$run/config.json" /workload/config.json
if [[ -n "${retrieval_fixture:-}" ]]; then
    "$retrieval_fixture" "$task" upload "$sandbox" "$run"
elif [[ "$task" == video-skill ]]; then
    "$AENV_CLI" upload "$sandbox" "$(dirname "$0")/video-skill-check.py" /workload/rootfs/tmp/video-skill-check.py
    input=/local/zb-study3-stock/local-sol/video-skill/skill_plan.md
    echo "46a21925e9773a2871c9d34c6ddfde7510b24f899e2aa014ca3708e708af4dbb  $input" | sha256sum -c -
    "$AENV_CLI" upload "$sandbox" "$input" /workload/rootfs/tmp_workspace/skill_plan.md
    guest sh -ec 'cd /workload/rootfs; test ! -e tmp_workspace/Auto-Research-Skills; test -d tmp_workspace/.git; test -s tmp_workspace/AGENTS.md; test -s usr/lib/node_modules/openclaw/skills/skill-creator/scripts/package_skill.py'
elif [[ "$task" == repo-homepage ]]; then
    "$(dirname "$0")/homepage-fixture.sh" upload "$sandbox"
elif [[ "$task" == sam3-inference || "$task" == sam3-debug ]]; then
    "$(dirname "$0")/sam-fixture.sh" "$task" upload "$sandbox"
elif [[ "$task" == paper-to-poster ]]; then
    paper=/local/zb-study3-stock/local-sol/paper-to-poster
    "$AENV_CLI" upload "$sandbox" "$paper/environment.tar" /tmp/paper.tar
    "$AENV_CLI" upload "$sandbox" "$paper/wheels.tar" /tmp/paper-wheels.tar
    "$AENV_CLI" upload "$sandbox" "$(dirname "$0")/paper-image.mjs" /workload/rootfs/tmp/paper-image.mjs
    guest sh -ec 'cd /workload/rootfs/tmp_workspace; tar xf /tmp/paper.tar; mkdir /workload/rootfs/tmp/paper-wheels; tar xf /tmp/paper-wheels.tar -C /workload/rootfs/tmp/paper-wheels; rm /tmp/paper.tar /tmp/paper-wheels.tar'
elif [[ "$task" == arxiv-tables || "$task" == wiki-biography || "$task" == scp-crawl || "$task" == excel-search || "$task" == arxiv-digest ]]; then
    "$AENV_CLI" upload "$sandbox" /local/zb-study3-stock/local-sol/npm.tar /tmp/arxiv-npm.tar
    "$AENV_CLI" upload "$sandbox" "/local/zb-study3-stock/local-sol/$task/warmup.sh" /workload/rootfs/tmp/arxiv-warmup.sh
    if [[ "$task" == excel-search || "$task" == arxiv-digest ]]; then
        "$AENV_CLI" upload "$sandbox" "$(dirname "$0")/../../../cmd/aries-agentenv-replay/brave.mjs" /workload/rootfs/tmp/zb-brave.mjs
        "$AENV_CLI" upload "$sandbox" "/local/zb-study3-stock/local-sol/$task/skills.tar" /tmp/excel-skills.tar
        guest sh -ec 'mkdir -p /workload/rootfs/root/skills; tar xf /tmp/excel-skills.tar -C /workload/rootfs/root/skills; rm /tmp/excel-skills.tar; chown 0:0 /workload/rootfs/tmp_workspace /workload/rootfs/tmp_workspace/.git'
    fi
    if [[ "$task" == wiki-biography ]]; then
        "$AENV_CLI" upload "$sandbox" /local/zb-study3-stock/local-sol/wiki-biography/skills.tar /tmp/biography-skills.tar
        guest sh -ec 'mkdir -p /workload/rootfs/root/skills; tar xf /tmp/biography-skills.tar -C /workload/rootfs/root/skills; rm /tmp/biography-skills.tar; test -f /workload/rootfs/root/skills/agent-browser/SKILL.md'
    fi
    guest sh -ec 'mkdir /workload/rootfs/tmp/arxiv-npm; tar xf /tmp/arxiv-npm.tar -C /workload/rootfs/tmp/arxiv-npm; rm /tmp/arxiv-npm.tar'
elif [[ "$task" == social-[2-6] ]]; then
    "$(dirname "$0")/social-fixture.sh" "$task" upload "$sandbox"
elif [[ "$task" == pdf-digest || "$task" == conflicting-handling ]]; then
    "$(dirname "$0")/document-fixture.sh" "$task" upload "$sandbox"
elif [[ "$task" == calendar ]]; then
    for file in calendar.ics constraints.yaml meeting_requests.json; do
        "$AENV_CLI" upload "$sandbox" "$CALENDAR_INPUTS/$file" "/workload/rootfs/tmp_workspace/$file"
    done
    guest sh -ec 'cd /workload/rootfs; test -d tmp_workspace/.git; test ! -e tmp_workspace/results; if ! test -e root/USER.md; then cp tmp_workspace/USER.md root/USER.md; fi'
elif [[ "$task" == schedule-edit || "$task" == trash-fixture || "$task" == chapter-summary ]]; then
    local_fixture="${LOCAL_SOL_DIR:-/local/zb-study3-stock/local-sol}/$task"
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
if [[ -n "${payments_helper:-}" ]]; then "$payments_helper" start "$run" "$sandbox"; fi
if [[ -n "${claims_helper:-}" ]]; then "$claims_helper" start "$run" "$sandbox"; fi
if [[ -n "${freight_helper:-}" ]]; then "$freight_helper" start "$run" "$sandbox"; fi
if [[ "$task" == tb41 ]] && jq -e '.task == "hof-topology-interpenetration" or .task == "nextjs-performance"' "$meta" >/dev/null; then
    source "$(dirname "$0")/scripts/zb-trace/stock/hof-network.sh"
    if [[ "$(jq -er .task "$meta")" == nextjs-performance ]]; then hof_hosts=("${nextjs_hosts[@]}"); fi
    hof_network_guest "$sandbox" "$run"
fi
guest sh -ec 'cat /proc/cmdline; cat /sys/module/page_reporting/parameters/page_reporting_order; cat /sys/module/damon_reclaim/parameters/enabled; cat /proc/sys/kernel/random/boot_id' >"$run/guest-setup.txt"
if [[ "$task" == tb41 ]]; then
    "$(dirname "$0")/tb41-ready.sh" "$sandbox" "$TB41_FIXTURE_DIR"
    if [[ -n "${payments_helper:-}" ]]; then "$payments_helper" health "$run" "$sandbox" before-snapshot; fi
    if [[ -n "${claims_helper:-}" ]]; then "$claims_helper" health "$run" "$sandbox" before-snapshot; fi
elif [[ -n "${retrieval_fixture:-}" ]]; then
    source "$(dirname "$0")/hof-network.sh"
    source "$(dirname "$0")/retrieval-config.sh"
    retrieval_hosts "$task"
    hof_network_guest "$sandbox" "$run"
    "$retrieval_fixture" "$task" ready "$sandbox" "$run"
elif [[ "$task" == roy ]]; then
    guest runc exec zb-workload sh -ec 'cd /app; export LC_ALL=C; test "$(sha256sum data/*.mol data/TB3_Conf_Properties.csv | sha256sum)" = "236aeefd45cca191035b28b385bc8226eca7b4c2bf8fb1d8966bfd7b8f288bb4  -"; test -d /results; test ! -e /results/TB3_Conf_Answers.csv; test ! -e fit_roy.py; ! command -v apply_patch; test "$(command -v python)" = /opt/venv/bin/python; python -c "import sys,importlib.util as u,importlib.metadata as m; assert sys.version_info[:2]==(3,11) and u.find_spec(\"sklearn\") is None; assert [m.version(k) for k in (\"pandas\",\"numpy\",\"scipy\",\"rdkit\")]==[\"3.0.3\",\"2.4.6\",\"1.17.1\",\"2026.3.2\"]"'
elif [[ "$task" == paper-to-poster ]]; then
    guest runc exec zb-workload sh -ec 'cd /tmp/paper-wheels; echo "f0bece302c4c3745a9067f9be75f4f60ff3eb510f4c7dde03a8713877a7eee07  SHA256SUMS" | sha256sum -c -; sha256sum -c SHA256SUMS; cd /tmp_workspace; echo "abaca18dd17b55f6bad05529b6cd3ac058015e59962c8e6270d0e3b3ca4b6ea3  paper.pdf" | sha256sum -c -; test ! -e results; git rev-parse --is-inside-work-tree || git init -q; PIP_NO_INDEX=1 PIP_FIND_LINKS=/tmp/paper-wheels bash .wildclaw/run-warmup.sh; rm -r /tmp/paper-wheels'
elif [[ "$task" == video-skill ]]; then
    guest runc exec zb-workload sh -ec 'cd /tmp_workspace; chown 0:0 . .git; git rev-parse --is-inside-work-tree; test ! -e /dev/video0; test -z "${ANTHROPIC_API_KEY:-}"; ffmpeg -version | head -1; python3 --version' >"$run/video-skill-ready.txt"
elif [[ "$task" == repo-homepage ]]; then
    source "$(dirname "$0")/hof-network.sh"
    hof_hosts=("${homepage_hosts[@]}")
    hof_network_guest "$sandbox" "$run"
    "$(dirname "$0")/homepage-fixture.sh" ready "$sandbox" >"$run/homepage-ready.txt"
elif [[ "$task" == sam3-inference || "$task" == sam3-debug ]]; then
    if [[ "$task" == sam3-debug ]]; then
        source "$(dirname "$0")/hof-network.sh"
        hof_hosts=(github.com)
        hof_network_guest "$sandbox" "$run"
    fi
    "$(dirname "$0")/sam-fixture.sh" "$task" ready "$sandbox" >"$run/$task-ready.txt"
elif [[ "$task" == arxiv-tables || "$task" == wiki-biography || "$task" == scp-crawl || "$task" == excel-search || "$task" == arxiv-digest ]]; then
    source "$(dirname "$0")/hof-network.sh"
    hof_hosts=(arxiv.org); cutoff=2026-07-17T14:15:00Z
    if [[ "$task" == scp-crawl ]]; then hof_hosts=(scp-wiki.wikidot.com scp-wiki.wdfiles.com); cutoff=2026-07-17T14:33:19Z; fi
    if [[ "$task" == wiki-biography ]]; then hof_hosts=(zh.wikipedia.org zh.m.wikipedia.org); cutoff=2026-07-17T14:25:12Z; fi
    if [[ "$task" == excel-search ]]; then hof_hosts=(www.faa.gov brave-broker); cutoff=2026-07-17T16:14:50Z; fi
    if [[ "$task" == arxiv-digest ]]; then hof_hosts=(export.arxiv.org arxiv.org ar5iv.labs.arxiv.org brave-broker); cutoff=2026-07-17T14:11:34Z; fi
    hof_network_guest "$sandbox" "$run"
    guest runc exec --env npm_config_cache=/tmp/arxiv-npm --env npm_config_offline=true --env npm_config_before="$cutoff" zb-workload bash /tmp/arxiv-warmup.sh
    guest runc exec zb-workload sh -ec 'set -e; test -f /tmp/wildclaw-harbor-warmup.done; agent-browser --version; rm -r /tmp/arxiv-npm; test ! -e /tmp_workspace/results' >"$run/$task-ready.txt"
    if [[ "$task" == excel-search || "$task" == arxiv-digest ]]; then
        broker_address=$(awk '$2 == "brave-broker" {print $1}' "$run/hof-endpoints.txt")
        guest sh -ec 'printf "%s api.search.brave.com\n" "$1" >>/workload/rootfs/etc/hosts' _ "${ZB_BRAVE_PUBLIC_IP:?}"
        guest runc exec zb-workload sh -ec '
            cd /tmp_workspace; git rev-parse --git-dir; test ! -e results; command -v curl; test -z "$(command -v python)"
            echo "c9dc9a11b8a2d3a2ee281952c97a905a4c2e48c1218e285fa455c4a9c97f538d  /root/skills/web-search/SKILL.md" | sha256sum -c -
            test -z "${BRAVE_API_KEY:-}"; test "$(curl -sS --max-time 10 -w "%{http_code}" -o /dev/null -H "Content-Type: application/json" --data "{}" http://brave-broker:443/search)" = 403
        ' >>"$run/$task-ready.txt"
        if [[ "$task" == excel-search ]]; then
            guest runc exec zb-workload sh -ec '
                cd /tmp_workspace; test -z "$(git config user.name)$(git config user.email)"
                test ! -e 04_Search_Retrieval_task_6_excel_with_search; test -z "$(command -v pdftotext)"
                python3 -c "import importlib.util; assert importlib.util.find_spec(\"openpyxl\") is None"
            ' >>"$run/$task-ready.txt"
        else
            guest runc exec zb-workload sh -ec '
                cd /tmp_workspace
                for file in AGENTS.md BOOTSTRAP.md HEARTBEAT.md IDENTITY.md SOUL.md TOOLS.md USER.md; do
                    if ! test -e /root/"$file"; then cp "$file" /root/"$file"; fi
                done
                if ! test -d /root/.git; then git -C /root init -q; fi
                test "$(git -C /root rev-parse --show-toplevel)" = /root
                git -C /root status --short
                echo "95c467f710183b2c0397c29ec4ef886bb723671a39ee767dc4d59165f723b68f  /root/SOUL.md" | sha256sum -c -
                echo "178a5807bd2e6ace25b80a366f0f7d8d3f354a47f577c4ab0092397456b8738f  /root/USER.md" | sha256sum -c -
                test ! -e /root/MEMORY.md; test ! -e /root/memory/2026-07-17.md; test ! -e /root/memory/2026-07-16.md
                python3 -c "import bs4; print(bs4.__version__)"
            ' >>"$run/$task-ready.txt"
        fi
        guest sh -ec 'runc exec -d --env "ZB_BRAVE_BROKER_URL=$1" --env NODE_NO_WARNINGS=1 zb-workload node /tmp/zb-brave.mjs serve </dev/null >/tmp/zb-brave-node.log 2>&1' _ "http://$broker_address:443/search"
        guest runc exec zb-workload sh -ec 'for i in $(seq 60); do test ! -S /tmp/zb-brave.sock || exit 0; sleep 1; done; exit 1'
    fi
    if [[ "$task" == arxiv-tables ]]; then guest runc exec zb-workload curl --fail --silent --head --max-time 20 https://arxiv.org/e-print/2501.07888 >/dev/null; fi
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
if [[ "$task" == paper-to-poster || "$task" == sam3-inference || "$task" == repo-homepage ]]; then
    guest sh -ec 'runc exec -d --env HOME=/tmp/zb-paper-agent --env OPENCLAW_STATE_DIR=/tmp/zb-paper-agent/state --env NODE_NO_WARNINGS=1 zb-workload node /tmp/paper-image.mjs serve </dev/null >/tmp/zb-paper-node.log 2>&1'
    guest runc exec zb-workload sh -ec 'for i in $(seq 60); do test ! -S /tmp/zb-paper-image.sock || exit 0; sleep 1; done; exit 1'
fi
date -u +%FT%T.%NZ >"$run/ready"
guest runc exec zb-workload cat /proc/sys/kernel/random/boot_id >"$run/boot-id"
"$AENV_CLI" snapshot create "$sandbox" --name "prepared-$sandbox" >"$run/snapshot.txt"
"$AENV_CLI" delete "$sandbox"
if [[ -n "${COW_PREPARE_SANDBOX:-}" ]]; then exit 0; fi
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
if [[ -n "${claims_helper:-}" ]]; then "$claims_helper" health "$run" "$sandbox" after-restore; fi
if [[ -n "${payments_helper:-}" ]]; then "$payments_helper" health "$run" "$sandbox" after-restore; fi
