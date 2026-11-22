#!/usr/bin/env bash
# Installs gVisor (runsc) and Kata Containers as extra containerd runtime
# handlers on a vHive-built node (containerd 2.x, config version 3), for the
# gvisor and kata RuntimeClasses in k8s/sandbox-runtimes.yaml. Run with sudo.
# Rerunning it keeps what is already installed.
#
# Kata 4.x ships only the Rust shim, under runtime-rs/, configured here for
# QEMU. Kata needs /dev/kvm; gVisor does not.
set -euo pipefail
GVISOR_VERSION=${GVISOR_VERSION:-20261116.0}
KATA_VERSION=${KATA_VERSION:-4.4.0}
config=/etc/containerd/config.toml
[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
[ "$(uname -m)" = x86_64 ] || { echo "only x86_64 is supported" >&2; exit 1; }

if [ "$(dpkg-query -W -f '${Version}' runsc 2>/dev/null)" != "$GVISOR_VERSION" ]; then
  curl -fsSL https://gvisor.dev/archive.key | gpg --batch --yes --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=amd64 signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" \
    > /etc/apt/sources.list.d/gvisor.list
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --allow-downgrades "runsc=$GVISOR_VERSION" >/dev/null
fi
echo "gVisor $(runsc --version | head -1)"

shim=/opt/kata/runtime-rs/bin/containerd-shim-kata-v2
if ! "$shim" --version 2>/dev/null | grep -q "version: $KATA_VERSION,"; then
  command -v zstd >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq zstd >/dev/null
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  curl -fsSL "https://github.com/kata-containers/kata-containers/releases/download/$KATA_VERSION/kata-static-$KATA_VERSION-amd64.tar.zst" -o "$work/kata.tar.zst"
  rm -rf /opt/kata
  tar --zstd -xf "$work/kata.tar.zst" -C /
fi
ln -sf "$shim" /usr/local/bin/containerd-shim-kata-v2
echo "Kata $("$shim" --version | sed -n 's/.*version: \([^,]*\),.*/\1/p')"
[ -c /dev/kvm ] || echo "warning: /dev/kvm is missing; kata pods will not start" >&2

if ! grep -q "runtimes.runsc\]" "$config"; then
  cp "$config" "$config.pre-sandbox-runtimes"
  cat >> "$config" <<'EOF'

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]
  runtime_type = 'io.containerd.runsc.v1'

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata]
  runtime_type = 'io.containerd.kata.v2'
  privileged_without_host_devices = true
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.kata.options]
    ConfigPath = '/opt/kata/share/defaults/kata-containers/runtime-rs/configuration-qemu-runtime-rs.toml'
EOF
  systemctl restart containerd
  for _ in $(seq 1 30); do systemctl is-active -q containerd && break; sleep 1; done
fi
echo "containerd $(systemctl is-active containerd)"
