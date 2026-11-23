#!/usr/bin/env bash
# Trace the original Git trial on the audited native PMEM maps deployment.
set -euo pipefail
work=$(realpath "$1"); assets=$(realpath "$2"); tracer=$(realpath "$3")
probe=$(realpath "$4"); out=$(realpath -m "$5")
unit=fcmod-zepbound-filemapsv4
image=docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74
[[ ! -e $out && -S /run/$unit/containerd.sock ]]
if pgrep -x 'firecracker|[.]firecracker-re'; then exit 2; fi
(cd "$assets" && sha256sum -c - <<'SOURCE'
ec22ca5398af7061a1b51261a7abefdb3775e3422682232fccdaa8494dd22a4f  02a77c58.json
8193dbe7464cf2822e736a524689e69c18ca905db0703c8fc0ee2b716d12e818  02a77c58.jsonl
bfbe5dd366a0ac2c720e701aa5bf1aa718b51c16a8688727d2ccd8a11d1784be  aries-d1cba28
e62198eb19b136b88c330af83647b5a962cb99b6b1f066758568f12de1974849  rg
SOURCE
)
mkdir -m 0700 "$out"
sha256sum "$0" "$work"/{bin/firecracker,bin/firecracker-containerd,bin/containerd-shim-aws-firecracker,common-vmlinux.bin,common-rootfs.ext4,runtime.json} \
    "$assets"/{aries-d1cba28,02a77c58.json,02a77c58.jsonl,rg} "$tracer"/{idle_hook.py,idle.py,run-traced.sh,kvm-slots.bt,sample_vmm.py,vmm_census.py} "$probe" > "$out/inputs.sha256"
cp "$work/runtime.json" "$out/runtime.json"
cat /proc/sys/kernel/random/boot_id > "$out/boot-id"
bpftrace --version > "$out/bpftrace-version"
[[ $(sha256sum "$probe" | cut -d' ' -f1) == 5ba3653cb6fec845eb7206827333a846b58441c1a5e88c302605b5a3a37dc555 ]]
"$work/bin/firecracker-ctr" -a /run/$unit/containerd.sock -n aries images check --snapshotter devmapper | grep -F "$image" > "$out/oci-image.txt"
if pgrep -x 'firecracker|[.]firecracker-re'; then exit 2; fi
bpftrace -q -B line "$probe" > "$out/demand.events" 2> "$out/demand.stderr" &
pid=$!
finish() { if kill -0 "$pid" 2>/dev/null; then kill -INT "$pid"; fi; wait "$pid" || true; }
trap finish EXIT
for _ in $(seq 1 150); do
    if grep -q '^ready ' "$out/demand.events"; then break; fi
    kill -0 "$pid"; sleep 0.1
done
grep -q '^ready ' "$out/demand.events"
export ZB_CHECKPOINT_NAMESPACE_DIR="$work/checkpoints/aries" ZB_VMM_CENSUS=1
rc=0
bash "$tracer/run-traced.sh" "$assets/aries-d1cba28" "$tracer" "$out/trace" -native-checkpoint \
    -trajectory "$assets/02a77c58.json" -rollout "$assets/02a77c58.jsonl" -image "$image" -ripgrep "$assets/rg" \
    -namespace aries -socket /run/$unit/containerd.sock -checkpoint-dir "$work/checkpoints" -shim-dir "$work/shim" \
    -io-dir /run/$unit/aries-io || rc=$?
finish
trap - EXIT
printf '%s\n' "$rc" > "$out/driver.exit"
shopt -s dotglob
mv "$out/trace/"* "$out/"
rmdir "$out/trace"
exit "$rc"
