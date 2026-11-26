#!/usr/bin/env bash
set -euo pipefail
umask 077
stage=/local/zb-tb41-medical-wave1
donor=/local/zb-tb41-cohort/wave10
test "$(cat /var/emulab/boot/nodeid)" = amd183
cd "$stage"
export ZB_SOURCE_DIR="$stage/source" ZB_GUEST_PYTHON=/usr/local/bin/python3 ZB_SCAN_THREADS=2
status=0
bash "$stage/zb-trace/tb41/pilot.sh" "$stage" "$stage/runtime" fcmod-zepbound-amd183-medical-wave1 traced "$stage/run" \
    -codex-tools "$donor/fixtures/codex-harbor-tools.tar.gz" \
    -image-helper /local/zb-video-network/replay/aries-harbor-image > "$stage/driver.log" 2>&1 || status=$?
printf '%s\n' "$status" > "$stage/driver.exit"
exit "$status"
