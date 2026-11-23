#!/usr/bin/env bash
# Start one study worker from the already built common kernel/root and stack binaries.
# Usage: provision_baseline.sh STACK_DIR UNIT RUNTIME_PROFILE v1|v2|v3|v4|native-v2 [IMAGE@sha256:DIGEST]
set -euo pipefail
stack=$(realpath "$1")
unit=$2
profile=$(realpath "$3")
variant=$4
[[ "$unit" =~ ^fcmod-zepbound-[a-z0-9-]+$ ]]
[[ "$variant" =~ ^(v[1-4]|native-v2)$ ]]
work="$stack/runtime"
bin="$work/bin"
socket="/run/$unit/containerd.sock"
image=${5:-docker.io/alexgshaw/fix-git@sha256:389b9c8247610c2c5be080b1ac00429007c2c69bf57f7f26c79f0f75ba2d5c74}
[[ "$image" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]
test -f "$work/common-rootfs.ext4" && test -x "$bin/containerd-shim-aws-firecracker"
test ! -e "$work/runtime.json"
if sudo -n systemctl is-active --quiet "$unit" || sudo -n dmsetup info "$unit" >/dev/null 2>&1; then
    echo "Unit or thin pool already exists: $unit" >&2
    exit 1
fi
mkdir -p "$work"/{thinpool,shim,checkpoints,devmapper} "$stack/evidence"
if ! sudo -n dmsetup info "$unit" >/dev/null 2>&1; then
    for entry in data:30G metadata:256M; do
        name=${entry%:*}
        if [[ ! -e "$work/thinpool/$name" ]]; then
            truncate -s "${entry#*:}" "$work/thinpool/$name"
        fi
    done
    sudo -n modprobe dm_thin_pool
    data=$(sudo -n losetup --noheadings --output NAME --associated "$work/thinpool/data")
    meta=$(sudo -n losetup --noheadings --output NAME --associated "$work/thinpool/metadata")
    if [[ -z "$data" ]]; then
        data=$(sudo -n losetup --find --show "$work/thinpool/data")
    fi
    if [[ -z "$meta" ]]; then
        meta=$(sudo -n losetup --find --show "$work/thinpool/metadata")
    fi
    sectors=$(sudo -n blockdev --getsz "$data")
    sudo -n dmsetup create "$unit" --table "0 $sectors thin-pool $meta $data 128 32768 1 skip_block_zeroing"
fi
sudo -n dmsetup table "$unit"

cat > "$work/containerd.toml" <<EOF
version = 4
root = "$work/root"
state = "/run/$unit"
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images", "io.containerd.cri.v1.runtime"]

[plugins."io.containerd.server.v1.grpc"]
  address = "$socket"
[plugins."io.containerd.server.v1.ttrpc"]
  address = "$socket.ttrpc"
[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "$unit"
  base_image_size = "10737418240"
  root_path = "$work/devmapper"
[[plugins."io.containerd.transfer.v1.local".unpack_config]]
  platform = "linux/amd64"
  snapshotter = "devmapper"
EOF

python3 - "$profile" "$work" "$variant" <<'PY'
import hashlib
import json
import pathlib
import sys

source, work = map(pathlib.Path, sys.argv[1:3])
config = json.loads(source.read_text())
variant = sys.argv[3]
if variant == "v2":
    config["free_page_reporting"] = True
    config["kernel_args"] += " page_reporting.page_reporting_order=9"
if variant in ("v3", "v4", "native-v2"):
    with (work / "common-rootfs.ext4").open("rb") as image:
        config["root_drive_sha256"] = hashlib.file_digest(image, "sha256").hexdigest()
    config["root_drive_use_pmem"] = variant == "v4"
    config["guest_reclamation"] = {"Enabled": variant != "native-v2"}
    if variant == "native-v2":
        config["kernel_args"] += " damon_reclaim.enabled=N page_reporting.page_reporting_order=9"
    config["smt_enabled"] = False
config.update(
    firecracker_binary_path=str(work / "bin/firecracker"),
    kernel_image_path=str(work / "common-vmlinux.bin"),
    root_drive=str(work / "common-rootfs.ext4"),
    shim_base_dir=str(work / "shim"),
    jailer={
        "runc_binary_path": str(work / "bin/runc"),
        "runc_config_path": str(work / "jailer.json"),
    },
)
(work / "runtime.json").write_text(json.dumps(config, indent=2) + "\n")
PY

"$bin/firecracker-containerd" --config "$work/containerd.toml" config dump |
    python3 -c 'import sys,tomllib; p=tomllib.loads(sys.stdin.read())["plugins"]; assert p["io.containerd.server.v1.grpc"]["address"]==sys.argv[1]; assert p["io.containerd.server.v1.ttrpc"]["address"]==sys.argv[1]+".ttrpc"; assert p["io.containerd.snapshotter.v1.devmapper"]["base_image_size"]=="10737418240"' "$socket"

if ! sudo -n systemctl is-active --quiet "$unit"; then
    sudo -n systemd-run --unit="$unit" --service-type=simple --property=Delegate=yes \
        --property=KillMode=process \
        --property="StandardOutput=append:$stack/evidence/$unit-runtime.log" \
        --property="StandardError=append:$stack/evidence/$unit-runtime.log" \
        --setenv="PATH=$bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
        --setenv="FIRECRACKER_CONTAINERD_RUNTIME_CONFIG_PATH=$work/runtime.json" \
        "$bin/firecracker-containerd" --config "$work/containerd.toml"
fi
ctr=(sudo -n "$bin/firecracker-ctr" --address "$socket" --namespace aries)
for attempt in $(seq 1 30); do
    if "${ctr[@]}" version; then break; fi
    sleep 1
done
test -S "$socket"
test -S "$socket.ttrpc"
"${ctr[@]}" plugins ls | awk '$1=="io.containerd.snapshotter.v1" && $2=="devmapper" && $NF=="ok" { good = 1 } END { exit !good }'
"${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$image"
"${ctr[@]}" images check --snapshotter devmapper
if [[ "$variant" == v1 || "$variant" == v2 ]]; then
    printf 'BASELINE_RUNTIME_READY socket=%s namespace=aries image=%s\n' "$socket" "$image"
else
    printf 'OPTIMIZED_RUNTIME_READY variant=%s socket=%s namespace=aries image=%s\n' "$variant" "$socket" "$image"
fi
