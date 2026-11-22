#!/usr/bin/env bash
# Same-worker diagnostics for snapshot-file fault-around and free-page reporting.
set -euo pipefail
stack=$(realpath "$1")
unit=$2
driver=$(realpath "$3")
series=$4
[[ "$series" =~ ^[a-zA-Z0-9-]+$ ]]
work="$stack/runtime"
replay="$stack/replay"
save="$stack/runs/$series"
mkdir -m 700 "$save"
cfg="$work/runtime.json"
test -z "$(pgrep -x firecracker || true)"
cp -p "$cfg" "$save/initial-runtime.json"
knob=/sys/kernel/debug/fault_around_bytes
original=$(cat "$knob")
restore() {
    cp -p "$save/initial-runtime.json" "$cfg.next"
    mv "$cfg.next" "$cfg"
    echo "$original" > "$knob"
}
trap restore EXIT
for pair in "v1 $original" "v2 $original" "v2 4096" "v1 4096"; do
    read -r variant around <<<"$pair"
    name="$series-$variant-around$around"
    if [[ -e "$stack/runs/$name" ]]; then echo "existing run $name" >&2; exit 1; fi
    python3 - "$cfg" "$variant" <<'PY'
import json,os,pathlib,sys
p=pathlib.Path(sys.argv[1]);v=json.loads(p.read_text())
v["free_page_reporting"]=sys.argv[2]=="v2"
tmp=p.with_suffix(".next");tmp.write_text(json.dumps(v,indent=2)+"\n");os.replace(tmp,p)
PY
    echo "$around" > "$knob"
    test "$(cat "$knob")" -eq "$around"
    export ZB_VMM_CENSUS=1 ZB_CHECKPOINT_NAMESPACE_DIR="$work/checkpoints/aries"
    "$replay/tracer/run-traced.sh" "$driver" "$replay/tracer" "$stack/runs/$name" \
        -trajectory "$replay/02a77c58.json" -rollout "$replay/02a77c58.jsonl" \
        -image docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74 \
        -namespace aries -socket "/run/$unit/containerd.sock" -checkpoint-dir "$work/checkpoints" \
        -shim-dir "$work/shim" -io-dir "/run/$unit/aries-io" -vcpus 1 -memory-mib 2048 \
        -storage-mib 10240 -ripgrep "$replay/rg"
    cp "$cfg" "$stack/runs/$name/effective-runtime.json"
    cat "$knob" > "$stack/runs/$name/fault-around-bytes"
done
