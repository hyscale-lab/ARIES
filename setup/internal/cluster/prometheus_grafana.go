package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/hyscale-lab/aries/setup/internal/configs"
	"github.com/hyscale-lab/aries/setup/internal/utils"
)

const helmBinary = "/usr/local/bin/helm"

// SetupPrometheus installs Prometheus and Grafana on the control plane.
//
// Grafana is not a separate release: it is a subchart of kube-prometheus-stack,
// which is why one `helm upgrade` brings up both and why its overrides are a
// second values file rather than a second install. Keeping it bundled is what
// gives the Prometheus datasource and the default Kubernetes dashboards
// without configuring either.
//
// This follows the loader's setup_prometheus flow — Helm, a monitoring
// namespace, then the chart — with three differences:
//
//   - Helm is a pinned release verified against a configured digest, not the
//     installer script from Helm's master branch piped into bash.
//   - The chart is vendored in the repository and installed from disk, so the
//     install needs no chart-repository access and cannot drift between runs.
//   - Placement comes from node_role and ARIES's role taints, not the
//     loader-nodetype label, which ARIES nodes do not carry.
//
// It deliberately leaves out the loader's perf_event_paranoid change, the
// metrics-server and pushgateway installs, the Knative monitors, and the
// kubeadm re-render of control-plane bind addresses.
func SetupPrometheus(prom configs.Prometheus, charts configs.Charts) error {
	if err := utils.RequireRoot(); err != nil {
		return err
	}
	if _, err := os.Stat(adminConf); err != nil {
		return fmt.Errorf("%s not found; setup_prometheus runs on an initialised control plane", adminConf)
	}
	if err := charts.RequirePrometheus(); err != nil {
		return err
	}
	// Helm ignores values keys a chart does not define, so a chart vendored at
	// a different version than prom_config.json pins would silently drop every
	// override in values.yaml instead of failing.
	vendored, err := chartVersionOf(charts.PrometheusChart())
	if err != nil {
		return err
	}
	if vendored != prom.ChartVersion {
		return fmt.Errorf("vendored chart at %s is version %s but prom_config.json pins %s; re-check the values files against the chart, then update chart_version",
			charts.PrometheusChart(), vendored, prom.ChartVersion)
	}

	// The master carries aries.dev/role=master but its taint is kubeadm's
	// control-plane taint, which the role toleration does not cover; allowing
	// it here would pass the label check and then hang helm --wait for 15m.
	if !slices.Contains(Roles, prom.NodeRole) {
		return fmt.Errorf("node_role %q must be one of %v", prom.NodeRole, Roles)
	}

	// Fail now rather than leave every monitoring pod Pending: nothing else
	// can land on a node without the role label, because every ARIES role
	// node is tainted.
	utils.WaitPrintf("Checking for nodes labelled %s=%s", RoleLabel, prom.NodeRole)
	nodes, err := utils.ExecShellCmd(kubectlAdmin+" get nodes -l %s -o name", utils.Quote(RoleLabel+"="+prom.NodeRole))
	if err != nil {
		return err
	}
	if strings.TrimSpace(nodes) == "" {
		return fmt.Errorf("no node is labelled %s=%s; label the role pools first (create_cluster does) or change node_role", RoleLabel, prom.NodeRole)
	}
	for _, node := range strings.Fields(nodes) {
		utils.InfoPrintf("monitoring runs on %s", strings.TrimPrefix(node, "node/"))
	}

	if err := installHelm(prom); err != nil {
		return err
	}

	helm := "KUBECONFIG=" + adminConf + " " + helmBinary

	placement, err := os.CreateTemp("", "aries-prometheus-placement-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(placement.Name())
	_ = placement.Close()
	rendered, err := PlacementValues(prom.NodeRole)
	if err != nil {
		return err
	}
	if err := utils.WriteFile(placement.Name(), rendered, 0o600); err != nil {
		return err
	}

	utils.WaitPrintf("Installing kube-prometheus-stack %s with Grafana as %s/%s (several minutes)",
		prom.ChartVersion, prom.Namespace, prom.Release)
	// Values files are applied in order and later ones win: our Prometheus
	// overrides, then Grafana's, then placement, which must beat whatever the
	// first two set for nodeSelector and tolerations.
	//
	// There is no --version: the chart is a directory, and its version was
	// checked against prom_config.json above. --create-namespace makes the
	// install idempotent without a separate kubectl step.
	if err := utils.ExecShellCmdStreaming(helm+" upgrade --install %s %s --namespace %s --create-namespace -f %s -f %s -f %s --wait --timeout 15m",
		utils.Quote(prom.Release), utils.Quote(charts.PrometheusChart()), utils.Quote(prom.Namespace),
		utils.Quote(charts.PrometheusValues()), utils.Quote(charts.GrafanaValues()),
		utils.Quote(placement.Name())); err != nil {
		return err
	}

	pods, _ := utils.ExecShellCmd(kubectlAdmin+" get pods -n %s -o wide", utils.Quote(prom.Namespace))
	utils.InfoPrintf("\n%s", pods)
	utils.SuccessPrintf("Prometheus and Grafana are up")
	for _, line := range AccessInstructions(prom, discoverGrafanaAccess(prom)) {
		utils.InfoPrintf("%s", line)
	}
	return nil
}

// installHelm installs the pinned Helm release, verifying the tarball against
// the digest in prom_config.json before anything from it is executed.
func installHelm(prom configs.Prometheus) error {
	if version, err := utils.ExecShellCmd(helmBinary + " version --short"); err == nil && strings.HasPrefix(version, prom.HelmVersion+"+") {
		utils.InfoPrintf("helm %s already installed", prom.HelmVersion)
		return nil
	}
	arch := runtime.GOARCH
	want, ok := prom.HelmSHA256[arch]
	if !ok {
		return fmt.Errorf("helm_sha256 has no digest for %s", arch)
	}

	utils.WaitPrintf("Installing helm %s (linux/%s)", prom.HelmVersion, arch)
	workdir, err := os.MkdirTemp("", "aries-helm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workdir)

	name := fmt.Sprintf("helm-%s-linux-%s.tar.gz", prom.HelmVersion, arch)
	tarball := filepath.Join(workdir, name)
	if _, err := utils.ExecShellCmd("curl -fsSL --retry 3 -o %s %s", utils.Quote(tarball), utils.Quote("https://get.helm.sh/"+name)); err != nil {
		return err
	}
	got, err := fileSHA256(tarball)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s has sha256 %s, want %s; refusing to install it", name, got, want)
	}
	_, err = utils.ExecShellCmd("tar -xzf %s -C %s && install -m 0755 %s %s",
		utils.Quote(tarball), utils.Quote(workdir), utils.Quote(filepath.Join(workdir, "linux-"+arch, "helm")), helmBinary)
	return err
}

// chartVersionOf reads the version from a chart directory's Chart.yaml. It
// scans for the top-level `version:` key rather than parsing YAML, because
// Chart.yaml is a flat, chart-defined schema and this avoids a YAML dependency
// in the installer. appVersion is a different key and is not matched.
func chartVersionOf(chartDir string) (string, error) {
	path := filepath.Join(chartDir, "Chart.yaml")
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read vendored chart version: %w", err)
	}
	for _, line := range strings.Split(string(content), "\n") {
		rest, ok := strings.CutPrefix(line, "version:")
		if !ok {
			continue
		}
		version := strings.TrimSpace(rest)
		version = strings.Trim(version, `"'`)
		if version == "" {
			continue
		}
		return version, nil
	}
	return "", fmt.Errorf("%s has no top-level version key", path)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// PlacementValues renders the chart values that pin every single-instance
// monitoring component to nodes of the given role. JSON is valid Helm values,
// and marshalling it means the role never needs escaping.
//
// Each component needs both halves. The nodeSelector puts it on the role's
// nodes; the toleration gets it past the NoSchedule taint on those nodes,
// without which it would stay Pending. The admission-webhook patch Jobs run
// as install hooks, so leaving them out would hang `helm --wait`.
//
// node-exporter is absent on purpose: it is a DaemonSet that must run on every
// node, and its chart default already tolerates any NoSchedule taint.
func PlacementValues(role string) (string, error) {
	placement := map[string]any{
		"nodeSelector": map[string]string{RoleLabel: role},
		"tolerations": []map[string]string{{
			"key": RoleLabel, "operator": "Equal", "value": role, "effect": "NoSchedule",
		}},
	}
	values := map[string]any{
		"alertmanager": map[string]any{"alertmanagerSpec": placement},
		"prometheus":   map[string]any{"prometheusSpec": placement},
		"prometheusOperator": merge(placement, map[string]any{
			"admissionWebhooks": map[string]any{
				"patch":      placement,
				"deployment": placement,
			},
		}),
		"grafana":            placement,
		"kube-state-metrics": placement,
	}
	content, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return "", err
	}
	return string(content) + "\n", nil
}

func merge(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// GrafanaAccess is how Grafana turned out to be reachable. NodePort is 0 when
// the Service is not a NodePort, in which case a port-forward is the only way
// in.
type GrafanaAccess struct {
	NodePort int
	NodeIP   string
}

// AccessInstructions tells the operator how to reach Prometheus and Grafana.
//
// Prometheus stays ClusterIP: it is scraped from inside the cluster and read
// through Grafana, so publishing it would add exposure for nothing. Grafana is
// a NodePort by default (see k8s/grafana/values.yaml), so when one is found
// this prints the URL instead of a port-forward. The loader keeps port-forwards
// running in tmux on the master; these run on demand from any machine with a
// kubeconfig.
func AccessInstructions(prom configs.Prometheus, grafanaAt GrafanaAccess) []string {
	ns := utils.Quote(prom.Namespace)
	grafana := GrafanaName(prom.Release)

	lines := []string{}
	if grafanaAt.NodePort > 0 {
		host := grafanaAt.NodeIP
		if host == "" {
			host = "<any-node-ip>"
		}
		lines = append(lines,
			fmt.Sprintf("grafana is on every node's IP: http://%s:%d", host, grafanaAt.NodePort),
			"  that port is open on all node interfaces; on a cluster with public node addresses it is public",
		)
	} else {
		lines = append(lines,
			"grafana has no NodePort; reach it with a port-forward:",
			fmt.Sprintf("  kubectl -n %s port-forward svc/%s 3000:80", ns, grafana),
		)
	}
	return append(lines,
		"grafana user is admin; the password is generated, not the chart's public default:",
		fmt.Sprintf("  kubectl -n %s get secret %s -o jsonpath='{.data.admin-password}' | base64 -d", ns, grafana),
		"prometheus stays cluster-internal; read it through grafana, or forward it:",
		fmt.Sprintf("  kubectl -n %s port-forward svc/%s 9090", ns, PrometheusServiceName(prom.Release)),
	)
}

// discoverGrafanaAccess reads back how the Service was actually created, rather
// than trusting the values file: a nodePort can be reassigned on conflict, and
// the Service type could have been overridden.
//
// Both lookups are best-effort. Failing to print a URL must not fail an install
// that already succeeded.
func discoverGrafanaAccess(prom configs.Prometheus) GrafanaAccess {
	var access GrafanaAccess
	port, err := utils.ExecShellCmd(kubectlAdmin+" -n %s get svc %s -o jsonpath={.spec.ports[0].nodePort}",
		utils.Quote(prom.Namespace), utils.Quote(GrafanaName(prom.Release)))
	if err != nil {
		return access
	}
	access.NodePort, err = strconv.Atoi(strings.TrimSpace(port))
	if err != nil || access.NodePort <= 0 {
		return GrafanaAccess{}
	}
	// Prefer an address reachable from the operator's machine. ExternalIP is
	// empty on a bare kubeadm cluster, where the node's routable address is
	// reported as InternalIP.
	for _, query := range []string{
		"{.items[0].status.addresses[?(@.type=='ExternalIP')].address}",
		"{.items[0].status.addresses[?(@.type=='InternalIP')].address}",
	} {
		address, err := utils.ExecShellCmd(kubectlAdmin+" get nodes -o jsonpath=%s", utils.Quote(query))
		if err == nil && strings.TrimSpace(address) != "" {
			access.NodeIP = strings.Fields(strings.TrimSpace(address))[0]
			break
		}
	}
	return access
}

// chartFullname mirrors the Helm fullname helper both charts use: the release
// name alone if it already contains the chart name, otherwise
// "<release>-<chart>", truncated to limit with a trailing dash removed.
func chartFullname(release, chart string, limit int) string {
	name := release
	if !strings.Contains(release, chart) {
		name = release + "-" + chart
	}
	if len(name) > limit {
		name = name[:limit]
	}
	return strings.TrimSuffix(name, "-")
}

// PrometheusServiceName is the Prometheus Service kube-prometheus-stack 72.6.2
// creates. Its fullname truncates at 26 characters, which is why the default
// release "prometheus" yields prometheus-kube-prometheus-prometheus.
func PrometheusServiceName(release string) string {
	return chartFullname(release, "kube-prometheus-stack", 26) + "-prometheus"
}

// GrafanaName is the Grafana Service and admin Secret name.
func GrafanaName(release string) string {
	return chartFullname(release, "grafana", 63)
}
