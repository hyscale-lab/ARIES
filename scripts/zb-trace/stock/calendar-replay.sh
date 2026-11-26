#!/usr/bin/env bash
# Run inside the stock service network namespace after prepare-pilot.sh.
set -euo pipefail
umask 077
if (($# < 4)); then
    echo 'usage: calendar-replay.sh PREPARED_DIR NEW_OUTPUT_DIR REPLAY_BINARY TRACE [REPLAY_FLAGS]' >&2
    exit 2
fi
: "${AENV_CLI:?}" "${XDG_CONFIG_HOME:?}"
prepared=$1
out=$2
replay=$3
trace=$4
shift 4
mkdir "$out"
sandbox=$(cat "$prepared/restored-id")
url=http://127.0.0.1:18763
token=$(python3 - "$XDG_CONFIG_HOME/aenv/credentials" "$url" "$sandbox" <<'PY'
import json
import sys
import tomllib
from urllib.request import Request, urlopen

path, url, sandbox = sys.argv[1:]
with open(path, "rb") as source:
    credentials = tomllib.load(source)
assert credentials["url"] == url
request = Request(f"{url}/sandboxes/{sandbox}", headers={"X-API-Key": credentials["api_key"]})
with urlopen(request, timeout=30) as result:
    detail = json.load(result)
assert detail["sandboxID"] == sandbox
print(detail.get("envdAccessToken") or "")
PY
)

ENVD_ACCESS_TOKEN="$token" "$replay" -url "$url" -sandbox "$sandbox" \
    -runtime runc -container zb-workload -calendar "$trace" "$@" \
    >"$out/replay.jsonl" 2>"$out/replay.stderr"
unset token

"$AENV_CLI" exec "$sandbox" runc exec --cwd /tmp_workspace zb-workload sh -ec '
    sha256sum results/decision_log.md results/scheduled.ics results/unscheduled.json
' >"$out/results.sha256"
"$AENV_CLI" exec "$sandbox" runc exec --cwd /tmp_workspace zb-workload git log -1 \
    --format='%H%n%s' >"$out/git-head.txt"
diff -u - "$out/results.sha256" <<'HASHES'
4d15946f45c9be84705eaef930d8490ddbae0b1bba2ff2aca6aa0d214debe5a1  results/decision_log.md
d389dc81871c9ebbf2b16ed0e6d7f30b1928e750cd4cd9521c843281d5154d80  results/scheduled.ics
1f14be687f308fdcfcda1534893944dd03f808687f24eaacb941d752525bdb6e  results/unscheduled.json
HASHES
