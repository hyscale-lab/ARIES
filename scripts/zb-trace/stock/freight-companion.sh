#!/usr/bin/env bash
# The event feed runs in the same VM with its original image, a separate root and its own cgroup.
set -euo pipefail
action=${1:?action}
run=${2:?prepared directory}
b=/local/zb-study3-stock
s="$b/scripts/zb-trace/stock"
digest=sha256:f5975c403f3d3edc43e926ac49a63ea8561f9731ea63366180424380ad952782
image="127.0.0.1:18762/library/event-feed@$digest"
case "$action" in
image)
    fixture="$run/event-feed"
    mkdir "$fixture"
    jq -n --arg digest "$digest" '{task:"event-feed",image_digest:$digest}' >"$fixture/metadata.json"
    reg="$b/server-bundle/deps/regctl/v0.11.5/regctl"
    if [[ "$("$reg" image digest "$image" 2>/dev/null || true)" != "$digest" ]]; then
        python3 "$s/stock-store-archive.py" "$TB41_IMAGE_STORE" "$digest" "$fixture/image.tar"
        python3 "$s/tb41-image.py" "$fixture/image.tar" "$fixture" "$reg" 127.0.0.1:18762 >&2
        rm "$fixture/image.tar"
    fi
    "$reg" image config "$image" --format raw-body >"$fixture/config.json"
    echo "$image"
    ;;
start)
    sandbox=${3:?sandbox}
    pid=$(jq -er '.pid | select(. > 0)' "$run/workload-state.json")
    jq -e --argjson pid "$pid" --slurpfile cfg "$run/event-feed/config.json" '
       $cfg[0].config as $c |
       select(($c.Entrypoint == null) and ($c.Cmd == ["python3", "/srv/app/server.py"])) |
       .process.args=$c.Cmd | .process.cwd="/" | .process.env=$c.Env |
       .linux.cgroupsPath="/zb-event-feed" |
       (.linux.namespaces[] | select(.type=="network")).path="/proc/\($pid)/ns/net"
    ' "$run/config.json" >"$run/event-feed-spec.json"
    "$AENV_CLI" upload "$sandbox" "$run/event-feed-spec.json" /event-feed/config.json
    "$AENV_CLI" exec "$sandbox" sh -ec 'runc run -d --bundle /event-feed zb-event-feed </dev/null >/tmp/zb-event-feed-start.log 2>&1'
    "$AENV_CLI" exec "$sandbox" runc state zb-event-feed >"$run/event-feed-state.json"
    jq -e '.status=="running"' "$run/event-feed-state.json" >/dev/null
    ;;
*) exit 2;;
esac
