#!/usr/bin/env bash
# Shared node preparation for the ARIES kubeadm bootstrap scripts.
#
# This file is sourced by install-master.sh, install-worker.sh and
# reset-node.sh. It never acts on its own; sourcing only defines functions and
# resolves configuration.
#
# Everything is downloaded from the upstream projects at run time:
#   - Kubernetes packages  -> https://pkgs.k8s.io  (community-owned repos)
#   - containerd runtime   -> https://download.docker.com
#   - CNI manifests        -> project GitHub releases
#
# Configuration (environment variables, all optional):
#   K8S_MINOR       Package channel, e.g. "v1.34". Default: resolved online
#                   from https://dl.k8s.io/release/stable.txt.
#   K8S_VERSION     Exact patch to pin, e.g. "1.34.2". Default: repo latest.
#   CNI             flannel | calico | none. Default: flannel.
#   POD_CIDR        Pod network CIDR. Default: per-CNI (see resolve_pod_cidr).
#   OPEN_FIREWALL   1 to open the required ports in ufw/firewalld. Default: 0.

set -euo pipefail

K8S_STABLE_URL="https://dl.k8s.io/release/stable.txt"

CNI="${CNI:-flannel}"
OPEN_FIREWALL="${OPEN_FIREWALL:-0}"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "run as root (sudo $0 ...)"
}

# detect_os sets OS_ID, OS_LIKE and PKG (apt|dnf).
detect_os() {
  [ -r /etc/os-release ] || die "cannot read /etc/os-release; unsupported host"
  # shellcheck disable=SC1091
  . /etc/os-release
  OS_ID="$ID"
  OS_LIKE="${ID_LIKE:-}"

  case "$OS_ID $OS_LIKE" in
    *debian*|*ubuntu*) PKG=apt ;;
    *rhel*|*fedora*|*centos*) PKG=dnf ;;
    *) die "unsupported distribution '$OS_ID'; expected a Debian- or RHEL-family host" ;;
  esac

  ARCH="$(uname -m)"
  case "$ARCH" in
    x86_64)  DEB_ARCH=amd64 ;;
    aarch64) DEB_ARCH=arm64 ;;
    *) die "unsupported architecture '$ARCH'; kubeadm nodes need x86_64 or aarch64" ;;
  esac

  log "host: $OS_ID ${VERSION_ID:-?} ($ARCH), package manager: $PKG"
}

# resolve_k8s_minor fills K8S_MINOR from the upstream stable pointer unless the
# caller pinned it. K8S_VERSION, when set, wins and decides the channel.
resolve_k8s_minor() {
  if [ -n "${K8S_MINOR:-}" ]; then
    log "kubernetes channel: $K8S_MINOR (from K8S_MINOR)"
    return
  fi

  local stable
  if [ -n "${K8S_VERSION:-}" ]; then
    stable="v${K8S_VERSION#v}"
  else
    stable="$(curl -fsSL --retry 3 "$K8S_STABLE_URL")" \
      || die "cannot reach $K8S_STABLE_URL; set K8S_MINOR to install offline of the pointer"
  fi

  # v1.34.2 -> v1.34
  K8S_MINOR="$(printf '%s' "$stable" | cut -d. -f1,2)"
  log "kubernetes channel: $K8S_MINOR (resolved from $stable)"
}

resolve_pod_cidr() {
  if [ -n "${POD_CIDR:-}" ]; then
    return
  fi
  case "$CNI" in
    flannel) POD_CIDR="10.244.0.0/16" ;;
    calico)  POD_CIDR="192.168.0.0/16" ;;
    none)    POD_CIDR="10.244.0.0/16" ;;
    *) die "unknown CNI '$CNI'; expected flannel, calico or none" ;;
  esac
}

pkg_refresh() {
  case "$PKG" in
    apt) DEBIAN_FRONTEND=noninteractive apt-get update -qq ;;
    dnf) : ;; # dnf refreshes metadata per transaction
  esac
}

pkg_install() {
  case "$PKG" in
    apt) DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@" ;;
    dnf) dnf install -y -q "$@" ;;
  esac
}

install_prerequisites() {
  log "installing prerequisites"
  pkg_refresh
  case "$PKG" in
    apt) pkg_install ca-certificates curl gnupg apt-transport-https ;;
    dnf) pkg_install ca-certificates curl gnupg2 iproute-tc ;;
  esac
}

# disable_swap is required: the kubelet refuses to start with swap on unless
# explicitly configured for it, and kubeadm preflight fails first.
disable_swap() {
  if [ "$(swapon --show --noheadings | wc -l)" -eq 0 ]; then
    log "swap already disabled"
  else
    log "disabling swap"
    swapoff -a
  fi
  # Comment out swap entries so the node survives a reboot.
  if grep -qE '^[^#].*\sswap\s' /etc/fstab 2>/dev/null; then
    cp /etc/fstab "/etc/fstab.aries.bak.$(date +%s)"
    sed -i -E 's|^([^#].*\sswap\s.*)$|# \1  # disabled by ARIES kubeadm bootstrap|' /etc/fstab
    log "commented swap entries in /etc/fstab (backup written alongside)"
  fi
}

configure_kernel() {
  log "configuring kernel modules and sysctls"
  cat >/etc/modules-load.d/k8s.conf <<'EOF'
overlay
br_netfilter
EOF
  modprobe overlay
  modprobe br_netfilter

  cat >/etc/sysctl.d/99-kubernetes.conf <<'EOF'
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
  sysctl --system >/dev/null
}

# install_containerd pulls containerd.io from Docker's repository, which ships a
# build that matches the CRI version kubeadm expects, then switches the cgroup
# driver to systemd to agree with the kubelet default.
install_containerd() {
  if command -v containerd >/dev/null 2>&1; then
    log "containerd already installed ($(containerd --version | awk '{print $3}'))"
  else
    log "installing containerd from download.docker.com"
    case "$PKG" in
      apt)
        local docker_distro="$OS_ID"
        case "$OS_ID" in
          ubuntu|debian) ;;
          *) case "$OS_LIKE" in *ubuntu*) docker_distro=ubuntu ;; *) docker_distro=debian ;; esac ;;
        esac
        install -m 0755 -d /etc/apt/keyrings
        curl -fsSL --retry 3 "https://download.docker.com/linux/${docker_distro}/gpg" \
          | gpg --dearmor --yes -o /etc/apt/keyrings/docker.gpg
        chmod a+r /etc/apt/keyrings/docker.gpg
        cat >/etc/apt/sources.list.d/docker.list <<EOF
deb [arch=${DEB_ARCH} signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/${docker_distro} ${VERSION_CODENAME} stable
EOF
        pkg_refresh
        pkg_install containerd.io
        ;;
      dnf)
        local docker_repo="centos"
        case "$OS_ID" in fedora) docker_repo=fedora ;; esac
        curl -fsSL --retry 3 -o /etc/yum.repos.d/docker-ce.repo \
          "https://download.docker.com/linux/${docker_repo}/docker-ce.repo"
        pkg_install containerd.io
        ;;
    esac
  fi

  log "writing containerd CRI configuration (SystemdCgroup=true)"
  mkdir -p /etc/containerd
  containerd config default >/etc/containerd/config.toml
  # Both the v2 and v3 plugin paths are matched so this holds across containerd
  # major versions.
  sed -i 's/^\( *\)SystemdCgroup = false/\1SystemdCgroup = true/' /etc/containerd/config.toml
  grep -q 'SystemdCgroup = true' /etc/containerd/config.toml \
    || warn "SystemdCgroup not found in containerd config; verify the cgroup driver manually"

  systemctl daemon-reload
  systemctl enable --now containerd
  systemctl restart containerd

  # crictl ships with the kube packages; point it at containerd so debugging on
  # the node works without repeating the endpoint flag.
  cat >/etc/crictl.yaml <<'EOF'
runtime-endpoint: unix:///run/containerd/containerd.sock
image-endpoint: unix:///run/containerd/containerd.sock
timeout: 10
EOF
}

install_kube_packages() {
  if command -v kubeadm >/dev/null 2>&1; then
    log "kubeadm already installed ($(kubeadm version -o short))"
    return
  fi

  log "installing kubelet, kubeadm and kubectl from pkgs.k8s.io ($K8S_MINOR)"
  case "$PKG" in
    apt)
      install -m 0755 -d /etc/apt/keyrings
      curl -fsSL --retry 3 "https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/deb/Release.key" \
        | gpg --dearmor --yes -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
      chmod a+r /etc/apt/keyrings/kubernetes-apt-keyring.gpg
      cat >/etc/apt/sources.list.d/kubernetes.list <<EOF
deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/deb/ /
EOF
      pkg_refresh
      if [ -n "${K8S_VERSION:-}" ]; then
        local v="${K8S_VERSION#v}"
        # pkgs.k8s.io debs carry a -1.1 packaging suffix.
        pkg_install "kubelet=${v}-*" "kubeadm=${v}-*" "kubectl=${v}-*"
      else
        pkg_install kubelet kubeadm kubectl
      fi
      apt-mark hold kubelet kubeadm kubectl >/dev/null
      ;;
    dnf)
      cat >/etc/yum.repos.d/kubernetes.repo <<EOF
[kubernetes]
name=Kubernetes
baseurl=https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/rpm/
enabled=1
gpgcheck=1
gpgkey=https://pkgs.k8s.io/core:/stable:/${K8S_MINOR}/rpm/repodata/repomd.xml.key
exclude=kubelet kubeadm kubectl cri-tools kubernetes-cni
EOF
      if [ -n "${K8S_VERSION:-}" ]; then
        local v="${K8S_VERSION#v}"
        dnf install -y -q --disableexcludes=kubernetes "kubelet-${v}" "kubeadm-${v}" "kubectl-${v}"
      else
        dnf install -y -q --disableexcludes=kubernetes kubelet kubeadm kubectl
      fi
      ;;
  esac

  systemctl enable --now kubelet
}

# open_firewall opens the ports kubeadm needs when a host firewall is active.
# Ports are per role: "control-plane" or "worker".
open_firewall() {
  local role="$1"
  local tcp udp
  case "$role" in
    control-plane) tcp="6443 2379:2380 10250 10256 10257 10259 30000:32767" ;;
    worker)        tcp="10250 10256 30000:32767" ;;
    *) die "open_firewall: unknown role '$role'" ;;
  esac
  udp=""
  case "$CNI" in
    flannel) udp="8472" ;;
    calico)  tcp="$tcp 179" ;;
  esac

  if [ "$OPEN_FIREWALL" != "1" ]; then
    log "firewall untouched (set OPEN_FIREWALL=1 to open: tcp ${tcp}${udp:+, udp $udp})"
    return
  fi

  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
    log "opening ports with ufw"
    local p
    for p in $tcp; do ufw allow "$p/tcp" >/dev/null; done
    for p in $udp; do ufw allow "$p/udp" >/dev/null; done
  elif command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    log "opening ports with firewalld"
    local p
    for p in $tcp; do firewall-cmd --permanent --add-port="${p/:/-}/tcp" >/dev/null; done
    for p in $udp; do firewall-cmd --permanent --add-port="$p/udp" >/dev/null; done
    firewall-cmd --reload >/dev/null
  else
    log "no active ufw/firewalld found; nothing to open"
  fi
}

# prepare_node runs every step both roles share.
prepare_node() {
  require_root
  detect_os
  resolve_k8s_minor
  resolve_pod_cidr
  install_prerequisites
  disable_swap
  configure_kernel
  install_containerd
  install_kube_packages
}
