#!/bin/bash
# Run with sudo unshare -m --propagation private from the node-local pilot directory.
set -euo pipefail
cd "$(dirname "$(realpath "$0")")"
export PATH="$PWD/apt/root/usr/bin:$PATH"
export LD_LIBRARY_PATH="$PWD/apt/root/usr/lib/x86_64-linux-gnu"
export TMPDIR="$PWD/tmp"
base=DeltaBox-ATC2026-v3-l1.qcow2
test -f "images/$base"
work=$(mktemp -d "$TMPDIR/direct.XXXXXX")
disk=$work/disk
mnt=$work/mnt
touch "$disk"
mkdir "$mnt"

stage_layer() (
    local name=$1 init=$2
    test ! -e "images/$name"
    qemu-img create -f qcow2 -F qcow2 -b "$base" "images/$name"
    qemu-storage-daemon \
        --blockdev "driver=qcow2,node-name=storage,file.driver=file,file.filename=$PWD/images/$name" \
        --export "type=fuse,id=image,node-name=storage,mountpoint=$disk,writable=on" &
    local daemon=$!
    trap 'umount "$mnt" 2>/dev/null || true; kill "$daemon" 2>/dev/null || true; wait "$daemon" || true' EXIT
    for attempt in {1..50}; do
        test "$(stat -c %s "$disk")" != 0 && break
        kill -0 "$daemon"
        sleep .1
    done
    local part
    part=$(sfdisk --json "$disk" | python3 -c '
import json, sys
table = json.load(sys.stdin)["partitiontable"]
p = table["partitions"][0]
sector = table["sectorsize"]
print("offset={},sizelimit={}".format(p["start"] * sector, p["size"] * sector))
')
    mount -o "loop,$part" "$disk" "$mnt"
    install -D -m 0755 "$init" "$mnt/usr/local/lib/deltabox-direct/init"
    install -D -m 0644 ae/ae/appliance/direct/check.py "$mnt/usr/local/lib/deltabox-direct/check.py"
    umount "$mnt"
    kill "$daemon"
    wait "$daemon"
    trap - EXIT
    qemu-img check "images/$name"
    sha256sum "images/$name"
)

stage_layer deltabox-direct.qcow2 ae/ae/appliance/direct/init
python3 - <<'PY'
from pathlib import Path
source = Path("ae/ae/appliance/direct/init").read_text()
old = 'cp /usr/local/lib/deltabox-direct/check.py "$root/tmp/check.py"'
assert source.count(old) == 1
Path("images/calendar-init").write_text(source.replace(old, """mkdir -p "$root/mnt/fixture"
mount -t ext4 -o ro /dev/vdb "$root/mnt/fixture"
cp "$root/mnt/fixture/guest.py" "$root/tmp/check.py"
"""))
PY
stage_layer calendar-direct.qcow2 images/calendar-init

cp -a payload "$work/fixture"
mkdir -p "$work/fixture/runtime" "$work/fixture/environment/.openclaw"
for marker in AGENTS.md BOOTSTRAP.md HEARTBEAT.md IDENTITY.md SOUL.md TOOLS.md USER.md .openclaw/workspace-state.json; do
    touch "$work/fixture/environment/$marker"
done
chown -R 0:0 "$work/fixture/environment"
git -c safe.directory="$PWD/ae" -C ae archive HEAD agent/sandbox_driver.py backends/deltabox common pycriu upper/agent/shell_server.py \
    | tar -xf - -C "$work/fixture/runtime"

test ! -e images/calendar-fixture.ext4
truncate -s 32M images/calendar-fixture.ext4
mkfs.ext4 -q -d "$work/fixture" images/calendar-fixture.ext4
test ! -e images/calendar-verifier.ext4
truncate -s 16M images/calendar-verifier.ext4
mkfs.ext4 -q -d verifier images/calendar-verifier.ext4
sha256sum ae/linux/vmlinux images/calendar-{fixture,verifier}.ext4
qemu-img info --output=json --backing-chain images/calendar-direct.qcow2
