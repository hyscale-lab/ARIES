#!/usr/bin/env bash
# Stage the candidate wheel on disk. The recorded pip install runs after restore.
set -euo pipefail
wheel=$(realpath "$1")
archive=$(realpath -m "$2")
[[ ! -e "$archive" ]]
echo "23d27a3e0307ec2244cc51e7287b919aa68d097504ebe19df4e76a98a3eea5bd  $wheel" | sha256sum -c -
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
dir="$tmp/pkg"
mkdir -p "$dir"/{DEBIAN,opt/zb-cad/wheels,etc}
install -m 644 "$wheel" "$dir/opt/zb-cad/wheels/"
cat >"$dir/DEBIAN/control" <<'CONTROL'
Package: zb-cad-wheel
Version: 1
Architecture: all
Maintainer: ARIES research
Description: Offline Pillow wheel for recorded CAD tool installs
CONTROL
cat >"$dir/etc/pip.conf" <<'PIP'
[install]
no-index = true
find-links = file:///opt/zb-cad/wheels
PIP
find "$dir" -exec touch -d @0 {} +
SOURCE_DATE_EPOCH=0 dpkg-deb --root-owner-group -Zgzip -z1 --build "$dir" "$tmp/cad-wheel.deb"
tar --sort=name --mtime=@0 --owner=0 --group=0 --numeric-owner -C "$tmp" -cf - cad-wheel.deb | gzip -n >"$archive"
sha256sum "$archive"
