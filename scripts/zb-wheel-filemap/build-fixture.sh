#!/usr/bin/env bash
# Install only this diagnostic guest in a new copy of the study's clean ext4 image.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
clean=$(realpath "$1")
probe=$(realpath "$2")
dest=$(realpath -m "$3")
mkdir "$dest"
echo "292a537a4bdb05fe4323352ecf74299c13c732b9e430d153d5bd5e53772fed5f  $clean" | sha256sum -c -
ZB_TORCH_FILE_CACHE=1 ZB_TORCH_WHEEL_DONTNEED=1 ZB_GUEST_PFN_COUNT=0 \
    "$here/../zb-guest/install-diagnostic-root.sh" "$clean" "$probe" "$dest/diagnostic.ext4"
gzip -1c -- "$dest/diagnostic.ext4" > "$dest/diagnostic.ext4.gz"
(cd "$dest" && sha256sum diagnostic.ext4 diagnostic.ext4.gz > fixture.sha256)
cat "$dest/fixture.sha256"
