#!/usr/bin/env bash
# Builds a Kubernetes cluster with vHive's setup tool, then applies ARIES to it.
#
# Every node is installed by vHive's setup_tool at a pinned commit, with
# Kubernetes and its pod network only (--k8s-only): no Knative, Istio or
# MetalLB. This script drives it over plain `ssh` from your own machine, so your
# SSH config or agent must already allow passwordless logins with sudo on every
# node. Every step checks what is already done, so a run that failed part-way
# can simply be run again.
set -euo pipefail

# vHive at this commit installs Kubernetes 1.35.3. When the commit changes,
# bump bridge.kubernetes_base_image in configs/versions.json (and
# configs/microbench-versions.json) to the Kubernetes version it installs.
VHIVE_REPO=${VHIVE_REPO:-https://github.com/vhive-serverless/vHive.git}
VHIVE_REVISION=${VHIVE_REVISION:-9826bb9c2af97d6478498df64879e8c30b21d31d}
# The Helm that installs the charts, checked against its published checksum.
HELM_VERSION=${HELM_VERSION:-v3.17.3}

config=""
prometheus=false
testbed=false
vhive_dir=""
skip_cluster=false
stage_runner=true
runner_dir=aries
ssh_key=""
ssh_options=()
housekeeping=""

usage() {
  cat <<'USAGE'
Usage: k8s/create-cluster.sh [options] [<master> <runner> [<worker> ...]]

Nodes are SSH targets such as user@host. The first is the control plane; the
second, the runner, joins the cluster, holds the monitoring stack and is where
the ARIES runner is staged; the rest are workers.

With no nodes on the command line, they and the settings below are read from a
JSON file: k8s/cluster.json, or the one --config names. Start from
k8s/cluster.json.example. Command-line options win over the file.

  --config FILE       read nodes and settings from FILE
  --prometheus        also install kube-prometheus-stack, on the runner
  --housekeeping DUR  set the kubelet's --housekeeping-interval on every node,
                      such as 1s; the kubelet's default is 10s
  --testbed           prepare the workers for measurements: pin the CPU clock
                      on the runner and workers, install Docker on the workers
  --vhive-dir DIR     install vHive from this local checkout's HEAD instead of
                      fetching VHIVE_REVISION from VHIVE_REPO
  --skip-cluster      the cluster already exists; only apply ARIES
  --no-runner         do not build and stage the runner on the runner node
  -h, --help          show this help

VHIVE_REPO and VHIVE_REVISION override the pinned vHive, HELM_VERSION the Helm.
USAGE
}

nodes=()
while [ $# -gt 0 ]; do
  case "$1" in
    --config) config=$2; shift 2 ;;
    --prometheus) prometheus=true; shift ;;
    --housekeeping) housekeeping=$2; shift 2 ;;
    --testbed) testbed=true; shift ;;
    --vhive-dir) vhive_dir=$2; shift 2 ;;
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

def flag(key):
    value = cfg.get(key, False)
    if not isinstance(value, bool):
        sys.exit(f"{key} must be true or false")
    return "true" if value else "false"

for retired in ("node_setup", "prune_invitro"):
    if retired in cfg:
        sys.exit(f"{retired} belonged to Invitro's setup, which ARIES no longer uses; remove it")
if not text("master") or not text("runner"):
    sys.exit('cluster config needs "master" and "runner"')
nodes = [text("master"), text("runner")] + strings("workers")
print("file_nodes=(" + " ".join(shlex.quote(node) for node in nodes) + ")")
print("file_ssh_options=(" + " ".join(shlex.quote(option) for option in strings("ssh_options")) + ")")
for key in ("runner_dir", "ssh_key", "kubelet_housekeeping_interval", "vhive_dir"):
    print(f"file_{key}={shlex.quote(text(key))}")
print("file_prometheus=" + flag("deploy_prometheus"))
print("file_testbed=" + flag("testbed"))
PY
  ) || exit 1
  eval "$parsed"
  [ ${#nodes[@]} -gt 0 ] || nodes=("${file_nodes[@]}")
  ssh_options=(${file_ssh_options[@]+"${file_ssh_options[@]}"})
  [ -z "$file_runner_dir" ] || runner_dir=$file_runner_dir
  ssh_key=$file_ssh_key
  [ "$prometheus" = true ] || prometheus=$file_prometheus
  [ "$testbed" = true ] || testbed=$file_testbed
  [ -n "$vhive_dir" ] || vhive_dir=$file_vhive_dir
  [ -n "$housekeeping" ] || housekeeping=$file_kubelet_housekeeping_interval
fi
case "$housekeeping" in
  ""|[1-9]s|[1-9][0-9]s|[1-9][0-9][0-9]ms) ;;
  *) echo "kubelet housekeeping interval must look like 1s or 500ms" >&2; exit 1 ;;
esac

[ ${#nodes[@]} -ge 2 ] || { usage >&2; exit 2; }
master=${nodes[0]}
runner=${nodes[1]}
members=("${nodes[@]:1}")
workers=("${nodes[@]:2}")
case "$runner_dir" in
  ""|/*|*..*|*[!A-Za-z0-9._/-]*) echo "runner_dir must be a simple path under the runner's home" >&2; exit 1 ;;
esac

# Every connection takes the key and options from the config through a shim
# placed first on PATH.
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
cleanup() { rm -rf "${shim:-}" "${stage:-}" "${applied:-}" "${bundle:-}" "${logs:-}"; }
trap cleanup EXIT

# ssh forwards its standard input; a call with nothing to send reads /dev/null,
# so it cannot consume this script's input.
remote() { ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new "$@"; }
for node in "${nodes[@]}"; do
  remote "$node" true </dev/null || { echo "cannot ssh to $node non-interactively" >&2; exit 1; }
done

# on_each runs a remote script on several nodes at once, prefixing each line
# with the node, and fails if any of them failed.
logs=$(mktemp -d)
on_each() {
  local script=$1; shift
  local pids=() node failed=0
  for node in "$@"; do
    remote "$node" "$script" </dev/null >"$logs/${node//[^A-Za-z0-9.@_-]/_}" 2>&1 &
    pids+=($!)
  done
  for index in "${!pids[@]}"; do
    node=${*:index+1:1}
    if ! wait "${pids[$index]}"; then failed=1; fi
    sed "s|^|    $node: |" "$logs/${node//[^A-Za-z0-9.@_-]/_}"
  done
  return $failed
}

# The node scripts ARIES runs on every node, from the committed tree.
ship_testbed() {
  for node in "$@"; do
    git -C "$root" archive HEAD scripts/testbed | remote "$node" "rm -rf ~/aries-testbed && mkdir -p ~/aries-testbed && tar -xf - -C ~/aries-testbed"
  done
}

if [ "$skip_cluster" = false ]; then
  ship_testbed "${nodes[@]}"

  if [ -n "$vhive_dir" ]; then
    vhive_dir=${vhive_dir/#\~/$HOME}
    git -C "$vhive_dir" diff --quiet HEAD -- || echo "    note: $vhive_dir has uncommitted changes; only its HEAD commit is installed" >&2
    VHIVE_REVISION=$(git -C "$vhive_dir" rev-parse HEAD)
    echo "==> Shipping vHive $VHIVE_REVISION from $vhive_dir"
    bundle=$(mktemp)
    git -C "$vhive_dir" bundle create -q "$bundle" HEAD
    for node in "${nodes[@]}"; do
      remote "$node" "cat > ~/vhive.bundle" <"$bundle"
    done
    source_command="git clone -q --no-checkout ~/vhive.bundle ~/vhive"
  else
    echo "==> Using vHive $VHIVE_REVISION from $VHIVE_REPO"
    source_command="git clone -q --no-checkout $(printf %q "$VHIVE_REPO") ~/vhive"
  fi

  echo "==> Installing every node with vHive's setup tool"
  on_each "
    set -e
    if [ \"\$(cat ~/.aries-node 2>/dev/null)\" = $VHIVE_REVISION ]; then
      echo 'already installed'
    else
      rm -rf ~/vhive
      $source_command
      git -C ~/vhive checkout -q --detach $VHIVE_REVISION ||
        { echo 'vHive $VHIVE_REVISION is not in the source; push it there or use --vhive-dir' >&2; exit 1; }
      cd ~/vhive/scripts
      ./install_go.sh > ~/aries-install-go.log 2>&1 || { tail -20 ~/aries-install-go.log >&2; exit 1; }
      source /etc/profile
      go build -o setup_tool
      ./setup_tool --k8s-only --containerd-service setup_node stock-only > ~/aries-setup-node.log 2>&1 ||
        { tail -20 ~/aries-setup-node.log >&2; exit 1; }
      echo $VHIVE_REVISION > ~/.aries-node
      echo 'installed'
    fi
    sudo ~/aries-testbed/scripts/testbed/quiet-node.sh
    sudo ~/aries-testbed/scripts/testbed/cpu-frequency.sh turbo-off >/dev/null
  " "${nodes[@]}"

  echo "==> Initialising the control plane on $master"
  remote "$master" "
    set -e
    if sudo test -f /etc/kubernetes/admin.conf; then
      echo '    already initialised'
    else
      cd ~/vhive/scripts && source /etc/profile
      ./setup_tool --k8s-only --init-only create_multinode_cluster stock-only > ~/aries-create-cluster.log 2>&1 ||
        { tail -20 ~/aries-create-cluster.log >&2; exit 1; }
    fi
  " </dev/null
  # A fresh token every time, so a rerun can still join a node.
  join=$(remote "$master" "sudo kubeadm token create --print-join-command" </dev/null)
  case "$join" in "kubeadm join "*) ;; *) echo "unexpected join command from $master: $join" >&2; exit 1 ;; esac

  echo "==> Joining the runner and workers"
  on_each "
    set -e
    if sudo test -f /etc/kubernetes/kubelet.conf; then
      echo 'already joined'
    else
      cd ~/vhive/scripts && source /etc/profile
      ./setup_tool setup_worker_kubelet stock-only > ~/aries-join.log 2>&1 && sudo $join >> ~/aries-join.log 2>&1 ||
        { tail -20 ~/aries-join.log >&2; exit 1; }
      echo 'joined'
    fi
  " "${members[@]}"

  echo "==> Installing the pod network and waiting for every node"
  remote "$master" "
    set -e
    cd ~/vhive/scripts && source /etc/profile
    ./setup_tool --k8s-only setup_master_node stock-only > ~/aries-master-node.log 2>&1 ||
      { tail -20 ~/aries-master-node.log >&2; exit 1; }
    kubectl get nodes -o wide | sed 's/^/    /'
  " </dev/null
fi

# The runner holds the monitoring stack, away from the workers, which the
# profiles' node_selector aries/role=worker places harness, sandbox and bridge
# pods on.
label() { remote "$master" "kubectl label node $(printf %q "$(remote "$1" hostname </dev/null)") aries/role=$2 --overwrite >/dev/null" </dev/null; }
label "$runner" monitoring
for node in ${workers[@]+"${workers[@]}"}; do
  label "$node" worker
done

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
    " </dev/null
  done
fi

echo "==> Applying ARIES on $master"
apply_args=(--runner-kubeconfig '$HOME/aries-kubeconfig')
[ "$prometheus" = false ] || apply_args+=(--monitoring)
# Ship the committed k8s/ tree only, so local credentials never leave this
# machine. Helm is installed at a pinned version, checked against the
# checksum its release publishes.
applied=$(mktemp)
git -C "$root" archive HEAD k8s | remote "$master" "
  set -e
  rm -rf ~/aries-k8s && mkdir -p ~/aries-k8s && tar -xf - -C ~/aries-k8s
  if ! helm version --short 2>/dev/null | grep -q '^$HELM_VERSION+'; then
    case \$(uname -m) in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo 'unsupported architecture' >&2; exit 1 ;; esac
    work=\$(mktemp -d)
    curl -fsSL -o \$work/helm.tgz https://get.helm.sh/helm-$HELM_VERSION-linux-\$arch.tar.gz
    curl -fsSL https://get.helm.sh/helm-$HELM_VERSION-linux-\$arch.tar.gz.sha256sum | awk '{print \$1\"  helm.tgz\"}' > \$work/sum
    (cd \$work && sha256sum -c --quiet sum)
    tar -xzf \$work/helm.tgz -C \$work
    sudo install -m 0755 \$work/linux-\$arch/helm /usr/local/bin/helm
    rm -rf \$work
  fi
  ~/aries-k8s/k8s/apply-aries.sh ${apply_args[*]}
" | tee "$applied"
prometheus_url=$(sed -n 's/^  prometheus_url  *: //p' "$applied")

if [ "$testbed" = true ] && [ "$skip_cluster" = false ]; then
  echo "==> Pinning the CPU clock on the runner and workers"
  on_each "sudo ~/aries-testbed/scripts/testbed/cpu-frequency.sh pin >/dev/null && ~/aries-testbed/scripts/testbed/cpu-frequency.sh status | grep -E '^(turbo|max kHz)'" "${members[@]}"
  if [ ${#workers[@]} -gt 0 ]; then
    echo "==> Installing Docker on the workers"
    on_each "sudo ~/aries-testbed/scripts/testbed/install-docker.sh 2>&1 | grep -E '^(Docker|replaced)'" "${workers[@]}"
    # cAdvisor finds Docker only when it starts.
    for node in "${workers[@]}"; do
      name=$(remote "$node" hostname </dev/null)
      remote "$master" "kubectl -n aries delete pod -l app.kubernetes.io/component=cadvisor --field-selector spec.nodeName=$(printf %q "$name") >/dev/null" </dev/null
    done
  fi
fi

if [ "$stage_runner" = true ]; then
  case "$(remote "$runner" uname -m </dev/null)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported runner architecture" >&2; exit 1 ;;
  esac
  echo "==> Building the runner for linux/$arch and staging it to $runner:~/$runner_dir"
  stage=$(mktemp -d)
  mkdir -p "$stage/bin"
  # The last commit that changed the bridge tags the Kubernetes bridge image
  # the runner requests, as in `make build`; k8s/publish-images.sh pushes it
  # under the same tag.
  revision=$("$root/scripts/bridge-revision.sh")
  for command in aries aries-ssh-client aries-replay; do
    (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
      -ldflags "-X main.bridgeRevision=$revision" \
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
