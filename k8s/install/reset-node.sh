#!/usr/bin/env bash
# Tear a node back down to a pre-kubeadm state so it can be re-bootstrapped.
#
# Usage:
#   sudo ./reset-node.sh --yes
#
# This destroys all cluster state on this node: etcd data on a control plane,
# every running pod, the CNI configuration and the local kubeconfigs. It does
# not uninstall containerd or the kube packages.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

CONFIRMED=0
while [ $# -gt 0 ]; do
  case "$1" in
    --yes|-y) CONFIRMED=1; shift ;;
    -h|--help) awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" ;;
  esac
done

require_root
[ "$CONFIRMED" = "1" ] || die "refusing to reset without --yes; this destroys all cluster state on $(hostname)"

log "running kubeadm reset on $(hostname)"
kubeadm reset -f --cri-socket unix:///run/containerd/containerd.sock || \
  warn "kubeadm reset reported an error; continuing with manual cleanup"

log "removing CNI, kubelet and kubeconfig state"
rm -rf /etc/cni/net.d /var/lib/cni /var/lib/kubelet/* /etc/kubernetes /etc/aries/kubeadm-join.sh
rm -f /root/.kube/config
if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
  home="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
  if [ -n "$home" ]; then
    rm -f "$home/.kube/config"
  fi
fi

# kubeadm reset leaves the CNI's bridge interfaces behind.
for link in cni0 flannel.1 vxlan.calico; do
  if ip link show "$link" >/dev/null 2>&1; then
    log "deleting stale interface $link"
    ip link delete "$link"
  fi
done

if command -v iptables >/dev/null 2>&1; then
  log "flushing iptables rules left by kube-proxy"
  iptables -F && iptables -t nat -F && iptables -t mangle -F && iptables -X || true
fi
command -v ipvsadm >/dev/null 2>&1 && ipvsadm -C || true

systemctl restart containerd || true

log "node reset; re-run install-master.sh or install-worker.sh to bootstrap again"
