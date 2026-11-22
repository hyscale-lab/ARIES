#!/usr/bin/env bash
# Run after the validation worker has released its only Firecracker VM.
set -euo pipefail
stage=$(realpath "$1")
cd "$stage"
runtime=/local/zb-validation/runtime
unit=fcmod-zepbound-amd182
if pgrep -x 'firecracker|[.]firecracker-re' >/dev/null; then exit 2; fi
echo '5bb9fb09fa59ea41a40905e9374d7c110b14ce8a1a141ba979c27e1eea71033a  SHA256SUMS' | sha256sum -c -
sha256sum -c --quiet SHA256SUMS
systemctl is-active --quiet "$unit"
test "$(cat /sys/devices/system/cpu/smt/active)" = 0
sha256sum "$runtime/bin/firecracker" "$runtime/common-vmlinux.bin" "$runtime/common-rootfs.ext4" > runtime-sha256.txt
python3 - <<'PY'
from pathlib import Path
expected={"firecracker":"b33f46cd6d88834cf6a4f8d184f08f219809c636236bffd286d233a60602a2ba","common-vmlinux.bin":"934a078f4b36ba6cfadcc62df0edfbe4b709e6545aa6220f02993eae05267856","common-rootfs.ext4":"292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f"}
assert {Path(name).name:sha for sha,name in map(str.split,Path("runtime-sha256.txt").read_text().splitlines())}==expected
PY
"$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" version > runtime-version.txt
grep -q bad3e9dd65c5db211ed5768bff834d4ef69806e0 runtime-version.txt
if ! command -v bpftrace >/dev/null; then
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bpftrace > bpf-install.log
fi
bpftrace --version > bpf-version.txt
bash provision-token-182.sh "$stage/cni" install dataset-amd182 > provision-token.log
sha256sum provision-token-182.sh cni/conf/10-video.conflist cni/conf/dataset-hosts.json > network-sha256.txt
ip -4 -br addr show > network-addresses.txt
image=$(python3 - <<'PY'
from pathlib import Path
import json
entry,=json.loads(Path("manifest.json").read_text())
assert entry["trial_id"]=="fc1cd637-cc25-45bc-9e02-901bd1e07801"
print(entry["receipt"]["image"])
PY
)
"$runtime/bin/firecracker-ctr" -a "/run/$unit/containerd.sock" -n aries images pull --local --platform linux/amd64 --snapshotter devmapper "$image" > image.log
python3 run-reviewed-cohort.py "$runtime" "$unit" "$stage" v1 manifest.json --dataset-cni "$stage/cni"
