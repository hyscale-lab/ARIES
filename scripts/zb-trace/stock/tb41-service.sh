#!/usr/bin/env bash
# One exclusive worker, a new service and prepared sandbox for each arm.
set -euo pipefail
umask 077
task=${1:?workload}; arm=${2:?6, 9, 1 or pack}; label=${3:?unique service label}; expected=${4:?artifact SHA}
: "${TB41_IMAGE_STORE:?}" "${TB41_CODEX_ARCHIVE:?}"
[[ "$task" =~ ^[a-z0-9-]+$ && "$label" =~ ^[a-zA-Z0-9-]+$ ]]
b=/local/zb-study3-stock
cd "$b"
cfg="$label.toml"
test ! -e "$cfg"
test ! -e "$label"
case "$arm" in
    1|6|9) sed "s/page_reporting_order=6/page_reporting_order=$arm/" server-bundle/default.toml >"$cfg";;
    pack) cp pack.toml "$cfg";;
    *) exit 2;;
esac
if [[ "$task" == freight-dispatch-shift || "$task" == hof-topology-interpenetration ]]; then
    fixture_helper=freight-companion.sh
    if [[ "$task" == hof-topology-interpenetration ]]; then fixture_helper=hof-network.sh; fi
    sha256sum "$cfg" "$task"/{trajectory,rollout,metadata.json} server-bundle/server \
        server-bundle/deps/firecracker/1.15.1-patch-v2/firecracker cli-bundle/aenv runc smoke.sh \
        aries-agentenv-replay native-deep-scan zb-guest prepare-pilot.sh tb41-{ready,arm}.sh \
        scripts/zb-trace/stock/"$fixture_helper" scripts/zb-trace/stock/{tb41-service.sh,run-pilot.py,deep.py,checkpoint.py,host_sample.py,guest-ticks.py} \
        >"$label.deployment-at-launch.txt"
fi
digest=$(jq -er .image_digest "$task/metadata.json")
if [[ ! -e "$task.tar" ]]; then
    python3 -B scripts/zb-trace/stock/stock-store-archive.py "$TB41_IMAGE_STORE" "$digest" "$task.tar"
fi
unshare --mount --net --propagation private /bin/bash "$b/smoke.sh" "$label" serve "$b/$cfg" >"$label.service.log" 2>&1 &
launcher=$!
pid=
hof_proxies=()
end_service() {
    if [[ -n "$pid" && "$(readlink "/proc/$pid/exe")" == "$b/server-bundle/server" ]]; then kill -TERM "$pid"; fi
    for proxy in "${hof_proxies[@]}"; do
        if [[ "$(ps -o ppid= -p "$proxy" | tr -d ' ')" == "$$" ]]; then kill -TERM "$proxy" || true; fi
    done
    wait "$launcher" || true
}
trap end_service EXIT
for _ in $(seq 60); do
    [[ ! -s "$label/server.pid" ]] || break
    kill -0 "$launcher"
    sleep 1
done
pid=$(cat "$label/server.pid")
ns=(nsenter -t "$pid" -n -m --)
if [[ "$task" == hof-topology-interpenetration || "$task" == nextjs-performance ]]; then
    source "$b/scripts/zb-trace/stock/hof-network.sh"
    if [[ "$task" == nextjs-performance ]]; then hof_hosts=("${nextjs_hosts[@]}"); fi
    hof_start_endpoints "$label" "$pid"
fi
reg="$b/server-bundle/deps/regctl/v0.11.5/regctl"
export REGCTL_CONFIG="$b/registry/regctl.json"
platform=$digest
if [[ -s "$task/image-platform.digest" ]]; then platform=$(cat "$task/image-platform.digest"); fi
if [[ ! -e "$task/image-env.json" || "$("${ns[@]}" "$reg" image digest "127.0.0.1:18762/library/$task:reviewed" 2>/dev/null || true)" != "$platform" ]]; then
    "${ns[@]}" python3 -B "$b/scripts/zb-trace/stock/tb41-image.py" "$b/$task.tar" "$b/$task" "$reg" 127.0.0.1:18762
fi
cat /proc/diskstats >"$label.diskstats-before"
cat /proc/meminfo >"$label.meminfo-before"
extra=()
[[ "$arm" != pack ]] || extra+=(pack)
"${ns[@]}" bash "$b/tb41-arm.sh" "$task" "$label" "$label" "$expected" "${extra[@]}"
cat /proc/diskstats >"$label.diskstats-after"
cat /proc/meminfo >"$label.meminfo-after"
actual=$arm
[[ "$arm" != pack ]] || actual=6
tail -n 3 "$label-prepared/guest-setup.txt" | head -n 2 | diff -u - <(printf '%s\nY\n' "$actual")
