#!/usr/bin/env bash
# Use the WAL fixtures already staged on amd189.
set -euo pipefail
name=${1:?new service label}
arm=${2:?1, 9, or pack}
base=/local/zb-study3-stock
fixtures=/local/zb-tb41-cohort/wave10/fixtures
export TB41_IMAGE_STORE=/var/lib/containerd/io.containerd.content.v1.content/blobs/sha256
export TB41_CODEX_ARCHIVE="$fixtures/codex-harbor-tools.tar.gz" TB41_PACKAGE_ARCHIVE="$fixtures/os-tools.tar.gz"
exec "$base/scripts/zb-trace/stock/tb41-service.sh" wal-recovery-ordering "$arm" "$name" \
    5bce5a1e26b9f0810835cca7f0922cd5cd12e69ac34b2274b538bbad910ec299
