#!/usr/bin/env bash
# Build a whole Kubernetes cluster from one machine over SSH.
#
# Reads a topology file naming the master and workers, copies the per-node
# scripts to each host, runs install-master.sh on the control plane, then joins
# every worker in parallel with the token the master produced.
#
# Usage:
#   cp cluster.conf.example cluster.conf     # then edit
#   ./install-cluster.sh --config cluster.conf
#   ./install-cluster.sh --config cluster.conf --check     # preflight only
#
# Options:
#   --config FILE   Topology file. Default: ./cluster.conf
#   --check         Verify SSH and passwordless sudo on every node, then stop
#                   without changing anything.
#   --skip-master   Only join workers against an already-initialized master.
#   --reset         Tear every node back to a pre-kubeadm state first, then
#                   rebuild. Required when re-running against a live cluster:
#                   `kubeadm init` refuses to touch an initialized control
#                   plane. DESTROYS all cluster state, including the contents
#                   of any hostPath volumes backing PersistentVolumes.
#
# Requires from this machine: ssh and scp to every node, and passwordless sudo
# on every node. Nothing is installed locally.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG="$SCRIPT_DIR/cluster.conf"
CHECK_ONLY=0
SKIP_MASTER=0
RESET=0
REMOTE_DIR=".aries-k8s-install"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '\033[1;33m warn\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror\033[0m %s\n' "$*" >&2; exit 1; }

usage() { awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "${BASH_SOURCE[0]}"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --config) CONFIG="$2"; shift 2 ;;
    --check) CHECK_ONLY=1; shift ;;
    --skip-master) SKIP_MASTER=1; shift ;;
    --reset) RESET=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument '$1' (try --help)" ;;
  esac
done

# --- configuration ------------------------------------------------------------

[ -f "$CONFIG" ] || die "config '$CONFIG' not found; start from cluster.conf.example"

MASTER=""; SSH_KEY=""; SSH_OPTS=""; CNI=""; POD_CIDR=""; K8S_VERSION=""
ADVERTISE_ADDRESS=""; SINGLE_NODE=0; OPEN_FIREWALL=0; FETCH_KUBECONFIG=0
TAINT_NODES=1
WORKERS=(); ARIES_NODES=(); HARNESS_NODES=(); SANDBOX_NODES=()
# shellcheck source=cluster.conf.example
. "$CONFIG"

[ -n "$MASTER" ] || die "MASTER is not set in $CONFIG"

ROLES="aries harness sandbox"
TAINT_KEY="aries.dev/role"

# role_nodes prints the SSH targets configured for one role. Expansions go
# through the `${x[@]+...}` guard because an empty array trips `set -u` under
# bash 3.2, which is what macOS ships.
role_nodes() {
  case "$1" in
    aries)   printf '%s\n' ${ARIES_NODES[@]+"${ARIES_NODES[@]}"} ;;
    harness) printf '%s\n' ${HARNESS_NODES[@]+"${HARNESS_NODES[@]}"} ;;
    sandbox) printf '%s\n' ${SANDBOX_NODES[@]+"${SANDBOX_NODES[@]}"} ;;
    plain)   printf '%s\n' ${WORKERS[@]+"${WORKERS[@]}"} ;;
    *) die "unknown role '$1'" ;;
  esac
}

# real_workers is the deduplicated union of every pool, minus the master. That
# makes listing the master under WORKERS harmless, and lets a node appear in
# more than one pool without being installed twice.
real_workers() {
  local node seen=""
  for node in $(role_nodes aries) $(role_nodes harness) $(role_nodes sandbox) $(role_nodes plain); do
    [ -n "$node" ] || continue
    if [ "$node" = "$MASTER" ]; then
      continue
    fi
    case " $seen " in
      *" $node "*) continue ;;
    esac
    seen="$seen $node"
    printf '%s\n' "$node"
  done
}

SSH_BASE="-o BatchMode=yes -o StrictHostKeyChecking=accept-new $SSH_OPTS"
if [ -n "$SSH_KEY" ]; then
  SSH_BASE="$SSH_BASE -i $SSH_KEY"
fi

# remote runs a command on one node with stdin detached. The -n matters: ssh
# forwards stdin by default, so a remote call inside a loop will swallow the
# loop's own input and hang. Only stage_scripts needs a stdin channel, and it
# uses remote_stdin.
# shellcheck disable=SC2086  # SSH_BASE is intentionally word-split into options
remote() { ssh -n $SSH_BASE "$1" "${@:2}"; }

# shellcheck disable=SC2086
remote_stdin() { ssh $SSH_BASE "$1" "${@:2}"; }

# --- preflight ----------------------------------------------------------------

# check_node fails loudly for one host so a broken node is reported before any
# other host has been modified.
check_node() {
  local node="$1" role="$2"
  if ! remote "$node" true 2>/dev/null; then
    die "cannot ssh to $role '$node' (non-interactively). Check the address, your key, and ssh-agent."
  fi
  if ! remote "$node" 'sudo -n true' 2>/dev/null; then
    die "no passwordless sudo on $role '$node'. Required, because the install runs unattended."
  fi
  local arch
  arch="$(remote "$node" 'uname -m' 2>/dev/null || echo unknown)"
  info "$role $node — reachable, sudo ok, $arch"
}

# node_role reports which pool an SSH target belongs to. Last match wins, which
# is the same precedence apply_node_roles produces by applying roles in order.
node_role() {
  local node="$1" role candidate found="worker"
  for role in $ROLES; do
    for candidate in $(role_nodes "$role"); do
      if [ "$candidate" = "$node" ]; then
        found="$role"
      fi
    done
  done
  printf '%s' "$found"
}

preflight() {
  local node count=0
  for node in $(real_workers); do
    count=$((count + 1))
  done
  log "preflight: $((count + 1)) node(s)"
  check_node "$MASTER" "master"
  for node in $(real_workers); do
    check_node "$node" "$(node_role "$node")"
  done
}

# --- staging ------------------------------------------------------------------

# stage_scripts ships the per-node installers to one host. tar over ssh avoids
# needing the destination to exist first and keeps the exec bits.
stage_scripts() {
  local node="$1"
  tar -C "$SCRIPT_DIR" -cf - common.sh install-master.sh install-worker.sh reset-node.sh \
    | remote_stdin "$node" "rm -rf $REMOTE_DIR && mkdir -p $REMOTE_DIR && tar -C $REMOTE_DIR -xf -"
}

# node_env renders the config as environment assignments for the remote script.
# Empty values are omitted so the scripts fall back to their own defaults.
# Every branch is a full `if` because `[ test ] && assign` returns non-zero when
# the test fails, which `set -e` treats as a fatal error.
node_env() {
  local out=""
  if [ -n "$CNI" ]; then
    out="$out CNI=$(printf '%q' "$CNI")"
  fi
  if [ -n "$POD_CIDR" ]; then
    out="$out POD_CIDR=$(printf '%q' "$POD_CIDR")"
  fi
  if [ -n "$K8S_VERSION" ]; then
    out="$out K8S_VERSION=$(printf '%q' "$K8S_VERSION")"
  fi
  if [ "$OPEN_FIREWALL" = "1" ]; then
    out="$out OPEN_FIREWALL=1"
  fi
  printf '%s' "$out"
}

# --- master -------------------------------------------------------------------

install_master() {
  log "staging scripts on master"
  stage_scripts "$MASTER"

  local env_args master_args
  env_args="$(node_env)"
  master_args=""
  if [ -n "$ADVERTISE_ADDRESS" ]; then
    master_args="$master_args --advertise-address $ADVERTISE_ADDRESS"
  fi
  if [ "$SINGLE_NODE" = "1" ]; then
    master_args="$master_args --single-node"
  fi

  log "installing control plane on $MASTER (this takes several minutes)"
  remote "$MASTER" "sudo env$env_args $REMOTE_DIR/install-master.sh$master_args"
}

# join_command mints a fresh 24h token rather than reusing the one the master
# script wrote, so re-running this against an existing cluster still works.
join_command() {
  local raw
  raw="$(remote "$MASTER" 'sudo kubeadm token create --print-join-command' 2>/dev/null | tail -n1)"
  raw="${raw#sudo }"
  case "$raw" in
    kubeadm\ join\ *) printf '%s' "$raw" ;;
    *) die "master did not return a usable join command (got: ${raw:-<empty>})" ;;
  esac
}

# --- workers ------------------------------------------------------------------

# install_workers runs every worker concurrently; each writes to its own log so
# interleaved output does not become unreadable. Failures are collected and
# reported together rather than aborting the first time one node breaks.
install_workers() {
  local join="$1"
  local logdir node pid
  logdir="$(mktemp -d)"

  local nodes=() pids=()
  for node in $(real_workers); do
    nodes+=("$node")
  done

  if [ "${#nodes[@]}" -eq 0 ]; then
    warn "no worker nodes configured"
    return 0
  fi

  log "joining ${#nodes[@]} worker(s) in parallel"
  local env_args
  env_args="$(node_env)"
  for node in "${nodes[@]}"; do
    (
      stage_scripts "$node"
      remote "$node" "sudo env$env_args $REMOTE_DIR/install-worker.sh --join $(printf '%q' "$join")"
    ) >"$logdir/$(printf '%s' "$node" | tr -c 'A-Za-z0-9._-' '_').log" 2>&1 &
    pids+=("$!")
    info "started $node"
  done

  local i=0 failed=0
  for pid in "${pids[@]}"; do
    node="${nodes[$i]}"
    if wait "$pid"; then
      info "joined  $node"
    else
      warn "FAILED  $node — log below"
      sed 's/^/        /' "$logdir/$(printf '%s' "$node" | tr -c 'A-Za-z0-9._-' '_').log" | tail -25 >&2
      failed=$((failed + 1))
    fi
    i=$((i + 1))
  done

  [ "$failed" -eq 0 ] || die "$failed worker(s) failed to join; logs in $logdir"
  rm -rf "$logdir"
}

# --- node roles ---------------------------------------------------------------

# KUBECTL is resolved once: the invoking user may already have a kubeconfig, and
# if not we fall back to the admin one through sudo.
KUBECTL=""
resolve_kubectl() {
  if remote "$MASTER" 'kubectl get nodes >/dev/null 2>&1'; then
    KUBECTL="kubectl"
  else
    KUBECTL="sudo KUBECONFIG=/etc/kubernetes/admin.conf kubectl"
  fi
}

kube() { remote "$MASTER" "$KUBECTL $*"; }

# node_k8s_name asks the node what the kubelet registered it as. The SSH target
# is not usable here: on CloudLab you reach a host as clnodeNNN.<site> while it
# joins the cluster as nodeN.<experiment>.<project>.<site>.
node_k8s_name() {
  remote "$1" 'hostname' | tr 'A-Z' 'a-z' | tr -d '\r' | head -n1
}

# apply_node_roles labels and taints each pool. The label is what pods select
# with nodeSelector; the taint is what keeps everything else away. Both are
# --overwrite so re-running the installer is idempotent.
apply_node_roles() {
  if [ "$TAINT_NODES" != "1" ]; then
    log "TAINT_NODES=0 — every node joined as a plain worker, no labels or taints"
    return 0
  fi

  resolve_kubectl
  log "labelling and tainting nodes by role"

  # The master keeps kubeadm's own control-plane taint; it only gains a label so
  # it can be selected explicitly and shows up in role listings.
  local master_name
  master_name="$(node_k8s_name "$MASTER")"
  kube "label node $master_name $TAINT_KEY=master --overwrite" >/dev/null
  info "master   $master_name"

  local role node name tainted=0
  for role in $ROLES; do
    for node in $(role_nodes "$role"); do
      [ -n "$node" ] || continue
      name="$(node_k8s_name "$node")"
      if [ -z "$name" ]; then
        die "could not resolve the Kubernetes node name for '$node'"
      fi
      kube "label node $name $TAINT_KEY=$role --overwrite" >/dev/null
      kube "taint node $name $TAINT_KEY=$role:NoSchedule --overwrite" >/dev/null
      # Cosmetic only: `kubectl get nodes` builds its ROLES column solely from
      # node-role.kubernetes.io/* labels, so the custom one above never shows
      # there. This makes the pool visible without needing -L. Scheduling still
      # keys off $TAINT_KEY; the value here is empty by convention.
      kube "label node $name node-role.kubernetes.io/$role= --overwrite" >/dev/null
      info "$(printf '%-8s' "$role") $name"
      tainted=$((tainted + 1))
    done
  done

  if [ "$tainted" -eq 0 ]; then
    warn "no role pools configured; all workers are general-purpose"
    return 0
  fi

  cat <<EOF

  Pods must now carry BOTH a nodeSelector and a toleration to land on a
  dedicated node, for example:

    nodeSelector:
      $TAINT_KEY: sandbox
    tolerations:
      - key: $TAINT_KEY
        operator: Equal
        value: sandbox
        effect: NoSchedule

EOF
}

# --- finish -------------------------------------------------------------------

fetch_kubeconfig() {
  [ "$FETCH_KUBECONFIG" = "1" ] || return 0
  log "fetching kubeconfig to ./kubeconfig"
  remote "$MASTER" 'sudo cat /etc/kubernetes/admin.conf' >kubeconfig
  chmod 600 kubeconfig
  info "export KUBECONFIG=$PWD/kubeconfig"
  info "the server address is the master's advertise address; it must be routable from here"
}

# reset_nodes tears every configured node back to a pre-kubeadm state. Scripts
# are staged first because reset-node.sh travels with them, and a node that was
# never installed may not have a copy yet.
#
# Workers are reset before the master, and sequentially. They hold leases on the
# control plane, so tearing the API server down first can leave a worker's
# `kubeadm reset` blocking on a server that no longer answers.
#
# A failing reset is a warning, not a fatal error: a node that was never part of
# a cluster has nothing to remove and reports failure for that reason alone.
reset_nodes() {
  local node
  log "resetting every node (--reset)"
  for node in $(real_workers) "$MASTER"; do
    info "reset $node"
    stage_scripts "$node"
    if ! remote "$node" "sudo $REMOTE_DIR/reset-node.sh --yes" >/dev/null 2>&1; then
      warn "reset reported errors on $node; continuing"
    fi
  done
}

# --- main ---------------------------------------------------------------------

preflight

if [ "$CHECK_ONLY" = "1" ]; then
  log "preflight passed; --check requested, stopping without changes"
  exit 0
fi

if [ "$RESET" = "1" ]; then
  reset_nodes
fi

if [ "$SKIP_MASTER" = "0" ]; then
  install_master
else
  log "--skip-master: using the existing control plane on $MASTER"
fi

install_workers "$(join_command)"
apply_node_roles

log "cluster status"
[ -n "$KUBECTL" ] || resolve_kubectl
kube "get nodes -o wide -L $TAINT_KEY"

fetch_kubeconfig

cat <<EOF

Cluster is up.

Nodes report Ready once the CNI DaemonSet has scheduled on each of them; that
can lag the join by a minute. Watch it with:

  ssh $MASTER kubectl get nodes -w

Then deploy ARIES (see ../README.md):

  kubectl apply -k k8s/overlays/incluster
EOF
