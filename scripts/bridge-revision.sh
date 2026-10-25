#!/usr/bin/env bash
# Prints the commit that last changed anything the bridge image is built from,
# shortened to twelve characters, with -dirty when those files have
# uncommitted changes. Kubernetes runs request the bridge tagged with it, and
# `make build`, k8s/create-cluster.sh and k8s/publish-images.sh all derive it
# here, so a published bridge image keeps matching until the bridge changes.
#
# The inputs are the repository's Go packages that cmd/aries-bridge compiles,
# without their tests; go.mod and go.sum; Dockerfile.bridge; and
# configs/versions.json, which pins the bridge's base images.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$root"
packages=$(go list -deps -f '{{if not .Standard}}{{.Dir}}{{end}}' ./cmd/aries-bridge | sed -n "s|^$root/||p")
[ -n "$packages" ] || { echo "cannot list the bridge's packages" >&2; exit 1; }
specs=()
for directory in $packages; do
  # Files directly in the package's directory, not its subpackages.
  specs+=(":(glob)$directory/*")
done
specs+=(go.mod go.sum Dockerfile.bridge configs/versions.json ":(exclude,glob)**/*_test.go")

commit=$(git log -1 --format=%H -- "${specs[@]}")
[ -n "$commit" ] || { echo "no commit touches the bridge's files" >&2; exit 1; }
revision=${commit:0:12}
git diff --quiet HEAD -- "${specs[@]}" || revision="$revision-dirty"
echo "$revision"
