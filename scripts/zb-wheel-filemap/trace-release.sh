#!/usr/bin/env bash
# Collect KVM registration and the stock Firecracker report handler during the fixed control.
set -euo pipefail
stage=$1
assets=$2
[[ $3 == -- ]]
shift 3
export ZB_SLOT_EVENTS="$stage/slots.log"
export ZB_RELEASE_TRANSPORT="$stage/guest_sample.py"
export ZB_RELEASE_HANDLER_EVENTS="$stage/handler/events"
export BPFTRACE_PERF_RB_PAGES=1024
bpftrace -q -B line "$assets/tracer/kvm-slots.bt" > "$ZB_SLOT_EVENTS" 2> "$stage/slots.stderr" &
slots=$!
finish_slots() {
    if kill -0 "$slots" 2>/dev/null; then kill -INT "$slots"; fi
    wait "$slots"
}
trap finish_slots EXIT
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$ZB_SLOT_EVENTS"; then break; fi
    kill -0 "$slots"
    sleep .1
done
grep -q '^ready ' "$ZB_SLOT_EVENTS"
python3 -B "$stage/watch-report-replacements.py" "$stage" "$stage/handler" \
    "$stage/reported-replacements.bt" --samples "$stage/samples.jsonl" > "$stage/handler-wrapper.stderr" 2>&1 &
watch=$!
rc=0
"$@" || rc=$?
if [[ ! -s "$stage/samples.jsonl" ]]; then kill -TERM "$watch" 2>/dev/null || true; fi
wrc=0
wait "$watch" || wrc=$?
finish_slots
trap - EXIT
printf '%s %s\n' "$rc" "$wrc" > "$stage/release-tracer.exit"
[[ "$rc $wrc" == '0 0' ]]
