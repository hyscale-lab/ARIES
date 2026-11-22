#!/usr/bin/env bash
# Drive the standalone fixture on an already provisioned diagnostic worker.
set -euo pipefail
stack=$(realpath "$1")
unit=$2
label=$3
variant=$4
bpf=$(realpath "${5:-/usr/bin/bpftrace}")
libs=${6:-}
[[ "$unit" =~ ^fcmod-zepbound-[a-z0-9-]+$ && "$label" =~ ^[a-z0-9-]+$ && "$variant" =~ ^(v1|v4)$ ]]
run="$stack/evidence/$label"
mkdir -m 700 "$run"
mkdir -m 700 "$run/io"
scripts="$stack/tools/scripts"
sudo -n sh -c 'echo "$$" > "$1"; exec env "$2" "$3" -q -B line "$4"' sh     "$run/slots.pid" "LD_LIBRARY_PATH=$libs" "$bpf" "$scripts/zb-trace/kvm-slots.bt"     > "$run/slots.events" 2> "$run/slots.stderr" &
pid=$!
cleanup() {
    if [[ -s "$run/slots.pid" ]]; then
        sudo -n kill -INT "$(cat "$run/slots.pid")" 2>/dev/null || true
    fi
    wait "$pid" || true
}
trap cleanup EXIT
for attempt in {1..80}; do
    if grep -q '^ready ' "$run/slots.events"; then break; fi
    if ! kill -0 "$pid" 2>/dev/null; then cat "$run/slots.stderr"; exit 1; fi
    sleep 0.1
done
grep -q '^ready ' "$run/slots.events"
sleep 0.2
native=()
if [[ "$variant" == v4 ]]; then native=(-native-checkpoint); fi
set +e
sudo -n env "ZB_SLOT_EVENTS=$run/slots.events"     "ZB_CHECKPOINT_NAMESPACE_DIR=$stack/runtime/checkpoints/aries"     "$stack/tools/zb-filecal-driver" "${native[@]}"     -socket "/run/$unit/containerd.sock" -namespace aries     -shim-dir "$stack/runtime/shim" -checkpoint-dir "$stack/runtime/checkpoints" -io-dir "$run/io"     -image docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74     -hook "$scripts/zb-demand/filecal/observe.py" > "$run/filecal.jsonl" 2> "$run/driver.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$run/driver.status"
cat "$run/driver.stderr"
exit "$status"
