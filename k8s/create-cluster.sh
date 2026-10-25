#!/usr/bin/env bash
# Builds a Kubernetes cluster with Invitro's setup, then applies ARIES to it.
#
# ARIES keeps no cluster bootstrap of its own. This script checks out Invitro at
# a pinned revision, runs its create_multinode_cluster against your nodes, and
# then runs k8s/apply-aries.sh on the master. Run it from your own machine; it
# reaches every node with plain `ssh`, as Invitro does, so your SSH config or
# agent must already allow passwordless logins.
set -euo pipefail

# This Invitro installs Kubernetes 1.34.3 through vHive v1.8.2. When the
# revision changes, bump bridge.kubernetes_base_image in configs/versions.json
# to the Kubernetes version it installs.
INVITRO_REPO=${INVITRO_REPO:-https://github.com/vhive-serverless/invitro.git}
INVITRO_REVISION=${INVITRO_REVISION:-49c90dfd977417a32ff4dbd767a8a82e794480b4}

config=""
prometheus=false
node_setup=""
skip_cluster=false
stage_runner=true
runner_dir=aries
ssh_key=""
ssh_options=()
housekeeping=""
# Empty until an option or the file decides; the default is to prune.
prune=""

usage() {
  cat <<'USAGE'
Usage: k8s/create-cluster.sh [options] [<master> <runner> [<worker> ...]]

Nodes are SSH targets such as user@host. As in Invitro, the first is the
control plane and the second is the loader node, which joins the cluster and is
where the ARIES runner is staged; the rest are workers.

With no nodes on the command line, they and the settings below are read from a
JSON file: k8s/cluster.json, or the one --config names. Start from
k8s/cluster.json.example. Command-line options win over the file.

  --config FILE       read nodes and settings from FILE
  --prometheus        also deploy kube-prometheus-stack and the ARIES overlay
  --node-setup FILE   an Invitro node_setup.json, for nodes whose internal
                      addresses are not Invitro's default 10.0.1.x layout
  --housekeeping DUR  set the kubelet's --housekeeping-interval on every node,
                      such as 1s; Invitro leaves the 10s default
  --keep-invitro      keep the Invitro components ARIES does not use, such as
                      Knative, Istio and MetalLB; by default they are removed
  --prune-invitro     remove them even if the config file says to keep them
  --skip-cluster      the cluster already exists; only apply ARIES
  --no-runner         do not build and stage the runner on the loader node
  -h, --help          show this help

INVITRO_REPO and INVITRO_REVISION override the pinned Invitro checkout.
USAGE
}

nodes=()
while [ $# -gt 0 ]; do
  case "$1" in
    --config) config=$2; shift 2 ;;
    --prometheus) prometheus=true; shift ;;
    --node-setup) node_setup=$2; shift 2 ;;
    --housekeeping) housekeeping=$2; shift 2 ;;
    --prune-invitro) prune=true; shift ;;
    --keep-invitro) prune=false; shift ;;
    --skip-cluster) skip_cluster=true; shift ;;
    --no-runner) stage_runner=false; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
    *) nodes+=("$1"); shift ;;
  esac
done
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

for tool in git go ssh python3; do
  command -v "$tool" >/dev/null || { echo "$tool is required on PATH" >&2; exit 1; }
done

# The file supplies whatever the command line left unset.
if [ -z "$config" ] && [ ${#nodes[@]} -eq 0 ] && [ -f "$root/k8s/cluster.json" ]; then
  config="$root/k8s/cluster.json"
fi
if [ -n "$config" ]; then
  [ -f "$config" ] || { echo "cluster config $config not found" >&2; exit 1; }
  echo "==> Reading $config"
  # Values are shell-quoted by Python, so nothing in the file is interpreted.
  parsed=$(python3 - "$config" <<'PY'
import json, shlex, sys

with open(sys.argv[1]) as handle:
    cfg = json.load(handle)

def text(key):
    value = cfg.get(key, "")
    if not isinstance(value, str):
        sys.exit(f"{key} must be a string")
    return value

def strings(key):
    value = cfg.get(key, [])
    if not isinstance(value, list) or not all(isinstance(item, str) for item in value):
        sys.exit(f"{key} must be a list of strings")
    return value

if not text("master") or not text("runner"):
    sys.exit('cluster config needs "master" and "runner"')
nodes = [text("master"), text("runner")] + strings("workers")
print("file_nodes=(" + " ".join(shlex.quote(node) for node in nodes) + ")")
print("file_ssh_options=(" + " ".join(shlex.quote(option) for option in strings("ssh_options")) + ")")
for key in ("runner_dir", "ssh_key", "node_setup", "kubelet_housekeeping_interval"):
    print(f"file_{key}={shlex.quote(text(key))}")
print("file_prometheus=" + ("true" if cfg.get("deploy_prometheus") is True else "false"))
print("file_prune=" + ("false" if cfg.get("prune_invitro") is False else "true"))
PY
  ) || exit 1
  eval "$parsed"
  [ ${#nodes[@]} -gt 0 ] || nodes=("${file_nodes[@]}")
  ssh_options=(${file_ssh_options[@]+"${file_ssh_options[@]}"})
  [ -z "$file_runner_dir" ] || runner_dir=$file_runner_dir
  ssh_key=$file_ssh_key
  [ -n "$node_setup" ] || node_setup=$file_node_setup
  [ "$prometheus" = true ] || prometheus=$file_prometheus
  [ -n "$prune" ] || prune=$file_prune
  [ -n "$housekeeping" ] || housekeeping=$file_kubelet_housekeeping_interval
fi
[ -n "$prune" ] || prune=true
case "$housekeeping" in
  ""|[1-9]s|[1-9][0-9]s|[1-9][0-9][0-9]ms) ;;
  *) echo "kubelet housekeeping interval must look like 1s or 500ms" >&2; exit 1 ;;
esac

[ ${#nodes[@]} -ge 2 ] || { usage >&2; exit 2; }
master=${nodes[0]}
runner=${nodes[1]}
case "$runner_dir" in
  ""|/*|*..*|*[!A-Za-z0-9._/-]*) echo "runner_dir must be a simple path under the runner's home" >&2; exit 1 ;;
esac

# Invitro calls plain `ssh`, so a key and options from the config are applied
# through a shim placed first on PATH for the rest of this script.
if [ -n "$ssh_key" ] || [ ${#ssh_options[@]} -gt 0 ]; then
  real_ssh=$(command -v ssh)
  shim=$(mktemp -d)
  {
    printf '#!/bin/sh\nexec %q' "$real_ssh"
    [ -z "$ssh_key" ] || printf ' -i %q -o IdentitiesOnly=yes' "${ssh_key/#\~/$HOME}"
    [ ${#ssh_options[@]} -eq 0 ] || printf ' %q' "${ssh_options[@]}"
    printf ' "$@"\n'
  } >"$shim/ssh"
  chmod +x "$shim/ssh"
  PATH="$shim:$PATH"
fi
cleanup() { rm -rf "${shim:-}" "${configs:-}" "${stage:-}" "${applied:-}"; }
trap cleanup EXIT

remote() { ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new "$@"; }
for node in "${nodes[@]}"; do
  remote "$node" true || { echo "cannot ssh to $node non-interactively" >&2; exit 1; }
done

if [ "$skip_cluster" = false ]; then
  invitro="$root/.cache/invitro"
  if [ ! -d "$invitro/.git" ]; then
    echo "==> Cloning Invitro into .cache/invitro"
    mkdir -p "$root/.cache"
    git clone -q "$INVITRO_REPO" "$invitro"
  fi
  echo "==> Checking out Invitro $INVITRO_REVISION"
  git -C "$invitro" fetch -q origin
  git -C "$invitro" checkout -q --detach "$INVITRO_REVISION"

  # Invitro reads its settings from a config directory. Work on a private copy
  # of its own defaults so the checkout stays pristine.
  configs=$(mktemp -d)
  cp -R "$invitro/scripts/setup/configs/." "$configs/"
  if [ "$prometheus" = true ]; then
    sed 's/"DEPLOY_PROMETHEUS": *false/"DEPLOY_PROMETHEUS": true/' \
      "$invitro/scripts/setup/configs/setup.json" >"$configs/setup.json"
    grep -q '"DEPLOY_PROMETHEUS": true' "$configs/setup.json" ||
      { echo "could not enable DEPLOY_PROMETHEUS in Invitro's setup.json" >&2; exit 1; }
  fi

  echo "==> Creating the cluster with Invitro (this takes a while)"
  if [ -n "$node_setup" ]; then
    cp "$node_setup" "$configs/node_setup.json"
    (cd "$invitro/scripts/setup" && go run . -setup-type create_multinode_cluster \
      -config-dir "$configs" -config node_setup.json)
  else
    (cd "$invitro/scripts/setup" && go run . -setup-type create_multinode_cluster \
      -config-dir "$configs" "${nodes[@]}")
  fi
fi

if [ -n "$housekeeping" ]; then
  # cAdvisor refreshes container counters on this interval, so it bounds how
  # fine pod measurements can be however often Prometheus scrapes them.
  echo "==> Setting the kubelet housekeeping interval to $housekeeping on every node"
  for node in "${nodes[@]}"; do
    remote "$node" "
      set -e
      file=/etc/default/kubelet
      if sudo grep -q -- '--housekeeping-interval=$housekeeping\\b' \$file 2>/dev/null; then exit 0; fi
      if sudo grep -q '^KUBELET_EXTRA_ARGS=' \$file 2>/dev/null; then
        sudo sed -i -E 's/ ?--housekeeping-interval=[^\" ]*//; s/^(KUBELET_EXTRA_ARGS=\".*)\"\$/\\1 --housekeeping-interval=$housekeeping\"/' \$file
      else
        echo 'KUBELET_EXTRA_ARGS=\"--housekeeping-interval=$housekeeping\"' | sudo tee -a \$file >/dev/null
      fi
      sudo systemctl restart kubelet
    "
  done
fi

echo "==> Applying ARIES on $master"
apply_args=(--runner-kubeconfig '$HOME/aries-kubeconfig')
[ "$prometheus" = false ] || apply_args+=(--monitoring)
[ "$prune" = false ] || apply_args+=(--prune-invitro)
# Ship the committed k8s/ tree only, so local credentials never leave this
# machine. Invitro installs helm with Prometheus; install it otherwise.
applied=$(mktemp)
git -C "$root" archive HEAD k8s | remote "$master" "
  set -e
  rm -rf ~/aries-k8s && mkdir -p ~/aries-k8s && tar -xf - -C ~/aries-k8s
  command -v helm >/dev/null ||
    curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
  ~/aries-k8s/k8s/apply-aries.sh ${apply_args[*]}
" | tee "$applied"
prometheus_url=$(sed -n 's/^  prometheus_url  *: //p' "$applied")

if [ "$stage_runner" = true ]; then
  case "$(remote "$runner" uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported runner architecture" >&2; exit 1 ;;
  esac
  echo "==> Building the runner for linux/$arch and staging it to $runner:~/$runner_dir"
  stage=$(mktemp -d)
  mkdir -p "$stage/bin"
  # The commit tags the Kubernetes bridge image the runner requests, as in
  # `make build`; k8s/publish-images.sh pushes it under the same tag.
  revision=$(git -C "$root" rev-parse --short=12 HEAD)
  git -C "$root" diff --quiet HEAD -- || revision="$revision-dirty"
  for command in aries aries-ssh-client aries-replay; do
    (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
      -ldflags "-X github.com/hyscale-lab/aries/internal/app/wiring/bridge.Revision=$revision" \
      -o "$stage/bin/$command" "./cmd/$command")
  done
  bridge_image=$(sed -n 's/.*"image": *"\(aries-bridge:[^"]*\)".*/\1/p' "$root/configs/versions.json" | head -1)
  echo "    the runner requests the bridge image ${bridge_image}-$revision;"
  echo "    publish it with: make k8s-images REGISTRY=<registry>"
  git -C "$root" archive HEAD profiles configs | tar -xf - -C "$stage"
  # Only bin/, profiles/ and configs/ are replaced; runs/, .cache/ and the
  # model key on the runner are kept.
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$stage" -cf - bin profiles configs | remote "$runner" "
    mkdir -p ~/$runner_dir && cd ~/$runner_dir && rm -rf bin profiles configs && tar -xf -"
  if [ -n "$prometheus_url" ]; then
    # The Service address differs per cluster, so it is written into the staged
    # Kubernetes profiles rather than committed.
    echo "==> Pointing the staged profiles at Prometheus ($prometheus_url)"
    remote "$runner" "python3 - ~/$runner_dir/profiles $prometheus_url" <<'PY'
import glob, json, os, sys

# Profiles live in subfolders too, such as profiles/kubernetes/.
for path in sorted(glob.glob(os.path.join(sys.argv[1], "**", "*.json"), recursive=True)):
    with open(path) as handle:
        profile = json.load(handle)
    placement = profile.get("sandbox", {}).get("deployment", {})
    if placement.get("backend") != "kubernetes":
        continue
    placement.setdefault("kubernetes", {})["prometheus_url"] = sys.argv[2]
    with open(path, "w") as handle:
        json.dump(profile, handle, indent=2)
        handle.write("\n")
PY
  fi
  # The token passes through a pipe, never a command line or a local file.
  remote "$master" 'cat ~/aries-kubeconfig' | remote "$runner" "umask 077; rm -f ~/$runner_dir/kubeconfig; cat > ~/$runner_dir/kubeconfig"
fi

echo
echo "Cluster ready. Next:"
echo "  1. Publish the bridge image for the Kubernetes version printed above (k8s/README.md)."
echo "  2. Set pod_cidr and service_cidr in the profile to the values printed above."
if [ "$stage_runner" = true ]; then
  echo "  3. scp DEEPSEEK_API.key $runner:$runner_dir/ && ssh $runner chmod 600 $runner_dir/DEEPSEEK_API.key"
  echo "  4. ssh $runner 'cd ~/$runner_dir && KUBECONFIG=\$PWD/kubeconfig ./bin/aries profiles/kubernetes/openclaw-tb2-fix-git-deepseek-k8s-pods.json'"
fi
