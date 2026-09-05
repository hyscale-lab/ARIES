#!/usr/bin/env bash
# Bootstrap a Kubernetes control-plane node for ARIES with kubeadm.
#
# Usage:
#   sudo ./install-master.sh [--advertise-address IP] [--control-plane-endpoint HOST:PORT]
#                            [--cni flannel|calico|none] [--pod-cidr CIDR]
#                            [--single-node] [--open-firewall]
#
# Everything is fetched online: kube packages from pkgs.k8s.io, containerd from
# download.docker.com, control-plane images from registry.k8s.io, and the CNI
# manifest from the project's GitHub release.
#
# On success the join command for workers is printed and written to
# /etc/aries/kubeadm-join.sh on this node.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=common.sh
. "$SCRIPT_DIR/common.sh"

ADVERTISE_ADDRESS="${ADVERTISE_ADDRESS:-}"
CONTROL_PLANE_ENDPOINT="${CONTROL_PLANE_ENDPOINT:-}"
SINGLE_NODE="${SINGLE_NODE:-0}"
JOIN_FILE="/etc/aries/kubeadm-join.sh"

# usage prints the leading comment block of this file.
usage() { awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --advertise-address) ADVERTISE_ADDRESS="$2"; shift 2 ;;
    --control-plane-endpoint) CONTROL_PLANE_ENDPOINT="$2"; shift 2 ;;
    --cni) CNI="$2"; shift 2 ;;
    --pod-cidr) POD_CIDR="$2"; shift 2 ;;
    --k8s-version) K8S_VERSION="$2"; shift 2 ;;
    --single-node) SINGLE_NODE=1; shift ;;
    --open-firewall) OPEN_FIREWALL=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" ;;
  esac
done

# default_advertise_address picks the address of the route to the default
# gateway, which is the interface workers will reach this node on.
default_advertise_address() {
  ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1;i<NF;i++) if ($i=="src") {print $(i+1); exit}}'
}

# kubectl_admin runs kubectl against the freshly written admin kubeconfig. The
# invoking user's kubeconfig is installed later; this keeps the script working
# before that happens.
kubectl_admin() {
  KUBECONFIG=/etc/kubernetes/admin.conf kubectl "$@"
}

# install_kubeconfig makes the cluster usable as root and as the user who
# invoked sudo.
install_kubeconfig() {
  install -m 0700 -d /root/.kube
  install -m 0600 /etc/kubernetes/admin.conf /root/.kube/config

  if [ -n "${SUDO_USER:-}" ] && [ "$SUDO_USER" != root ]; then
    local home group
    home="$(getent passwd "$SUDO_USER" | cut -d: -f6)"
    # Not every site gives a user a like-named group (CloudLab puts users in a
    # shared project group), so ask for the primary group rather than assuming.
    group="$(id -gn "$SUDO_USER" 2>/dev/null || echo "$SUDO_USER")"
    if [ -n "$home" ] && [ -d "$home" ]; then
      install -m 0700 -o "$SUDO_USER" -g "$group" -d "$home/.kube"
      install -m 0600 -o "$SUDO_USER" -g "$group" /etc/kubernetes/admin.conf "$home/.kube/config"
      log "kubeconfig installed for $SUDO_USER at $home/.kube/config"
    fi
  fi
}

wait_for_api() {
  log "waiting for the API server to serve requests"
  local i
  for i in $(seq 1 60); do
    if kubectl_admin get --raw='/readyz' >/dev/null 2>&1; then
      return 0
    fi
    sleep 5
  done
  die "API server did not become ready; inspect 'journalctl -u kubelet' and 'crictl ps -a'"
}

install_cni() {
  case "$CNI" in
    none)
      warn "no CNI installed; nodes stay NotReady until you apply one"
      return
      ;;
    flannel)
      log "installing Flannel"
      [ "$POD_CIDR" = "10.244.0.0/16" ] \
        || warn "Flannel's manifest hard-codes 10.244.0.0/16; pod CIDR $POD_CIDR needs a patched manifest"
      kubectl_admin apply -f \
        https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml
      ;;
    calico)
      log "installing Calico (Tigera operator)"
      # Server-side apply: the operator bundle carries CRDs larger than the
      # client-side last-applied annotation limit, and it must stay re-runnable.
      kubectl_admin apply --server-side --force-conflicts -f \
        https://raw.githubusercontent.com/projectcalico/calico/v3.32.2/manifests/v1_crd_projectcalico_org.yaml
      kubectl_admin apply --server-side --force-conflicts -f \
        https://raw.githubusercontent.com/projectcalico/calico/v3.32.2/manifests/tigera-operator.yaml
      kubectl_admin wait --for=condition=Available --timeout=180s \
        -n tigera-operator deployment/tigera-operator
      kubectl_admin apply -f - <<EOF
apiVersion: operator.tigera.io/v1
kind: Installation
metadata:
  name: default
spec:
  calicoNetwork:
    ipPools:
      - name: default-ipv4-ippool
        cidr: ${POD_CIDR}
        encapsulation: VXLANCrossSubnet
---
apiVersion: operator.tigera.io/v1
kind: APIServer
metadata:
  name: default
spec: {}
EOF
      ;;
    *) die "unknown CNI '$CNI'" ;;
  esac
}

# write_join_command records a fresh 24h join command for the worker script.
write_join_command() {
  install -m 0700 -d "$(dirname "$JOIN_FILE")"
  {
    printf '#!/usr/bin/env bash\n# Generated by ARIES install-master.sh on %s.\n# Valid for 24h; regenerate with: kubeadm token create --print-join-command\n' \
      "$(date -Is)"
    kubeadm token create --print-join-command
  } >"$JOIN_FILE"
  chmod 0700 "$JOIN_FILE"
}

prepare_node
open_firewall control-plane

if [ -f /etc/kubernetes/admin.conf ]; then
  log "control plane already initialized; skipping kubeadm init"
else
  [ -n "$ADVERTISE_ADDRESS" ] || ADVERTISE_ADDRESS="$(default_advertise_address)"
  [ -n "$ADVERTISE_ADDRESS" ] || die "cannot determine the advertise address; pass --advertise-address"

  log "pre-pulling control-plane images"
  kubeadm config images pull --cri-socket unix:///run/containerd/containerd.sock

  log "running kubeadm init (advertise $ADVERTISE_ADDRESS, pod CIDR $POD_CIDR)"
  init_args=(
    --cri-socket unix:///run/containerd/containerd.sock
    --pod-network-cidr "$POD_CIDR"
    --apiserver-advertise-address "$ADVERTISE_ADDRESS"
  )
  if [ -n "${K8S_VERSION:-}" ]; then
    init_args+=(--kubernetes-version "v${K8S_VERSION#v}")
  fi
  if [ -n "$CONTROL_PLANE_ENDPOINT" ]; then
    init_args+=(--control-plane-endpoint "$CONTROL_PLANE_ENDPOINT" --upload-certs)
  fi
  kubeadm init "${init_args[@]}"
fi

install_kubeconfig
wait_for_api
install_cni

if [ "$SINGLE_NODE" = "1" ]; then
  log "removing the control-plane taint so workloads schedule on this node"
  kubectl_admin taint nodes --all node-role.kubernetes.io/control-plane- 2>/dev/null || true
fi

log "waiting for this node to become Ready"
kubectl_admin wait --for=condition=Ready node --all --timeout=300s || \
  warn "node not Ready yet; check 'kubectl get pods -A' for CNI pods"

write_join_command

kubectl_admin get nodes -o wide

cat <<EOF

Control plane is up.

Join a worker by running this on each worker node:

  $(tail -n1 "$JOIN_FILE")

Or copy k8s/install/ to the worker and run:

  sudo ./install-worker.sh --join "<the command above>"

The join command is also saved at $JOIN_FILE (root only, valid 24h).
Regenerate it with: kubeadm token create --print-join-command

Next, deploy ARIES itself:

  kubectl apply -k k8s/overlays/local
EOF
