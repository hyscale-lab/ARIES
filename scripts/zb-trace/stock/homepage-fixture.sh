#!/usr/bin/env bash
set -euo pipefail
stage=$1 sandbox=$2
base=/local/zb-study3-stock/local-sol/repo-homepage
here=$(dirname "$0")
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
upload() { "$AENV_CLI" upload "$sandbox" "$1" "$2"; }
if [[ "$stage" == upload ]]; then
    upload "$base/environment.tar" /tmp/homepage-environment.tar
    guest sh -ec 'cd /workload/rootfs/tmp_workspace; tar xf /tmp/homepage-environment.tar; rm /tmp/homepage-environment.tar; mkdir /workload/rootfs/tmp/homepage-wheels'
    upload "$base/packages.tar" /tmp/homepage-wheels.tar
    guest sh -ec 'tar xf /tmp/homepage-wheels.tar -C /workload/rootfs/tmp/homepage-wheels; rm /tmp/homepage-wheels.tar'
    upload "$here/paper-image.mjs" /workload/rootfs/tmp/paper-image.mjs
    upload "$here/homepage-check.py" /workload/rootfs/tmp/homepage-check.py
    exit
fi
[[ "$stage" == ready ]]
guest runc exec zb-workload sh -ec '
    cd /tmp_workspace
    echo "9017b4b6c4adc889c1b6f7f25c639e9a90254c462a68838bc2578416d0710ac2  .wildclaw/run-warmup.sh" | sha256sum -c -
    echo "c9dc9a11b8a2d3a2ee281952c97a905a4c2e48c1218e285fa455c4a9c97f538d  /root/skills/web-search/SKILL.md" | sha256sum -c -
    cd /tmp/homepage-wheels
    echo "31415f1c18169ffdc17dec880e23a52d006cb764d92a7a6bd21aa0bee371a980  SHA256SUMS" | sha256sum -c -
    sha256sum -c SHA256SUMS
    cd /tmp_workspace
    test ! -e results && test ! -e src && test ! -e node_modules
    if command -v python; then exit 1; fi
    PIP_NO_INDEX=1 PIP_FIND_LINKS=/tmp/homepage-wheels bash .wildclaw/run-warmup.sh
    test -f /tmp/wildclaw-harbor-warmup.done
    python3 -c "from importlib.metadata import version; print({p:version(p) for p in (\"playwright\",\"pillow\",\"requests\",\"beautifulsoup4\")})"
    rm -r /tmp/homepage-wheels
'
