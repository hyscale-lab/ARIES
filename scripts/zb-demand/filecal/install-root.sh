#!/usr/bin/env bash
# Add one host-only calibration service to a private copy of an ext4 root.
set -euo pipefail
source_image=$(realpath "$1")
probe=$(realpath "$2")
dest=$(realpath -m "$3")
service=${4:-zb-filecal}
[[ "$service" =~ ^zb-(filecal|pressure)$ ]]
test -f "$source_image" && test -x "$probe" && test ! -e "$dest"
staging=$(mktemp -d "$(dirname "$dest")/.filecal.XXXXXXXX")
trap 'rm -d "$staging" 2>/dev/null || true' EXIT
cp --sparse=always --reflink=auto "$source_image" "$staging/image"
chmod u+w "$staging/image"
cat > "$staging/unit" <<UNIT
[Unit]
Description=Controlled guest file calibration
Before=firecracker.target
After=local-fs.target

[Service]
ExecStart=/usr/local/bin/$service-guest

[Install]
WantedBy=firecracker.target
UNIT
debugfs -w "$staging/image" <<DEBUG
write $probe /usr/local/bin/$service-guest
set_inode_field /usr/local/bin/$service-guest mode 0100755
write $staging/unit /etc/systemd/system/$service.service
symlink /etc/systemd/system/firecracker.target.wants/$service.service /etc/systemd/system/$service.service
DEBUG
debugfs -R "cat /usr/local/bin/$service-guest" "$staging/image" 2>/dev/null | cmp - "$probe"
debugfs -R "cat /etc/systemd/system/$service.service" "$staging/image" 2>/dev/null | cmp - "$staging/unit"
debugfs -R "stat /etc/systemd/system/firecracker.target.wants/$service.service" "$staging/image" 2>/dev/null |
    grep -F "/etc/systemd/system/$service.service"
e2fsck -fn "$staging/image"
chmod a-w "$staging/image"
ln "$staging/image" "$dest"
unlink "$staging/image"
unlink "$staging/unit"
sha256sum "$source_image" "$dest" "$probe"
