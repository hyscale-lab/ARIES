#!/usr/bin/env bash
set -euo pipefail
test "$(cat /var/emulab/boot/nodeid)" = amd183
stage=/local/zb-tb41-medical-wave1
donor=/local/zb-v5-matched-deep/runtime
runtime="$stage/runtime"
unit=fcmod-zepbound-amd183-medical-wave1
donor_unit=fcmod-zepbound-amd183-v5-deep
socket="/run/$unit/containerd.sock"

mkdir -m 0700 "$runtime"
ln -s "$donor/bin" "$runtime/bin"
ln -s "$donor/common-vmlinux.bin" "$runtime/common-vmlinux.bin"
ln -s "$donor/jailer.json" "$runtime/jailer.json"
mkdir "$runtime"/{thinpool,shim,checkpoints,devmapper}
sed "s,$donor,$runtime,g" "$donor/runtime.json" > "$runtime/runtime.json"
sed -e "s,$donor,$runtime,g" -e "s,$donor_unit,$unit,g" -e 's,10737418240,21474836480,g' "$donor/containerd.toml" > "$runtime/containerd.toml"
python3 - "$runtime/runtime.json" "$runtime/containerd.toml" <<'PY'
import json, sys, tomllib
with open(sys.argv[1], 'rb') as f: runtime = json.load(f)
with open(sys.argv[2], 'rb') as f: snapshotter = tomllib.load(f)['plugins']['io.containerd.snapshotter.v1.devmapper']
assert runtime['root_drive_sha256'] == '4f48362409e1bb93fadd07b503502134cd105b8eddab8916bd78a26f7c8cba20'
assert runtime['guest_reclamation_profile'] == 'agentenv' and not runtime.get('default_network_interfaces') and not runtime['smt_enabled']
assert snapshotter['base_image_size'] == '21474836480'
PY
truncate -s 30G "$runtime/thinpool/data"
truncate -s 256M "$runtime/thinpool/metadata"
data=$(sudo -n losetup --find --show "$runtime/thinpool/data")
meta=$(sudo -n losetup --find --show "$runtime/thinpool/metadata")
sectors=$(sudo -n blockdev --getsz "$data")
sudo -n dmsetup create "$unit" --table "0 $sectors thin-pool $meta $data 128 32768 1 skip_block_zeroing"
sudo -n systemd-run --unit="$unit" --collect --service-type=simple --property=Delegate=yes --property=KillMode=process \
  --property="StandardOutput=append:$stage/runtime.log" --property="StandardError=append:$stage/runtime.log" \
  --setenv="PATH=$runtime/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin" \
  --setenv="FIRECRACKER_CONTAINERD_RUNTIME_CONFIG_PATH=$runtime/runtime.json" \
  "$runtime/bin/firecracker-containerd" --config "$runtime/containerd.toml"
ctr=(sudo -n "$runtime/bin/firecracker-ctr" -a "$socket" -n aries)
for attempt in {1..50}; do
  if "${ctr[@]}" version > "$stage/runtime-version.txt" 2>&1; then break; fi
  sleep .2
done
test -S "$socket" && test -S "$socket.ttrpc"
while IFS=$'\t' read -r role pin; do
  "${ctr[@]}" images pull --local --platform linux/amd64 --snapshotter devmapper "$pin" > "$stage/pull-$role.log" 2>&1
  "${ctr[@]}" images tag "$pin" "${pin%:*@*}@${pin##*@}"
  test "$("${ctr[@]}" content get "${pin##*@}" | sha256sum | cut -d' ' -f1)" = "${pin##*sha256:}"
done < "$stage/images.tsv"
"${ctr[@]}" images check --snapshotter devmapper > "$stage/image-check.txt"
