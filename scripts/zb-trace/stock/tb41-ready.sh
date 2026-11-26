#!/usr/bin/env bash
set -euo pipefail
sandbox=${1:?sandbox}
fixture=${2:?fixture directory}
meta="$fixture/metadata.json"
: "${AENV_CLI:?}" "${TB41_CODEX_ARCHIVE:?}"
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
container() { guest runc exec --cwd "$(jq -er .workdir "$meta")" zb-workload "$@"; }
upload_archive() {
    local archive=$1 expected=$2 target=$3
    echo "$expected  $archive" | sha256sum -c -
    "$AENV_CLI" upload "$sandbox" "$archive" /tmp/tb41-fixture.tar.gz
    guest tar -xzf /tmp/tb41-fixture.tar.gz -C "$target"
    guest rm /tmp/tb41-fixture.tar.gz
}
helper=$(jq -r '.image_helper_sha256 // ""' "$meta")
if [[ -n "$helper" ]]; then
    : "${TB41_IMAGE_HELPER:?}"
    echo "$helper  $TB41_IMAGE_HELPER" | sha256sum -c -
    "$AENV_CLI" upload "$sandbox" "$TB41_IMAGE_HELPER" /workload/rootfs/usr/local/bin/aries-harbor-image
    container chmod 755 /usr/local/bin/aries-harbor-image
fi
packages=$(jq -r .packages_sha256 "$meta")
if [[ "$(jq -er .task "$meta")" == hof-topology-interpenetration ]]; then
    : "${TB41_PACKAGE_ARCHIVE:?}"
    guest mkdir -p /workload/rootfs/opt/zb-hof
    upload_archive "$TB41_PACKAGE_ARCHIVE" "$packages" /workload/rootfs/opt/zb-hof
elif [[ "$(jq -er .task "$meta")" == intrastat-meldung ]]; then
    upload_archive "${TB41_PACKAGE_ARCHIVE:?}" "$packages" /workload/rootfs/tmp
elif [[ -n "$packages" ]]; then
    : "${TB41_PACKAGE_ARCHIVE:?}"
    guest mkdir /workload/rootfs/tmp/aries-guest-offline-packages
    upload_archive "$TB41_PACKAGE_ARCHIVE" "$packages" /workload/rootfs/tmp/aries-guest-offline-packages
    container sh -ec 'cd /tmp/aries-guest-offline-packages; export DEBIAN_FRONTEND=noninteractive; apt-get -o Dir::Etc::sourcelist=/dev/null -o Dir::Etc::sourceparts=- --no-install-recommends -y install ./*.deb; rm -f -- ./*.deb; cd /tmp; rmdir aries-guest-offline-packages'
fi
upload_archive "$TB41_CODEX_ARCHIVE" "$(jq -er .codex_prefix_sha256 "$meta")" /workload/rootfs
container bash -lc "$(jq -er .ready_script "$meta")"
