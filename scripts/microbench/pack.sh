#!/usr/bin/env bash
# Packs the committed microbenchmark tasks into the git checkout that the
# microbenchmark profiles run from, .cache/microbench.
#
#   scripts/microbench/pack.sh [DESTINATION]
#   scripts/microbench/pack.sh --print-revision
#
# ARIES loads Terminal-Bench-format tasks only from a clean checkout at the
# revision its versions file pins. The tasks live in this repository under
# benchmarks/microbench/, so this script commits that tree into a repository of
# its own with a fixed author, date and message: the commit hash then depends
# only on the tasks' contents, and configs/microbench-versions.json pins it.
# Only committed task files are packed, so the pin always names reviewed
# content. Safe to run again.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
print_only=false
destination=$root/.cache/microbench
case "${1:-}" in
  --print-revision) print_only=true ;;
  "") ;;
  -*) echo "unknown option: $1" >&2; exit 2 ;;
  *) destination=$1 ;;
esac

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
git -C "$root" archive HEAD:benchmarks/microbench | tar -x -C "$work"
export GIT_AUTHOR_NAME=ARIES GIT_AUTHOR_EMAIL=aries@localhost GIT_AUTHOR_DATE=2026-01-01T00:00:00Z
export GIT_COMMITTER_NAME=ARIES GIT_COMMITTER_EMAIL=aries@localhost GIT_COMMITTER_DATE=2026-01-01T00:00:00Z
git -C "$work" init --quiet
git -C "$work" add --all
git -C "$work" -c commit.gpgsign=false commit --quiet --no-verify -m "ARIES microbenchmark tasks"
revision=$(git -C "$work" rev-parse HEAD)

if [ "$print_only" = true ]; then
  echo "$revision"
  exit 0
fi

pinned=$(sed -n '/"terminalbench2"/,/}/s/.*"revision": *"\([0-9a-f]*\)".*/\1/p' "$root/configs/microbench-versions.json")
if [ "$revision" != "$pinned" ]; then
  echo "the committed tasks pack to $revision, but configs/microbench-versions.json pins ${pinned:-nothing};" >&2
  echo "set terminalbench2.revision there to $revision and commit it" >&2
  exit 1
fi

if [ -d "$destination" ] && [ "$(git -C "$destination" rev-parse HEAD 2>/dev/null)" = "$revision" ] &&
  [ -z "$(git -C "$destination" status --porcelain --untracked-files=all)" ]; then
  echo "$destination is already at $revision"
  exit 0
fi
# Replace only an earlier packed checkout, never an unrelated directory.
if [ -e "$destination" ]; then
  if [ "$(git -C "$destination" log -1 --format=%an%n%s 2>/dev/null)" != "ARIES
ARIES microbenchmark tasks" ]; then
    echo "$destination exists and is not a packed microbenchmark checkout; remove it or choose another destination" >&2
    exit 1
  fi
  rm -rf "$destination"
fi
mkdir -p "$(dirname "$destination")"
mv "$work" "$destination"
trap - EXIT
echo "packed the microbenchmark tasks at $revision into $destination"
