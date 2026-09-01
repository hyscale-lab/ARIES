#!/usr/bin/env bash
# Bootstrap a Kubernetes worker node for ARIES and join it to an existing
# control plane.
#
# Usage:
#   sudo ./install-worker.sh --join "kubeadm join 10.0.0.1:6443 --token ... --discovery-token-ca-cert-hash sha256:..."
#
#   sudo ./install-worker.sh --api-server 10.0.0.1:6443 \
#                            --token abcdef.0123456789abcdef \
#                            --discovery-hash sha256:<hash>
#
# Get the join command from the control-plane node with:
#   sudo kubeadm token create --print-join-command
#
# Options:
#   --cni flannel|calico|none   Match the control plane (firewall ports only).
#   --open-firewall             Open the worker ports in ufw/firewalld.
#   --k8s-version 1.34.2        Pin the same version as the control plane.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

JOIN_COMMAND="${JOIN_COMMAND:-}"
API_SERVER="${API_SERVER:-}"
JOIN_TOKEN="${JOIN_TOKEN:-}"
DISCOVERY_HASH="${DISCOVERY_HASH:-}"

# usage prints the leading comment block of this file.
usage() { awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --join) JOIN_COMMAND="$2"; shift 2 ;;
    --api-server) API_SERVER="$2"; shift 2 ;;
    --token) JOIN_TOKEN="$2"; shift 2 ;;
    --discovery-hash) DISCOVERY_HASH="$2"; shift 2 ;;
    --cni) CNI="$2"; shift 2 ;;
    --k8s-version) K8S_VERSION="$2"; shift 2 ;;
    --open-firewall) OPEN_FIREWALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" ;;
  esac
done

if [ -z "$JOIN_COMMAND" ]; then
  if [ -n "$API_SERVER" ] && [ -n "$JOIN_TOKEN" ] && [ -n "$DISCOVERY_HASH" ]; then
    JOIN_COMMAND="kubeadm join ${API_SERVER} --token ${JOIN_TOKEN} --discovery-token-ca-cert-hash ${DISCOVERY_HASH}"
  else
    die "need --join \"<command>\", or all of --api-server, --token and --discovery-hash (try --help)"
  fi
fi

case "$JOIN_COMMAND" in
  kubeadm\ join\ *) ;;
  *) die "--join must be a full 'kubeadm join ...' command" ;;
esac

if [ -f /etc/kubernetes/kubelet.conf ]; then
  die "this node is already joined to a cluster; run reset-node.sh first to re-join"
fi

prepare_node
open_firewall worker

log "joining the cluster"
# The control plane's socket flag may be absent from an older join command, so
# it is appended unless the caller already set one.
case "$JOIN_COMMAND" in
  *--cri-socket*) ;;
  *) JOIN_COMMAND="$JOIN_COMMAND --cri-socket unix:///run/containerd/containerd.sock" ;;
esac

# Word splitting is intended: the join command is a full command line.
# shellcheck disable=SC2086
eval $JOIN_COMMAND

cat <<EOF

Worker joined. Verify from the control-plane node:

  kubectl get nodes -o wide

The node reports Ready once the CNI DaemonSet pod schedules on it.
EOF
