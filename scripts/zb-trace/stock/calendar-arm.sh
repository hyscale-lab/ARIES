#!/usr/bin/env bash
# Run one Calendar reporting-order pilot on an exclusively assigned amd191.
set -euo pipefail
umask 077
if (($# != 1)); then echo 'usage: calendar-arm.sh 9|1|pack' >&2; exit 2; fi
base=/local/zb-study3-stock
smoke="$base/smoke.sh"
tools="$base/calendar-tools"
prepare="$base/prepare-pilot-calendar-v2.sh"
options=(--deep)
store=
case "$1" in
    9|1)
        order=$1
        service="cal-o$order"
        config="$base/order$order.toml"
        ;;
    pack)
        order=6
        service=cal-cr
        config="$base/calendar-pack.toml"
        smoke="$base/calendar-smoke-pack.sh"
        tools="$base/calendar-pack-tools"
        prepare="$base/prepare-pilot-calendar-pack.sh"
        store="$base/$service/home/snapshot-store/repository"
        options+=(--checkpoint)
        ;;
    *) exit 2 ;;
esac
run="$base/$service-run"
work="$base/$service"
trace="$base/01_Productivity_Flow_task_6_calendar_scheduling.jsonl"
cache="$base/live/home/image-cache"
mkdir "$run"
python3 - "$config" "$cache" >"$run/server.toml" <<'PY'
import sys
from pathlib import Path

config, cache = sys.argv[1:]
body = Path(config).read_text()
assert body.count("[image.cache]\n") == 1
print(body.replace("[image.cache]\n", f'[image.cache]\nroot_dir = "{cache}"\n'), end="")
PY
unshare --mount --net --propagation private /bin/bash "$smoke" "$service" serve "$run/server.toml" \
    >"$run/service.log" 2>&1 &
launcher=$!
pid=
sandbox=
in_stock() { nsenter -t "$pid" -m -n -- "$@"; }
aenv() { in_stock env XDG_CONFIG_HOME="$work/xdg" "$base/cli-bundle/aenv" "$@"; }
active_vmms() {
    ps --ppid "$pid" -o pid=,comm= | while read -r child name; do
        if [[ "$name" == firecracker ]] && ls -l "/proc/$child/fd" | grep 'anon_inode:kvm-vcpu' >/dev/null; then
            printf '%s\n' "$child"
        fi
    done
}
cleanup() {
    local status=$?
    trap - EXIT
    if [[ -n "$pid" ]]; then
        local ids id
        ids=$(aenv list --output json | python3 -c 'import json,sys; print(*(x["sandboxID"] for x in json.load(sys.stdin)),sep="\n")') || status=1
        while IFS= read -r id; do
            if [[ -n "$id" ]]; then
                aenv delete "$id" >>"$run/cleanup.log" 2>&1 || status=1
            fi
        done <<<"$ids"
        aenv list --output json >"$run/list-after-delete.json" || status=1
        python3 -c 'import json,sys; assert json.load(sys.stdin) == []' <"$run/list-after-delete.json" || status=1
        [[ -z "$(active_vmms)" ]] || status=1
        kill -TERM "$pid" 2>/dev/null || status=1
    else
        kill -TERM "$launcher" 2>/dev/null || true
    fi
    wait "$launcher" || true
    exit "$status"
}
trap cleanup EXIT
for _ in {1..60}; do
    [[ ! -s "$work/server.pid" ]] || break
    kill -0 "$launcher"
    sleep 1
done
pid=$(cat "$work/server.pid")

digest=$(in_stock env REGCTL_CONFIG="$base/registry/regctl.json" \
    "$base/server-bundle/deps/regctl/v0.11.5/regctl" image digest 127.0.0.1:18762/library/wildclaw:v13)
[[ "$digest" == sha256:60eec8752cb597e180780ff08d7569c1892c169521f1f2b069c2efeb006a4078 ]]
printf '%s\n' "$digest" >"$run/image-digest"
in_stock env STOCK_PACK_STORE="$store" XDG_CONFIG_HOME="$work/xdg" AENV_CLI="$base/calendar-aenv-v2.py" STOCK_AENV_CLI="$base/cli-bundle/aenv" \
    STOCK_BASE_IMAGE=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a \
    CALENDAR_INPUTS="$base/environment" "$prepare" \
    calendar "127.0.0.1:18762/library/wildclaw@$digest" "$base/runc" "$run/prepared" >"$run/prepare.log" 2>&1

sandbox=$(cat "$run/prepared/restored-id")
snapshot=$(awk '/^Created snapshot / { print $3 }' "$run/prepared/snapshot.txt")
cmp "$run/prepared/"{workload-state,restored-workload-state}.json
aenv exec "$sandbox" sh -ec '
    cat /sys/module/page_reporting/parameters/page_reporting_order
    cat /sys/module/damon_reclaim/parameters/enabled
    blockdev --getsize64 /dev/vda
    blockdev --getsize64 "$(findmnt -n -o SOURCE /workload/rootfs)"
' >"$run/guest-restored-probe.txt"
diff -u - "$run/guest-restored-probe.txt" < <(printf '%s\nY\n68719476736\n68719476736\n' "$order")
aenv list --output json >"$run/list-before-replay.json"
python3 - "$run/list-before-replay.json" "$sandbox" "$snapshot" <<'PY'
import json, sys
rows = json.load(open(sys.argv[1]))
assert len(rows) == 1 and rows[0]["sandboxID"] == sys.argv[2] and rows[0]["templateID"] == sys.argv[3], rows
PY
mapfile -t candidates < <(active_vmms)
[[ "${#candidates[@]}" == 1 ]]
vm=${candidates[0]}
socket=$(tr '\0' '\n' <"/proc/$vm/cmdline" | awk 'seen { print; exit } $0=="--api-sock" { seen=1 }')
printf '%s\n' "$vm" >"$run/vmm.pid"
printf '%s\n' "$socket" >"$run/vmm.socket"
curl -fsS --unix-socket "$socket" http://localhost/ >"$run/firecracker-info.json"
sha256sum "$base"/{native-deep-scan,zb-guest,aries-agentenv-replay} \
    "$tools"/stock/{run-pilot,deep,host_sample}.py >"$run/binary-and-observer-sha256.txt"
awk '/^(MemAvailable|Buffers|Cached|SwapCached|Dirty|Writeback):/' /proc/meminfo >"$run/meminfo-before.txt"
cat /proc/diskstats >"$run/diskstats-before.txt"
in_stock python3 "$tools/stock/run-pilot.py" "$run/prepared" "$run/traced" \
    --service "$service" --calendar "$trace" --socket "$socket" "${options[@]}" >"$run/traced.stdout" 2>"$run/traced.stderr"
cat /proc/diskstats >"$run/diskstats-after.txt"
awk '/^(MemAvailable|Buffers|Cached|SwapCached|Dirty|Writeback):/' /proc/meminfo >"$run/meminfo-after.txt"
python3 "$base/calendar-check.py" "$trace" "$run/traced/events.jsonl" >"$run/traced/validation.json"
sed 's#  /tmp_workspace/#  #' "$run/traced/output.sha256" | diff -u "$base/calendar-03/replay-light/results.sha256" -
