#!/usr/bin/env bash
# Applies ARIES to an existing Kubernetes cluster.
#
# k8s/create-cluster.sh builds a cluster and runs this on its master; any
# cluster works, run from a machine whose kubectl and helm reach it as a
# cluster admin, such as the master node.
#
# It installs the ARIES chart (service accounts, RBAC and the cAdvisor
# DaemonSet), and optionally the monitoring stack and a namespaced kubeconfig
# for the runner.
set -euo pipefail

namespace=aries
release=aries
values=()
monitoring=false
monitoring_release=prometheus
monitoring_namespace=monitoring
runner_kubeconfig=""
account=aries
# The kube-prometheus-stack chart --monitoring installs.
monitoring_chart_version=${MONITORING_CHART_VERSION:-72.6.2}

usage() {
  cat <<'USAGE'
Usage: k8s/apply-aries.sh [options]

  --namespace NAME           namespace for ARIES pods (default: aries)
  --values FILE              extra chart values file; repeatable, later files win
  --monitoring               install or upgrade kube-prometheus-stack with ARIES's
                             values, on the node labelled aries/role=monitoring
  --monitoring-release NAME  its release name (default: prometheus)
  --monitoring-namespace NS  its namespace (default: monitoring)
  --runner-kubeconfig PATH   write a kubeconfig limited to the ARIES namespace
  --runner-account NAME      the chart's runner ServiceAccount (default: aries)
  -h, --help                 show this help

MONITORING_CHART_VERSION overrides the kube-prometheus-stack chart version.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --namespace) namespace=$2; shift 2 ;;
    --values) values+=(-f "$2"); shift 2 ;;
    --monitoring) monitoring=true; shift ;;
    --monitoring-release) monitoring_release=$2; shift 2 ;;
    --monitoring-namespace) monitoring_namespace=$2; shift 2 ;;
    --runner-kubeconfig) runner_kubeconfig=$2; shift 2 ;;
    --runner-account) account=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "unknown option: $1" >&2; usage >&2; exit 2 ;;
  esac
done

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

for tool in kubectl helm; do
  command -v "$tool" >/dev/null || { echo "$tool is required on PATH" >&2; exit 1; }
done
kubectl auth can-i create namespaces >/dev/null 2>&1 ||
  { echo "kubectl must reach the cluster as an admin; the chart creates a namespace and Roles" >&2; exit 1; }

# The monitoring stack goes first: the chart creates its cAdvisor PodMonitor
# only when the PodMonitor resource type, which the stack installs, exists.
if [ "$monitoring" = true ]; then
  kubectl get nodes -l aries/role=monitoring -o name | grep -q . ||
    { echo "no node is labelled aries/role=monitoring to hold the monitoring stack" >&2; exit 1; }
  echo "==> Installing $monitoring_namespace/$monitoring_release (kube-prometheus-stack $monitoring_chart_version)"
  helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update >/dev/null
  helm upgrade --install "$monitoring_release" prometheus-community/kube-prometheus-stack \
    --namespace "$monitoring_namespace" --create-namespace --version "$monitoring_chart_version" \
    -f "$here/monitoring/values-aries.yaml" --wait --timeout 15m
fi

echo "==> Installing the ARIES chart as $namespace/$release"
helm upgrade --install "$release" "$here/aries" \
  --namespace "$namespace" --create-namespace \
  -f "$here/aries/values-cluster.yaml" ${values[@]+"${values[@]}"} --wait --timeout 5m


if [ -n "$runner_kubeconfig" ]; then
  # The chart's runner ServiceAccount holds only the namespaced Role, so the
  # runner need not hold cluster-admin.
  secret="${account}-token"
  echo "==> Writing a kubeconfig for ServiceAccount $namespace/$account to $runner_kubeconfig"
  token=""
  for _ in $(seq 1 30); do
    token=$(kubectl -n "$namespace" get secret "$secret" -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)
    [ -n "$token" ] && break
    sleep 1
  done
  [ -n "$token" ] || { echo "secret $namespace/$secret has no token" >&2; exit 1; }
  ca=$(kubectl -n "$namespace" get secret "$secret" -o jsonpath='{.data.ca\.crt}')
  server=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
  # Remove any earlier file first: writing in place would keep a looser mode.
  rm -f "$runner_kubeconfig"
  ( umask 077
    cat >"$runner_kubeconfig" <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
  - name: aries
    cluster:
      server: $server
      certificate-authority-data: $ca
users:
  - name: $account
    user:
      token: $token
contexts:
  - name: aries
    context:
      cluster: aries
      user: $account
      namespace: $namespace
current-context: aries
KUBECONFIG
  )
fi

echo
echo "ARIES is applied. Facts the profiles need:"
echo "  kubernetes version : $(kubectl version -o json 2>/dev/null | sed -n 's/.*"gitVersion": "\(v[^"]*\)".*/\1/p' | tail -1)"
dump=$(kubectl cluster-info dump 2>/dev/null || true)
echo "  pod_cidr           : $(printf '%s' "$dump" | grep -m1 -o -- '--cluster-cidr=[^" ,]*' | cut -d= -f2)"
echo "  service_cidr       : $(printf '%s' "$dump" | grep -m1 -o -- '--service-cluster-ip-range=[^" ,]*' | cut -d= -f2)"
pools=$(kubectl get ippools.crd.projectcalico.org -o jsonpath='{.items[*].spec.cidr}' 2>/dev/null || true)
[ -z "$pools" ] || echo "  calico pools       : $pools"
# The recorder reads pod measurements from Prometheus. A cluster node reaches
# the Service's cluster address directly, with no port-forward.
prometheus_ip=$(kubectl -n "$monitoring_namespace" get service "${monitoring_release}-kube-prometheus-prometheus" \
  -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
[ -z "$prometheus_ip" ] || echo "  prometheus_url     : http://${prometheus_ip}:9090"
echo "Set pod_cidr, service_cidr and prometheus_url under sandbox.deployment.kubernetes,"
echo "and build the bridge image on a base whose kubectl matches the version above."
