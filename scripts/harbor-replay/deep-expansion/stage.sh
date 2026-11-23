#!/usr/bin/env bash
set -euo pipefail
helper=$(cd -- "$(dirname -- "$0")" && pwd)
scripts=$(dirname "$(dirname "$helper")")
stage=$(realpath -m "$1")
cli=$(realpath "$2")
umask 077
mkdir "$stage"
install -m 0644 "$helper"/{replay.py,worker.sh,trace-contract.py} \
  "$scripts/harbor-replay/audit-idle-runtime.py" "$scripts/zb-trace/vmm_exclusivity.py" "$stage/"
install -m 0755 "$cli" "$stage/aries-normal-launchclock-v3"
