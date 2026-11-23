#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
dst=$(realpath -m "$1")
test ! -e "$dst"
work=$(mktemp -d)
trap 'python3 -c "import shutil,sys; shutil.rmtree(sys.argv[1])" "$work"' EXIT
git -C "$repo" archive de9649feef1fa0ae8e150d26890313cd71ead678 | tar -C "$work" -xf -
cd "$work"
patch --batch --fuzz=0 -p1 < "$here/native.patch"
cp "$here/relaxed_process_test.go.txt" pkg/replay/harbor/relaxed_compcert_test.go
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1
go test -buildvcs=false ./pkg/replay/harbor ./cmd/aries-harbor
go build -trimpath -buildvcs=false -o "$dst" ./cmd/aries-harbor
sha256sum "$dst"
