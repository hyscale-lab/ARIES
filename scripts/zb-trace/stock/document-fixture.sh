#!/usr/bin/env bash
set -euo pipefail
task=$1 stage=$2 sandbox=$3
base=/local/zb-study3-stock/local-sol
guest() { "$AENV_CLI" exec "$sandbox" "$@"; }
upload() { "$AENV_CLI" upload "$sandbox" "$1" "$2"; }
if [[ "$stage" == upload ]]; then
    upload "$base/$task/environment.tar" /tmp/document-inputs.tar
    upload "$base/wheels.tar" /tmp/document-wheels.tar
    guest sh -ec 'set -e; cd /workload/rootfs/tmp_workspace; test ! -e results; tar xf /tmp/document-inputs.tar; mkdir /workload/rootfs/tmp/document-wheels; tar xf /tmp/document-wheels.tar -C /workload/rootfs/tmp/document-wheels; rm /tmp/document-inputs.tar /tmp/document-wheels.tar'
    if [[ "$task" == conflicting-handling ]]; then
        upload "$base/npm.tar" /tmp/npm-registry.tar
        guest sh -ec 'mkdir /workload/rootfs/tmp/npm-registry; tar xf /tmp/npm-registry.tar -C /workload/rootfs/tmp/npm-registry; rm /tmp/npm-registry.tar'
    fi
    exit
fi
[[ "$stage" == ready ]]
guest runc exec zb-workload sh -ec 'cd /tmp/document-wheels; test "$(sha256sum SHA256SUMS | cut -d" " -f1)" = 93799e3e316974fae8d665641dfe4a13ae81f53164fb48ce5d95788d1eecef39; sha256sum -c SHA256SUMS; python3 -c "import sys,importlib.util as u; assert sys.version_info[:2]==(3,10) and u.find_spec(\"pypdf\") is None"; ! command -v python'
case "$task" in
  pdf-digest)
    guest runc exec zb-workload sh -ec 'cd /tmp_workspace; test "$(sha256sum papers.tar | cut -d" " -f1)" = 474c2a204a878c2a38b6e7f91971e79c9f45b478ebcef7625ec28a1046f3a107; test ! -d work_extract'
    ;;
  conflicting-handling)
    guest runc exec zb-workload sh -ec 'cd /tmp_workspace; find .wildclaw 04_Search_Retrieval_task_2_conflicting_handling -type f | sed "s,^,./," | LC_ALL=C sort | xargs sha256sum | sha256sum | grep -q "^a19729dcd2480bbde806e299a7da81177215e9ec3f1b299b986ef3df8c2bb13b "; test ! -e /root/memory/2026-07-17.md; test ! -e /root/memory/2026-07-16.md; if ! test -d /root/.git; then git -C /root init --quiet; fi; for f in AGENTS.md BOOTSTRAP.md HEARTBEAT.md IDENTITY.md SOUL.md TOOLS.md USER.md; do if test -f "$f" && ! test -e "/root/$f"; then cp "$f" "/root/$f"; fi; done'
    guest runc exec --env npm_config_cache=/tmp/npm-registry --env npm_config_registry=https://registry.npmjs.org --env npm_config_offline=true --env npm_config_before=2026-07-17T16:08:34Z zb-workload bash /tmp_workspace/.wildclaw/run-warmup.sh
    guest runc exec zb-workload sh -ec 'test -f /tmp/wildclaw-harbor-warmup.done; agent-browser --version | grep -F "0.27.0"; rm -r /tmp/npm-registry'
    ;;
  *) exit 2;;
esac
