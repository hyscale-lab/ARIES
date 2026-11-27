#!/usr/bin/env bash
set -euo pipefail
task=$1 stage=$2 sandbox=$3 run=$4
here=$(dirname "$(realpath "$0")")
base=/local/zb-study3-stock
source "$here/supplemental-config.sh"
supplemental_config "$task" 6 ready "$base"
fixture="$base/local-sol/$task"
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
upload() { "$AENV_CLI" upload "$sandbox" "$1" "$2"; }
case "$task" in local-fact-check|local-comment-draft|fixture-file-overwrite) warmup=none;; resume-homepage) warmup=chromium;; *) warmup=npm;; esac
case "$task" in scholar-search|paper-affiliations|resume-homepage) source "$here/native-brave-fixture.sh";; esac
if [[ $stage == upload ]]; then
    (cd "$fixture" && sha256sum -c SHA256SUMS)
    upload "$fixture/inputs.tar" /tmp/supplemental-inputs.tar
    guest sh -ec 'tar --no-same-owner -C /workload/rootfs -xf /tmp/supplemental-inputs.tar; rm /tmp/supplemental-inputs.tar; chown 0:0 /workload/rootfs/tmp_workspace /workload/rootfs/tmp_workspace/.git'
    upload "$here/../../../cmd/aries-agentenv-replay/brave.mjs" /workload/rootfs/tmp/zb-brave.mjs
    upload "$here/web-fetch.mjs" /workload/rootfs/tmp/zb-web-fetch.mjs
    if [[ $warmup == npm ]]; then
        upload "$base/local-sol/npm.tar" /tmp/supplemental-npm.tar
        guest sh -ec 'mkdir /workload/rootfs/tmp/supplemental-npm; tar xf /tmp/supplemental-npm.tar -C /workload/rootfs/tmp/supplemental-npm; rm /tmp/supplemental-npm.tar'
    fi
    exit
fi
[[ $stage == ready ]]
guest runc exec zb-workload sh -ec 'test ! -e /tmp_workspace/results; test -z "${BRAVE_API_KEY:-}${GH_TOKEN:-}${GITHUB_TOKEN:-}${GITHUB_PAT:-}"; git -C /tmp_workspace rev-parse --is-inside-work-tree'
case "$task" in
    fixture-file-overwrite) guest runc exec zb-workload sh -ec 'test -s /tmp_workspace/ResNet.pdf && test -s /tmp_workspace/summary.md && test ! -e /tmp_workspace/MAE.pdf';;
    local-fact-check) guest runc exec zb-workload test ! -e /tmp_workspace/post.md;;
    local-comment-draft) guest runc exec zb-workload test ! -e /tmp_workspace/comment.txt;;
esac
if [[ $warmup == npm ]]; then
    cutoff=$(head -n1 "$trace" | jq -er .timestamp)
    guest runc exec --env npm_config_cache=/tmp/supplemental-npm --env npm_config_offline=true --env npm_config_before="$cutoff" zb-workload bash /tmp_workspace/.wildclaw/run-warmup.sh
    guest runc exec zb-workload sh -ec 'test -f /tmp/wildclaw-harbor-warmup.done; agent-browser --version | grep -F 0.27.0; rm -r /tmp/supplemental-npm'
elif [[ $warmup == chromium ]]; then
    guest runc exec zb-workload bash /tmp_workspace/.wildclaw/run-warmup.sh
fi
broker=$(awk '$2=="brave-broker"{print $1}' "$run/hof-endpoints.txt")
[[ $broker == 192.0.2.* ]]
guest sh -ec 'printf "%s api.search.brave.com\n" "$1" >> /workload/rootfs/etc/hosts' _ "${ZB_BRAVE_PUBLIC_IP:?}"
guest runc exec zb-workload sh -ec 'test "$(curl --max-time 10 -s -o /dev/null -w "%{http_code}" --data "{}" "$1")" = 403' _ "http://$broker:443/search"
guest sh -ec 'runc exec -d --env ZB_BRAVE_BROKER_URL="$1" --env NODE_NO_WARNINGS=1 zb-workload node /tmp/zb-brave.mjs serve </dev/null >/tmp/zb-brave-node.log 2>&1' _ "http://$broker:443/search"
if guest runc exec zb-workload test -f /tmp/zb-approved-fetches.json; then
    guest sh -ec 'runc exec -d --env NODE_NO_WARNINGS=1 zb-workload node /tmp/zb-web-fetch.mjs serve /tmp/zb-approved-fetches.json </dev/null >/tmp/zb-fetch-node.log 2>&1'
fi
guest runc exec zb-workload sh -ec 'for n in $(seq 100); do if test -S /tmp/zb-brave.sock && { test ! -e /tmp/zb-approved-fetches.json || test -S /tmp/zb-web-fetch.sock; }; then exit 0; fi; sleep 1; done; exit 1'
