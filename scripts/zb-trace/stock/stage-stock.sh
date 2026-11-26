#!/bin/bash
set -euo pipefail
umask 077

base=/local/zb-study3-stock
mkdir -m 711 "$base"
cd "$base"
mkdir downloads server-bundle cli-bundle os-packages registry

release=https://github.com/kvcache-ai/AgentENV/releases/download/v0.2.5
(
  cd downloads
  for asset in SHA256SUMS aenv-server-linux-x86_64.tar.gz aenv-linux-x86_64.tar.gz
  do
    curl -fsSL --retry 3 --max-time 120 "$release/$asset" -o "$asset.part"
    mv "$asset.part" "$asset"
  done
  sha256sum --check <<'SUMS'
8a808c6d04ff8f37e1323b33a29bf475524bcfb881477b9f55e691d59d4bdaff  SHA256SUMS
875090192024f56daabfb14395c7518cfa0b8bc43d458c2840ecc95893d3e5c8  aenv-server-linux-x86_64.tar.gz
ce0b2e80375a131ad1747b065e3d2ac82c0b8dac13ff7c7e169234e2db366d76  aenv-linux-x86_64.tar.gz
SUMS
  awk '$2=="aenv-server-linux-x86_64.tar.gz" || $2=="aenv-linux-x86_64.tar.gz"' SHA256SUMS | sha256sum --check
  tar -xzf aenv-server-linux-x86_64.tar.gz -C ../server-bundle
  tar -xzf aenv-linux-x86_64.tar.gz -C ../cli-bundle
)
sha256sum --check <<'SUMS'
19b402c82e4ef16c12f2434d579448d933ac943f099c993581226d8b6d8f30ea  server-bundle/server
cad0cd9291e7364dff7270981b277e0094b2d5ab76dbd17e16bbe9e41def9875  server-bundle/deps/firecracker/1.15.1-patch-v2/firecracker
SUMS
(
  cd os-packages
  apt-get download umoci docker-registry
  sha256sum *.deb
  for package in *.deb; do dpkg-deb --extract "$package" .; done
)
sudo -n modprobe ublk_drv
sudo -n test -c /dev/ublk-control
sudo -n test -c /dev/kvm

cat >registry/config.yml <<'YAML'
version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /local/zb-study3-stock/registry/data
http:
  addr: 127.0.0.1:18762
YAML
test -z "$(ss -H -ltn 'sport = :18762')"
os-packages/usr/bin/docker-registry serve registry/config.yml >registry/seed.log 2>&1 &
registry_pid=$!
cleanup() {
  kill -TERM "$registry_pid" 2>/dev/null || true
  wait "$registry_pid" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for _ in $(seq 30)
do
  if curl -fsS http://127.0.0.1:18762/v2/ >/dev/null 2>&1; then break; fi
  kill -0 "$registry_pid"
  sleep 1
done
curl -fsS http://127.0.0.1:18762/v2/ >/dev/null
export REGCTL_CONFIG="$base/registry/regctl.json"
regctl="$base/server-bundle/deps/regctl/v0.11.5/regctl"
"$regctl" registry set 127.0.0.1:18762 --tls disabled
source=docker.io/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a
destination=127.0.0.1:18762/library/ubuntu:stock-fixture
"$regctl" image copy "$source" "$destination"
actual=$("$regctl" image digest "$destination")
test "$actual" = "${source#*@}"
printf '%s\n' "$source" "$destination" "$actual" >registry/fixture.txt
