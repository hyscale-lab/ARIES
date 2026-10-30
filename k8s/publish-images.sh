#!/usr/bin/env bash
# Builds the two images a Kubernetes run pulls from a registry, the bridge and
# the derived Hermes image, for the cluster's platform, and pushes them.
#
# Image names and the Hermes tag come from configs/versions.json through
# scripts/k8s-image-pins, so they match what ARIES requests. The bridge is built
# on bridge.kubernetes_base_image, whose kubectl follows the Kubernetes version
# the pinned Invitro checkout installs; no cluster access is needed. Log in to
# the registry with `docker login` first.
set -euo pipefail

kube_version=""
platform=linux/amd64
build_bridge=true
build_hermes=true
dry_run=false

usage() {
  cat <<'USAGE'
Usage: k8s/publish-images.sh [options] <registry>

The registry is a repository prefix such as docker.io/<user> or
ghcr.io/<org>. Pushes <registry>/aries-bridge and <registry>/aries-hermes, then
prints the profile "images" entries that map ARIES's names onto them.

Options:
  --kube-version X.Y.Z  build the bridge on alpine/k8s:X.Y.Z instead of the
                        pinned bridge.kubernetes_base_image, for a cluster
                        that runs another Kubernetes version
  --platform P          default linux/amd64
  --only bridge|hermes  build one image
  --dry-run             print the build commands without running them
USAGE
}

registry=""
while [ $# -gt 0 ]; do
  case "$1" in
    --kube-version) kube_version=${2#v}; shift 2 ;;
    --platform) platform=$2; shift 2 ;;
    --only)
      case "$2" in
        bridge) build_hermes=false ;;
        hermes) build_bridge=false ;;
        *) echo "--only takes bridge or hermes" >&2; exit 2 ;;
      esac
      shift 2 ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    *) [ -z "$registry" ] || { usage >&2; exit 2; }; registry=${1%/}; shift ;;
  esac
done
[ -n "$registry" ] || { usage >&2; exit 2; }
case "$registry" in
  *[!A-Za-z0-9._:/-]*) echo "registry must be a repository prefix such as docker.io/user" >&2; exit 1 ;;
esac
case "$platform" in
  linux/amd64|linux/arm64) ;;
  *) echo "platform must be linux/amd64 or linux/arm64" >&2; exit 1 ;;
esac
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

for tool in go docker; do
  command -v "$tool" >/dev/null || { echo "$tool is required on PATH" >&2; exit 1; }
done
docker buildx version >/dev/null 2>&1 || { echo "docker buildx is required" >&2; exit 1; }

# Without a login, buildx falls back to an anonymous token and the push fails
# with a bare 403 only after the build. Check the Docker config's logins first:
# its auths and credHelpers keys, and the servers its credential store lists.
host=${registry%%/*}
case "$host" in *.*|*:*|localhost) ;; *) host=docker.io ;; esac
logged_in() {
  local config=${DOCKER_CONFIG:-$HOME/.docker}/config.json servers="" store=""
  [ -f "$config" ] || return 1
  # The config can be a single line, so match keys anywhere on a line.
  servers=$(grep -oE '"[^"]+"[[:space:]]*:[[:space:]]*[{"]' "$config" | sed 's/^"\([^"]*\)".*/\1/')
  store=$(sed -n 's/.*"credsStore": *"\([^"]*\)".*/\1/p' "$config")
  if [ -n "$store" ] && command -v "docker-credential-$store" >/dev/null; then
    servers="$servers
$(docker-credential-"$store" list 2>/dev/null | tr ',' '\n' | sed -n 's/^[{ ]*"\([^"]*\)":.*/\1/p')"
  fi
  case "$host" in
    docker.io) grep -q -E '^(https://)?(index\.)?docker\.io(/v1/?)?$' <<<"$servers" ;;
    *) grep -q -E "^(https://)?${host//./\\.}/?\$" <<<"$servers" ;;
  esac
}
if [ "$dry_run" = false ] && ! logged_in; then
  echo "Docker is not logged in to $host, so the push would be refused." >&2
  [ "$host" != ghcr.io ] || echo "Log in with a classic token holding write:packages:" >&2
  echo "  docker login $host" >&2
  exit 1
fi

run() {
  printf '+'; printf ' %q' "$@"; printf '\n'
  [ "$dry_run" = true ] || "$@"
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

pins=$(cd "$root" && go run ./scripts/k8s-image-pins -hermes-context "$work/hermes")
while IFS='=' read -r key value; do
  case "$key" in
    BRIDGE_IMAGE) bridge_image=$value ;;
    BRIDGE_KUBERNETES_BASE_IMAGE) base=$value ;;
    HERMES_IMAGE) hermes_image=$value ;;
    HERMES_BASE) hermes_base=$value ;;
    HERMES_OTEL_PLUGIN) hermes_otel_plugin=$value ;;
    HERMES_GRPCIO) hermes_grpcio=$value ;;
  esac
done <<<"$pins"

# A mapping replaces the repository and keeps the requested tag, so each image
# is pushed under the tag ARIES asks for.
bridge_repo=${bridge_image%:*}
bridge_tag=${bridge_image##*:}
hermes_repo=${hermes_image%:*}
hermes_tag=${hermes_image##*:}

if [ "$build_bridge" = true ]; then
  if [ -n "$kube_version" ]; then
    case "$kube_version" in
      [0-9]*.[0-9]*.[0-9]*) ;;
      *) echo "--kube-version must look like 1.34.3" >&2; exit 1 ;;
    esac
    base=alpine/k8s:$kube_version
  fi
  [ -n "${base:-}" ] ||
    { echo "configs/versions.json has no bridge.kubernetes_base_image; pass --kube-version" >&2; exit 1; }
  docker buildx imagetools inspect "$base" >/dev/null 2>&1 ||
    { echo "$base does not exist; pass the nearest published version with --kube-version" >&2; exit 1; }

  echo "==> Building $registry/aries-bridge:$bridge_tag on $base for $platform"
  mkdir -p "$work/bridge/bin"
  (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=${platform#linux/} \
    go build -trimpath -o "$work/bridge/bin/aries-bridge" ./cmd/aries-bridge)
  run docker buildx build --platform "$platform" -f "$root/Dockerfile.bridge" \
    --build-arg "BASE_IMAGE=$base" -t "$registry/aries-bridge:$bridge_tag" --push "$work/bridge"
fi

if [ "$build_hermes" = true ]; then
  echo "==> Building $registry/aries-hermes:$hermes_tag on $hermes_base for $platform"
  run docker buildx build --platform "$platform" -f "$work/hermes/Dockerfile" \
    --build-arg "BASE=$hermes_base" --build-arg "OTEL_PLUGIN=$hermes_otel_plugin" \
    --build-arg "GRPCIO=$hermes_grpcio" \
    -t "$registry/aries-hermes:$hermes_tag" --push "$work/hermes"
fi

echo
echo "Profile image mappings, under each block's deployment.kubernetes:"
[ "$build_hermes" = false ] ||
  echo "  harness: \"images\": { \"$hermes_repo\": \"$registry/aries-hermes\" }"
[ "$build_bridge" = false ] ||
  echo "  bridge:  \"images\": { \"$bridge_repo\": \"$registry/aries-bridge\" }"
