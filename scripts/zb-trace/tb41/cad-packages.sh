#!/usr/bin/env bash
# Build pinned offline archives from the Debian 2026-08-31 bookworm snapshot.
set -euo pipefail
export LC_ALL=C
data=$(realpath "$1")
wheel_archive=$(realpath "$2")
dest=$(realpath "$3")
here=$(dirname "$(realpath "$0")")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for pkg in libgl python; do
    (cd "$data/$pkg" && sha256sum -c "$here/cad-$pkg.sha256")
done
echo "25a171383f021a29ad1b6028b77ef1a019e8b385e4b673acd763186ef8f5c892  $wheel_archive" | sha256sum -c -
for profile in libgl libgl-python python-pillow; do
    out="$dest/cad-$profile.tar.gz"
    [[ ! -e "$out" ]]
    dir="$tmp/$profile"
    mkdir "$dir"
    for pkg in libgl python; do
        if [[ "$profile" == *"$pkg"* ]]; then
            while read -r digest filename; do
                install -m644 "$data/$pkg/$filename" "$dir/$filename"
            done < "$here/cad-$pkg.sha256"
        fi
    done
    if [[ "$profile" == python-pillow ]]; then
        tar xzf "$wheel_archive" -C "$dir" cad-wheel.deb
    fi
    (cd "$dir" && tar --sort=name --owner=0 --group=0 --numeric-owner --mtime=@0 -cf - *.deb) | gzip -n > "$tmp/archive"
    mv -T "$tmp/archive" "$out"
    sha256sum "$out"
done
