#!/usr/bin/env bash
# Run as the worker operator with KVM tracing and page-idle privileges.
set -euo pipefail
if (( $# < 4 )); then
    echo "usage: run-traced.sh ARIES_HARBOR TRACE_SCRIPTS NEW_RUN_DIRECTORY [aries-harbor options...]" >&2
    exit 2
fi
binary=$(realpath "$1")
scripts=$(realpath "$2")
run_dir=$(realpath -m "$3")
shift 3
umask 077
mkdir "$run_dir"
export ZB_SLOT_EVENTS="$run_dir/slots.log"
export ZB_TRACE_OUT="$run_dir/idle.jsonl"
bpftrace -q -B line "$scripts/kvm-slots.bt" >"$ZB_SLOT_EVENTS" 2>"$run_dir/bpf.stderr" &
capture=$!
finish() {
    if kill -0 "$capture" 2>/dev/null; then
        kill -INT "$capture"
    fi
    wait "$capture" || true
}
trap finish EXIT
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$ZB_SLOT_EVENTS"; then
        break
    fi
    if ! kill -0 "$capture" 2>/dev/null; then
        cat "$run_dir/bpf.stderr" >&2
        exit 1
    fi
    sleep 0.1
done
grep -q '^ready ' "$ZB_SLOT_EVENTS" || { echo 'KVM registration observer did not become ready' >&2; exit 1; }
"$binary" "$@" -observer "$scripts/idle_hook.py" -sampler "$scripts/sample_vmm.py" \
    -log "$run_dir/events.jsonl" -samples "$run_dir/samples.jsonl" \
    2> >(tee "$run_dir/replay.stderr" >&2)
