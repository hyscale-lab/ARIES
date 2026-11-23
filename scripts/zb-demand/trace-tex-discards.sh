#!/usr/bin/env bash
# Capture VMM fixed mapping replacement ranges alongside a complete TeX demand trace.
set -euo pipefail
collector=$(realpath "$1")
discard_probe=$(realpath "$2")
shift 2
evidence=$(realpath -m "$6")-discards
mkdir -m 0700 "$evidence"
sha256sum "$collector" "$discard_probe" > "$evidence/inputs.sha256"
bpftrace -q -B line "$discard_probe" > "$evidence/fixed.events" 2> "$evidence/fixed.stderr" &
pid=$!
stop() {
    if kill -0 "$pid" 2>/dev/null; then kill -INT "$pid"; fi
    wait "$pid" || true
}
trap stop EXIT
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$evidence/fixed.events"; then break; fi
    kill -0 "$pid"
    sleep 0.1
done
grep -q '^ready ' "$evidence/fixed.events"
bash "$collector" "$@"
kill -0 "$pid"
stop
trap - EXIT
test ! -s "$evidence/fixed.stderr"
echo 0 > "$evidence/driver.exit"
