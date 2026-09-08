#!/usr/bin/env bash
# Build the ARIES image and push it to the registry the cluster pulls from.
#
# The image name is read from k8s/overlays/incluster/kustomization.yaml rather
# than hardcoded here, so the thing built and the thing deployed cannot drift.
#
# Two tags are pushed for every build: the mutable one the overlay references
# (:latest) and an immutable one derived from the commit. Runs record the image
# they used, so the immutable tag is what lets a result be traced back to the
# exact source that produced it once :latest has moved on.
#
# Usage:
#   ./deploy/build-image.sh                  # verify, build, push
#   ./deploy/build-image.sh --rollout        # ...then restart the cluster pod
#   ./deploy/build-image.sh --no-push        # build locally only
#   ./deploy/build-image.sh --skip-verify    # skip build+test gate
#   ./deploy/build-image.sh --check          # verify only, build nothing
#
# Options:
#   --rollout        After pushing, apply the incluster overlay and wait for the
#                    new pod. Refuses to run while a benchmark is in flight.
#   --no-push        Build without pushing. Implies no rollout.
#   --check          Run the verification gate and stop, changing nothing.
#   --skip-verify    Do not run the cross-compile and test gate first.
#   --tag TAG        Extra tag to push alongside :latest and the commit tag.
#   --platform P     Target platform. Default: linux/amd64 (the cluster nodes).
#
# Requires: docker (logged in to the registry), and for --rollout, kubectl with
# a kubeconfig for the cluster.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OVERLAY="$REPO_ROOT/k8s/overlays/incluster"

PUSH=1
ROLLOUT=0
CHECK_ONLY=0
VERIFY=1
EXTRA_TAG=""
PLATFORM="linux/amd64"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

usage() { awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --rollout) ROLLOUT=1; shift ;;
    --no-push) PUSH=0; shift ;;
    --check) CHECK_ONLY=1; shift ;;
    --skip-verify) VERIFY=0; shift ;;
    --tag) EXTRA_TAG="$2"; shift 2 ;;
    --platform) PLATFORM="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" ;;
  esac
done

[ "$PUSH" = "1" ] || ROLLOUT=0

command -v docker >/dev/null 2>&1 || die "docker is not on PATH"

# --- image identity -----------------------------------------------------------

# The overlay's images: block is the single source of truth for what the cluster
# pulls. Reading it here means renaming the registry in one place is enough.
read_overlay_image() {
  awk '
    /^images:/ { in_images = 1; next }
    in_images && /^[^ -]/ { in_images = 0 }
    in_images && $1 == "newName:" { print $2; exit }
  ' "$OVERLAY/kustomization.yaml"
}

IMAGE="$(read_overlay_image)"
[ -n "$IMAGE" ] || die "could not read images.newName from $OVERLAY/kustomization.yaml"

# The commit tag identifies the source, so a dirty tree must not silently
# produce one that claims to be that commit.
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
DIRTY=0
if ! git -C "$REPO_ROOT" diff --quiet HEAD 2>/dev/null; then
  DIRTY=1
fi
COMMIT_TAG="$COMMIT"
if [ "$DIRTY" = "1" ]; then
  COMMIT_TAG="$COMMIT-dirty-$(date -u +%Y%m%dT%H%M%SZ)"
fi

TAGS=("$IMAGE:latest" "$IMAGE:$COMMIT_TAG")
if [ -n "$EXTRA_TAG" ]; then
  TAGS+=("$IMAGE:$EXTRA_TAG")
fi

log "image $IMAGE"
info "platform    $PLATFORM"
info "commit tag  $COMMIT_TAG"
if [ "$DIRTY" = "1" ]; then
  warn "working tree has uncommitted changes; the commit tag is timestamped so it"
  warn "does not claim to be $COMMIT. Commit first if this build produces results."
fi

# --- verify -------------------------------------------------------------------

# Compile for the target OS and run the tests before spending time on a build
# and a push. The cluster is linux/amd64 while this script usually runs on a
# Mac, and the platform-specific packages only fail under the right GOOS.
verify() {
  [ "$VERIFY" = "1" ] || { log "skipping verification (--skip-verify)"; return 0; }
  log "verifying"
  ( cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 go build ./... ) \
    || die "linux build failed; not building an image from it"
  info "linux build ok"
  # Several packages refuse an output directory whose path contains a symlink,
  # which is a deliberate guard. On macOS the default TMPDIR is under /var, and
  # /var is a symlink to /private/var, so t.TempDir() trips the guard before any
  # test logic runs. Resolving TMPDIR gives the tests the kind of real path they
  # would get on Linux; it does not relax anything they assert.
  local resolved_tmp
  resolved_tmp="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
  # pkg/... only: internal/modelruntime/sglang does not compile on darwin, and
  # the image build recompiles everything for linux regardless.
  ( cd "$REPO_ROOT" && TMPDIR="$resolved_tmp" go test ./pkg/... >/dev/null ) \
    || die "tests failed; not building an image from them"
  info "tests ok"
  if [ -n "$(cd "$REPO_ROOT" && gofmt -l pkg cmd internal)" ]; then
    die "gofmt reports unformatted files; run gofmt -w"
  fi
  info "gofmt ok"
}

verify

if [ "$CHECK_ONLY" = "1" ]; then
  log "--check requested; stopping without building"
  exit 0
fi

# --- build --------------------------------------------------------------------

build() {
  local args=()
  for tag in "${TAGS[@]}"; do
    args+=(-t "$tag")
  done
  log "building"
  # --load keeps the result in the local daemon so a --no-push build is still
  # usable; buildx otherwise discards a cross-platform image after building it.
  docker buildx build --platform "$PLATFORM" "${args[@]}" --load "$REPO_ROOT" \
    || die "docker build failed"
  for tag in "${TAGS[@]}"; do
    info "built $tag"
  done
}

build

if [ "$PUSH" = "0" ]; then
  log "--no-push: stopping after build"
  exit 0
fi

# --- push ---------------------------------------------------------------------

log "pushing"
for tag in "${TAGS[@]}"; do
  docker push "$tag" >/dev/null || die "push failed for $tag (is 'docker login' current?)"
  info "pushed $tag"
done

DIGEST="$(docker inspect --format '{{index .RepoDigests 0}}' "$IMAGE:latest" 2>/dev/null || true)"
if [ -n "$DIGEST" ]; then
  info "digest $DIGEST"
fi

# --- rollout ------------------------------------------------------------------

if [ "$ROLLOUT" = "0" ]; then
  cat <<EOF

Pushed. To roll the cluster onto it:

  kubectl apply -k k8s/overlays/incluster
  kubectl rollout status -n aries deployment/aries

The overlay sets imagePullPolicy: Always against :latest, so a restart is what
picks this up. Re-running this script with --rollout does both.
EOF
  exit 0
fi

command -v kubectl >/dev/null 2>&1 || die "kubectl is not on PATH; cannot --rollout"

# A rollout deletes the ARIES pod, and with it any benchmark it is driving. The
# task and agent pods it spawned would be left behind with no owner.
running_tasks() {
  kubectl get pods -n aries -l app.kubernetes.io/managed-by=aries \
    -o name 2>/dev/null | grep -c . || true
}

if [ "$(running_tasks)" -gt 0 ]; then
  kubectl get pods -n aries -l app.kubernetes.io/managed-by=aries >&2
  die "a benchmark appears to be in flight; restarting ARIES would orphan those pods. Wait, or delete them first."
fi

log "rolling out"
kubectl apply -k "$OVERLAY"
# Recreate strategy plus a ReadWriteOnce volume means the old pod goes first;
# a gap with no pod at all is expected here, not a failure.
kubectl rollout status -n aries deployment/aries --timeout=300s

log "done"
kubectl get pods -n aries -o wide
