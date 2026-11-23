#!/usr/bin/env bash
# Seal the worker input after building the source-bound adapter.
set -euo pipefail
stage=$(realpath "$1")
assets=$(realpath "$2")
helper=$(realpath "$(dirname "$0")")
chmod 0700 "$stage"
codex=$assets/codex-harbor-tools.tar.gz
shared=$assets/relaxed-ml
tracer=$assets/replacements/cancel-async-stage-01/tracer
echo "1a698641a7ced85512aab1fd6f9277494561ffe18172240f7e7593825dbb6aab  $codex" | sha256sum --quiet -c -
install -m 0600 "$codex" "$stage/codex-harbor-tools.tar.gz"
for name in audit-idle-runtime.py vmm_exclusivity.py; do
    install -m 0644 "$shared/$name" "$stage/$name"
done
install -m 0755 "$helper/"{network,worker,queue}.sh "$stage/"
mkdir -p "$stage/tracer"
for name in guest_sample.py idle.py idle_hook.py kvm-slots.bt run-traced.sh sample_vmm.py snapshot_probe.py vmm_census.py; do
    install -m "$(stat -c '%a' "$tracer/$name")" "$tracer/$name" "$stage/tracer/$name"
done
cat > "$stage/cni-bin.sha256" <<'SUM'
4df8530ce820795ee1df2fbc81d1f225fccacbba311e2e9949b592de4de9c2c4  bridge
879345d7088cd8348eb9480e689757e273317cf2feb3262239922a2142171961  firewall
a1a79380e1eee6b81fb9f62b60691d025cff30a9cc85b5e01079b40d586eaf0e  host-local
c5f5b9e15ebb53760ca8c8a961c987a270db79822d36761cdacc41a5e874d505  loopback
49c53b26e5fe91105391cc2ce3cdde2d2a37be9ac2f73865069280023170e1e5  tc-redirect-tap
SUM
(cd "$stage"; find . -mindepth 1 -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS)
