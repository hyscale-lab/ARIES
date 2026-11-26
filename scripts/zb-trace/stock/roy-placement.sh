#!/usr/bin/env bash
# In the stock service namespace, prepare two independent restores for a placement check.
set -euo pipefail
umask 077
b=/local/zb-study3-stock
cd "$b"
service=${1:?service}
out="$b/${2:?new placement directory}"
mkdir "$out"
export XDG_CONFIG_HOME="$b/$service/xdg" AENV_CLI="$b/cli-bundle/aenv"
export STOCK_BASE_IMAGE=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a
aenv() { "$AENV_CLI" "$@"; }
test "$(aenv list --output json | jq length)" = 0
cleanup() {
    local status=$?
    trap - EXIT
    if [[ -n "${monitor:-}" ]]; then kill "$monitor" || true; wait "$monitor" || true; fi
    for file in "$out"/{left,right}-prepared/{restored-id,original-id}; do
        if [[ -s "$file" ]] && aenv list --output json | jq -e --arg id "$(cat "$file")" 'any(.[]; .sandboxID==$id)' >/dev/null; then
            aenv delete "$(cat "$file")" || status=1
        fi
    done
    aenv list --output json >"$out/final-list.json" || status=1
    exit "$status"
}
trap cleanup EXIT
"$b/prepare-pilot.sh" roy 127.0.0.1:18762/library/roy@sha256:3ed43bff698735f15fa9db6e020df8e44e1d3ec37e8828ebc53eba054e509a77 "$b/runc" "$out/left-prepared"
left="$out/left-prepared"; right="$out/right-prepared"
mkdir "$right"
cp "$left"/{snapshot.txt,boot-id,guest-setup.txt} "$right"
date -u +%FT%T.%NZ >"$right/restore-start"
aenv start -d --timeout 3600 "prepared-$(cat "$left/original-id")" >"$right/restored-id"
date -u +%FT%T.%NZ >"$right/restore-end"
aenv exec "$(cat "$right/restored-id")" runc state zb-workload >"$right/restored-workload-state.json"
aenv exec "$(cat "$right/restored-id")" runc exec zb-workload cat /proc/sys/kernel/random/boot_id | cmp "$left/boot-id" -
aenv list --output json | jq '[.[]|{sandboxID,templateID,state}]' >"$out/placement.json"
jq -e 'length==2 and ([.[].sandboxID]|unique|length)==2 and ([.[].templateID]|unique|length)==1 and all(.[]; .state=="running")' "$out/placement.json" >/dev/null
python3 -u - <<'PY' >"$out/pressure.jsonl" &
from pathlib import Path
import json,time
deadline=time.monotonic()
while True:
    print(json.dumps(dict(ns=time.monotonic_ns(),cpu=Path('/proc/stat').read_text().splitlines()[0],
              pressure={k:Path('/proc/pressure',k).read_text() for k in ['cpu','io','memory']},
              diskstats=Path('/proc/diskstats').read_text(),meminfo=Path('/proc/meminfo').read_text())))
    deadline+=1
    time.sleep(max(0,deadline-time.monotonic()))
PY
monitor=$!
python3 -B "$b/scripts/zb-trace/stock/run-pilot.py" "$left" "$out/left" --service "$service" --deep >"$out/left-driver.log" 2>&1 &
first=$!
python3 -B "$b/scripts/zb-trace/stock/run-pilot.py" "$right" "$out/right" --service "$service" --deep >"$out/right-driver.log" 2>&1 &
second=$!
rc=0
wait "$first" || rc=1
wait "$second" || rc=1
((rc==0))
for arm in left right; do
    test "$(cut -d ' ' -f1 "$out/$arm/output.sha256")" = edbc25f87ba99b8b2fac6eaf74d80e55610b836c7d6e04c293ae4c90561801cc
done
