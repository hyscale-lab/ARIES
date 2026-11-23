#!/usr/bin/env bash
set -euo pipefail
dir=$(cd -- "$(dirname -- "$0")" && pwd)
repo=$(realpath "$1")
output=$(realpath -m "$2")
go=${3:-go}
patch=adapter.patch
case "${4:-base}" in
  base) ;;
  gnu-patch) patch=patch-adapter.patch ;;
  *) exit 2 ;;
esac
scratch=$(mktemp -d)
finish() {
  git -C "$repo" worktree remove --force "$scratch/source"
  rmdir "$scratch"
}
[[ ! -e $output ]]
git -C "$repo" worktree add --detach "$scratch/source" "$(cat "$dir/base-revision.txt")"
trap finish EXIT
git -C "$scratch/source" apply --check "$dir/$patch"
git -C "$scratch/source" apply "$dir/$patch"
cd "$scratch/source"
export CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 GOTOOLCHAIN=local
"$go" test ./pkg/replay/harbor ./cmd/aries-harbor
"$go" build -trimpath -buildvcs=false -mod=readonly -o "$output" ./cmd/aries-harbor
sha256sum "$output"
