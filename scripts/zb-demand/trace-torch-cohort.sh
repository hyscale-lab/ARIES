#!/usr/bin/env bash
# Trace the original 4b588047 native block or DAX replay with both stock probes.
set -euo pipefail
src=$(realpath "$1")
rt=$(realpath "$2")
unit=$3
out=$(realpath -m "$4")
tools=$(realpath "$5")
variant=${6:-v3}
[[ "$variant" == v3 || "$variant" == v4 ]]
test ! -e "$out"
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 2; fi
systemctl is-active --quiet "$unit"
(cd "$src"; echo '6f50c5bd8fa791b90e600d487842668baf38e1e1835d68c3cc55426fa9dd06f2  SHA256SUMS' | sha256sum -c -; sha256sum --quiet -c SHA256SUMS)
mkdir -p "$out/bundle/runs"
for f in "$src"/*; do
    case ${f##*/} in runs|__pycache__|pending.json|input-check.log|provision.log) continue;; esac
    ln -s "$f" "$out/bundle/"
done
run="$out/bundle/runs/4b588047-$variant-cohort-01"
sha256sum "$rt"/{bin/firecracker,common-vmlinux.bin,common-rootfs.ext4,runtime.json} "$src"/{SHA256SUMS,run-reviewed-cohort.py,manifest.json} "$tools"/{kvm-demand.bt,reported-replacements.bt,watch-report-replacements.py,trace-torch-cohort.sh} > "$out/inputs.sha256"
cp "$rt/runtime.json" "$out"
cat /proc/sys/kernel/random/boot_id > "$out/boot-id"
export BPFTRACE_PERF_RB_PAGES=1024
bpftrace -q -B line "$tools/kvm-demand.bt" > "$out/demand.events" 2> "$out/demand.stderr" &
tracer=$!
end_trace() {
    if kill -0 "$tracer" 2>/dev/null; then kill -INT "$tracer"; fi
    wait "$tracer"
}
trap end_trace EXIT
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$out/demand.events"; then break; fi
    kill -0 "$tracer"; sleep .1
done
grep -q '^ready ' "$out/demand.events"
python3 "$tools/watch-report-replacements.py" "$run" "$out/handler" "$tools/reported-replacements.bt" --samples "$run/samples.jsonl" &
watch=$!
replay=(python3 "$src/run-reviewed-cohort.py" "$rt" "$unit" "$out/bundle" "$variant" "$src/manifest.json" --torch-cni "$src/cni")
if [[ "$variant" == v4 ]]; then
    sha256sum "$tools"/{observe-balloon.py,vmm_exclusivity.py} >> "$out/inputs.sha256"
    hostname -s > "$out/worker"
    python3 "$tools/observe-balloon.py" "$rt/shim" "$out/balloon-series.jsonl" --restored-run "$run" --vm-config "$out/effective-vmm.json" &
    balloon=$!
    replay=(python3 "$tools/vmm_exclusivity.py" "$out/placement-monitor.jsonl" -- "${replay[@]}")
fi
rc=0
"${replay[@]}" || rc=$?
end_trace
trap - EXIT
if (( rc != 0 )) && [[ ! -s "$run/samples.jsonl" ]]; then
    kill -TERM "$watch" ${balloon:+"$balloon"} 2>/dev/null || true
fi
wrc=0
wait "$watch" || wrc=$?
echo "$rc $wrc" > "$out/combined.exit"
if [[ "$variant" == v4 ]]; then
    brc=0
    wait "$balloon" || brc=$?
    echo "$brc" > "$out/balloon.exit"
    test "$brc" = 0
fi
test "$rc $wrc" = '0 0'
