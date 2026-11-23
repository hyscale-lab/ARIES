#!/usr/bin/env bash
# Build the diagnostic guest as its own main package, preserving the measured import path.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
dst=$(realpath -m "$1")
test ! -e "$dst"
mkdir -p "$(dirname "$dst")"
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
cp "$repo"/scripts/zb-guest/*.go "$repo/go.mod" "$repo/go.sum" "$work/"
cp "$here/testdata/filemap_probe.go" "$work/"
cd "$work"
patch --quiet --fuzz=0 -p0 --input "$here/testdata/wheel-release.patch"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1
go test -buildvcs=false .
go build -buildvcs=false -trimpath -o "$dst" .
sha256sum "$dst"
