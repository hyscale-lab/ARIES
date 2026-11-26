#!/usr/bin/env bash
# Extend the verified CAD Qt and Python bundle for the recorded GNU patch calls.
set -euo pipefail
export LC_ALL=C
bundle=$(realpath "$1")
patch_deb=$(realpath "$2")
output=$(realpath -m "$3")
[[ ! -e "$output" ]]
echo "0147cfc122120b0891434f43c9a81db9655cf7f4914d7704789116e54c953ba9  $bundle" | sha256sum -c -
echo "8c6d49b771530dbe26d7bd060582dc7d2b4eeb603a20789debc1ef4bbbc4ef67  $patch_deb" | sha256sum -c -
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
tar xzf "$bundle" -C "$tmp"
install -m644 "$patch_deb" "$tmp/patch_2.7.6-7_amd64.deb"
(cd "$tmp" && tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - *.deb) | gzip -n >"$tmp/bundle.tar.gz"
mv -T "$tmp/bundle.tar.gz" "$output"
sha256sum "$output"
