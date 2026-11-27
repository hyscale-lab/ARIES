#!/usr/bin/env bash
set -euo pipefail
task=$1 stage=$2 sandbox=$3 run=$4
here=$(dirname "$(realpath "$0")")
case "$task" in
    local-fact-check|local-comment-draft|fixture-file-overwrite|openmmlab-contributors|conference-papers|scholar-search|paper-affiliations|resume-homepage)
        exec "$here/supplemental-fixture.sh" "$@";;
esac
base=/local/zb-study3-stock
source "$here/retrieval-config.sh"
retrieval_config "$task" 6 ready "$base"
fixture="$base/local-sol/$task"
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
upload() { "$AENV_CLI" upload "$sandbox" "$1" "$2"; }

if [[ "$stage" == upload ]]; then
    (cd "$fixture" && sha256sum -c SHA256SUMS)
    upload "$fixture/inputs.tar" /tmp/retrieval-inputs.tar
    guest sh -ec 'tar --no-same-owner -C /workload/rootfs -xf /tmp/retrieval-inputs.tar; rm /tmp/retrieval-inputs.tar'
    upload "$here/../../../cmd/aries-agentenv-replay/brave.mjs" /workload/rootfs/tmp/zb-brave.mjs
    upload "$here/web-fetch.mjs" /workload/rootfs/tmp/zb-web-fetch.mjs
    if [[ "$task" != tomllib-trace ]]; then
        upload "$base/local-sol/npm.tar" /tmp/retrieval-npm.tar
        guest sh -ec 'mkdir /workload/rootfs/tmp/retrieval-npm; tar xf /tmp/retrieval-npm.tar -C /workload/rootfs/tmp/retrieval-npm; rm /tmp/retrieval-npm.tar'
    fi
    exit
fi
[[ "$stage" == ready ]]
guest runc exec zb-workload sh -ec 'test ! -e /tmp_workspace/results; test ! -e /tmp/wildclaw-harbor-warmup.done; test -z "${BRAVE_API_KEY:-}"; echo "c9dc9a11b8a2d3a2ee281952c97a905a4c2e48c1218e285fa455c4a9c97f538d  /root/skills/web-search/SKILL.md" | sha256sum -c -'
if [[ "$task" == tomllib-trace ]]; then
    guest runc exec zb-workload sh -ec '
        echo "95c467f710183b2c0397c29ec4ef886bb723671a39ee767dc4d59165f723b68f  /root/SOUL.md" | sha256sum -c -
        echo "178a5807bd2e6ace25b80a366f0f7d8d3f354a47f577c4ab0092397456b8738f  /root/USER.md" | sha256sum -c -
        for p in /usr/lib/node_modules/openclaw/skills/web-search/SKILL.md /root/MEMORY.md /root/memory/2026-07-16.md /root/memory/2026-07-17.md; do test ! -e "$p"; done
        test ! -e /tmp_workspace/.wildclaw/run-warmup.sh'
else
    cutoff=$(head -n1 "$trace" | jq -er .timestamp)
    guest runc exec --env npm_config_cache=/tmp/retrieval-npm --env npm_config_offline=true --env npm_config_before="$cutoff" zb-workload bash /tmp_workspace/.wildclaw/run-warmup.sh
    guest runc exec zb-workload sh -ec 'test -f /tmp/wildclaw-harbor-warmup.done; agent-browser --version | grep -F 0.27.0; rm -r /tmp/retrieval-npm'
fi
broker=$(awk '$2=="brave-broker"{print $1}' "$run/hof-endpoints.txt")
[[ "$broker" == 192.0.2.* ]]
guest sh -ec 'printf "%s api.search.brave.com\n" "$1" >> /workload/rootfs/etc/hosts' _ "${ZB_BRAVE_PUBLIC_IP:?}"
guest runc exec zb-workload sh -ec 'test "$(curl --max-time 10 -sS -o /dev/null -w "%{http_code}" -H "Content-Type: application/json" --data "{}" "$1")" = 403' _ "http://$broker:443/search"
guest sh -ec 'runc exec -d --env ZB_BRAVE_BROKER_URL="$1" --env NODE_NO_WARNINGS=1 zb-workload node /tmp/zb-brave.mjs serve </dev/null >/tmp/zb-brave-node.log 2>&1' _ "http://$broker:443/search"
guest sh -ec 'runc exec -d --env NODE_NO_WARNINGS=1 zb-workload node /tmp/zb-web-fetch.mjs serve /tmp/zb-approved-fetches.json </dev/null >/tmp/zb-fetch-node.log 2>&1'
guest runc exec zb-workload sh -ec 'for i in $(seq 120); do if test -S /tmp/zb-brave.sock && test -S /tmp/zb-web-fetch.sock; then exit 0; fi; sleep 1; done; exit 1'
