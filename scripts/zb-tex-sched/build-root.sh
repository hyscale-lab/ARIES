#!/usr/bin/env bash
# Build a new isolated guest root with only the trace service enabled.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(realpath "$1")
out=$(realpath -m "$2")
mkdir "$out"
echo "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f  $root" | sha256sum -c -
cd "$here/../.."
CGO_ENABLED=0 go build -trimpath -o "$out/tex-sched" ./scripts/zb-tex-sched/guest
cp --reflink=auto --sparse=always "$root" "$out/diagnostic.ext4"
chmod u+w "$out/diagnostic.ext4"
cat > "$out/tex-sched.service" <<'EOF'
[Unit]
Description=Private TeX compilation trace
Before=firecracker.target
After=local-fs.target

[Service]
ExecStart=/usr/local/bin/tex-sched

[Install]
WantedBy=firecracker.target
EOF
cd "$out"
debugfs -w diagnostic.ext4 <<'EOF'
write tex-sched /usr/local/bin/tex-sched
set_inode_field /usr/local/bin/tex-sched mode 0100755
write tex-sched.service /etc/systemd/system/tex-sched.service
symlink /etc/systemd/system/firecracker.target.wants/tex-sched.service /etc/systemd/system/tex-sched.service
EOF
debugfs -R 'cat /usr/local/bin/tex-sched' diagnostic.ext4 2>/dev/null | cmp - tex-sched
debugfs -R 'cat /etc/systemd/system/tex-sched.service' diagnostic.ext4 2>/dev/null | cmp - tex-sched.service
debugfs -R 'stat /etc/systemd/system/firecracker.target.wants/tex-sched.service' diagnostic.ext4 2>/dev/null | grep -F '/etc/systemd/system/tex-sched.service'
e2fsck -fn diagnostic.ext4
chmod a-w diagnostic.ext4
gzip -1c diagnostic.ext4 > diagnostic.ext4.gz
sha256sum diagnostic.ext4 diagnostic.ext4.gz tex-sched tex-sched.service > root.sha256
