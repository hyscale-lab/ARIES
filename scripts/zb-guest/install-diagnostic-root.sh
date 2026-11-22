#!/usr/bin/env bash
# Build a diagnostic copy of the shared ext4 root without modifying the input.
set -euo pipefail
source_image=$(realpath "$1")
probe=$(realpath "$2")
dest=$(realpath -m "$3")
test -f "$source_image" && test -f "$probe" && test ! -e "$dest"
staging=$(mktemp -d "$(dirname "$dest")/.zb-root.XXXXXXXX")
trap 'rm -d "$staging" 2>/dev/null || true' EXIT
cp --sparse=always --reflink=auto "$source_image" "$staging/image"
chmod u+w "$staging/image"
regular_maps=${ZB_REGULAR_MAPS:-0}
[[ "$regular_maps" =~ ^[01]$ ]]
pfn_count=${ZB_GUEST_PFN_COUNT:-524288}
[[ "$pfn_count" =~ ^[1-9][0-9]{0,6}$ ]] && (( pfn_count <= 2097152 )) || {
    echo 'ZB_GUEST_PFN_COUNT must be an integer from 1 through 2097152' >&2
    exit 2
}
cat > "$staging/unit" <<EOF
[Unit]
Description=Private guest memory telemetry
Before=firecracker.target
After=local-fs.target

[Service]
Environment=ZB_REGULAR_MAPS=$regular_maps
ExecStart=/usr/local/bin/zb-guest serve --host-vsock --pfn-count $pfn_count

[Install]
WantedBy=firecracker.target
EOF
# The guest's systemd unit reads only kernel-provided memory metadata.
debugfs -w "$staging/image" <<EOF
write $probe /usr/local/bin/zb-guest
set_inode_field /usr/local/bin/zb-guest mode 0100755
write $staging/unit /etc/systemd/system/zb-guest.service
symlink /etc/systemd/system/firecracker.target.wants/zb-guest.service /etc/systemd/system/zb-guest.service
EOF
debugfs -R 'cat /usr/local/bin/zb-guest' "$staging/image" 2>/dev/null | cmp - "$probe"
debugfs -R 'cat /etc/systemd/system/zb-guest.service' "$staging/image" 2>/dev/null | cmp - "$staging/unit"
debugfs -R 'stat /etc/systemd/system/firecracker.target.wants/zb-guest.service' "$staging/image" 2>/dev/null | grep -F '/etc/systemd/system/zb-guest.service'
e2fsck -fn "$staging/image"
chmod a-w "$staging/image"
ln "$staging/image" "$dest"
unlink "$staging/image"
unlink "$staging/unit"
sha256sum "$source_image" "$dest" "$probe"
