#!/usr/bin/env bash
set -euo pipefail
stage=$1
assets=$2
unit=$3
image=$4
runtime="$stage/runtime"
export ZB_TEX_SCHED_OUTPUT="$stage"
export PYTHONPATH="$assets/tracer" PYTHONDONTWRITEBYTECODE=1
export BPFTRACE_PERF_RB_PAGES=1024
bpftrace --version > "$stage/bpftrace-version.txt"
tracers=()
finish() {
    local pid
    for pid in "${tracers[@]}"; do
        if kill -0 "$pid" 2>/dev/null; then kill -INT "$pid"; fi
        wait "$pid"
    done
}
trap finish EXIT
bpftrace -q -B line "$stage/kvm-demand.bt" > "$stage/demand.events" 2> "$stage/demand.stderr" &
tracers+=("$!")
bpftrace -q -B line "$assets/tracer/kvm-slots.bt" > "$stage/slots.log" 2> "$stage/slots.stderr" &
tracers+=("$!")
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$stage/demand.events" && grep -q '^ready ' "$stage/slots.log"; then break; fi
    kill -0 "${tracers[@]}"
    sleep .1
done
grep -q '^ready ' "$stage/demand.events"
grep -q '^ready ' "$stage/slots.log"
"$stage/clock-adapter" -log "$stage/events.jsonl" -observer "$stage/observe.py" \
    -trajectory "$assets/trajectory.json" -rollout "$assets/rollout.jsonl" \
    -codex-tools "$assets/codex-harbor-tools.tar.gz" -os-tools "$assets/overfull-hbox-debs.tar.gz" \
    -image "$image" -namespace aries -socket "/run/$unit/containerd.sock" \
    -checkpoint-dir "$runtime/checkpoints" -shim-dir "$runtime/shim" -io-dir "/run/$unit/aries-io" \
    -vcpus 2 -memory-mib 4096 -storage-mib 10240 -native-checkpoint -guest-task-v3 \
    > "$stage/replay.stdout" 2> "$stage/replay.stderr"
