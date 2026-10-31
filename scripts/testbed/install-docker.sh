#!/usr/bin/env bash
# Installs a Docker engine on a Kubernetes node of an Invitro-built cluster, so
# Docker runs can execute on the same hardware as Kubernetes runs and be
# measured by the same cAdvisor.
#
#   sudo scripts/testbed/install-docker.sh
#
# vHive runs its own containerd from /usr/local/bin, which the kubelet uses. A
# packaged Docker would install a second containerd on the same socket, so this
# installs only dockerd and the CLI from Docker's static release and points
# dockerd at the containerd already running; Docker keeps its containers in its
# own containerd namespace, apart from the kubelet's.
#
# Docker 29 needs a newer runc than vHive installs (1.1.4), so the release's
# runc replaces the node's at /usr/local/sbin/runc, the old one kept beside it.
# Docker and Kubernetes containers on the node then run under the same runc,
# which keeps the two backends comparable. Running containers are unaffected;
# runc is only invoked to create, exec into or stop one. Safe to run again.
set -euo pipefail

version=${DOCKER_VERSION:-29.8.2}
containerd_socket=/run/containerd/containerd.sock

[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
case "$(uname -m)" in
  x86_64) arch=x86_64 ;;
  aarch64) arch=aarch64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
[ -S "$containerd_socket" ] || { echo "no containerd at $containerd_socket; start the cluster's containerd first" >&2; exit 1; }

node_runc=/usr/local/sbin/runc
if ! /usr/local/bin/dockerd --version 2>/dev/null | grep -q " $version,"; then
  echo "==> Installing Docker $version"
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  curl -fsSL "https://download.docker.com/linux/static/stable/$arch/docker-$version.tgz" -o "$work/docker.tgz"
  tar -xzf "$work/docker.tgz" -C "$work"
  # Not containerd, ctr or containerd-shim: the node's own are used.
  for binary in dockerd docker docker-proxy docker-init; do
    install -m 0755 "$work/docker/$binary" /usr/local/bin/$binary
  done
  current=$("$node_runc" --version 2>/dev/null | awk 'NR==1 {print $3}')
  bundled=$("$work/docker/runc" --version | awk 'NR==1 {print $3}')
  if [ "$current" != "$bundled" ]; then
    [ -z "$current" ] || [ -e "$node_runc.$current" ] || cp -p "$node_runc" "$node_runc.$current"
    install -m 0755 "$work/docker/runc" "$node_runc"
    echo "replaced runc ${current:-none} with $bundled; the old one is $node_runc.$current"
  fi
fi

mkdir -p /etc/docker
cat >/etc/docker/daemon.json <<JSON
{
  "containerd": "$containerd_socket",
  "exec-opts": ["native.cgroupdriver=systemd"],
  "features": { "containerd-snapshotter": false },
  "ip-forward-no-drop": true
}
JSON
# containerd-snapshotter off keeps the overlay2 storage driver, which cAdvisor's
# Docker support reads. ip-forward-no-drop stops Docker from setting the
# FORWARD chain's policy to DROP, which would cut pod traffic on this node.

getent group docker >/dev/null || groupadd --system docker
cat >/etc/systemd/system/docker.service <<UNIT
[Unit]
Description=Docker engine for ARIES Docker runs, on the node's containerd
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/dockerd --group docker
Restart=on-failure
LimitNOFILE=1048576
Delegate=yes
KillMode=process

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable docker.service >/dev/null
systemctl restart docker.service

# The runner reaches the daemon without sudo.
if [ -n "${SUDO_USER:-}" ] && ! id -nG "$SUDO_USER" | grep -qw docker; then
  usermod -aG docker "$SUDO_USER"
  echo "added $SUDO_USER to the docker group; log in again for it to apply"
fi

for _ in $(seq 1 30); do
  docker info >/dev/null 2>&1 && break
  sleep 1
done
docker info --format 'Docker {{.ServerVersion}}, storage {{.Driver}}, cgroup driver {{.CgroupDriver}}'
