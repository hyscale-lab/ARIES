package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/setup/internal/configs"
)

// Every single-instance component the chart schedules. The paths are the
// placement keys of kube-prometheus-stack 72.6.2 and its grafana 9.0.0 and
// kube-state-metrics 5.33 subcharts. Helm silently ignores a key the chart does
// not define, so a misspelt path would not fail install — it would leave that
// component Pending on a tainted cluster. This list pins the spelling.
var placedComponents = []string{
	"alertmanager.alertmanagerSpec",
	"prometheus.prometheusSpec",
	"prometheusOperator",
	"prometheusOperator.admissionWebhooks.patch",
	"prometheusOperator.admissionWebhooks.deployment",
	"grafana",
	"kube-state-metrics",
}

func lookup(t *testing.T, values map[string]any, path string) map[string]any {
	t.Helper()
	current := values
	for _, key := range strings.Split(path, ".") {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("placement values have no %s (stopped at %q)", path, key)
		}
		current = next
	}
	return current
}

// Both halves are needed on every component: the nodeSelector puts it on the
// role's nodes, and the toleration gets it past the NoSchedule taint those
// nodes carry. Either one alone leaves the pod Pending.
func TestPlacementPinsEveryComponentToTheRole(t *testing.T) {
	rendered, err := PlacementValues("aries")
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(rendered), &values); err != nil {
		t.Fatalf("placement is not valid JSON (and so not valid Helm values): %v", err)
	}
	for _, path := range placedComponents {
		component := lookup(t, values, path)
		selector, _ := component["nodeSelector"].(map[string]any)
		if selector[RoleLabel] != "aries" {
			t.Errorf("%s nodeSelector = %v, want %s=aries", path, component["nodeSelector"], RoleLabel)
		}
		tolerations, _ := component["tolerations"].([]any)
		if len(tolerations) != 1 {
			t.Fatalf("%s tolerations = %v, want exactly the role toleration", path, component["tolerations"])
		}
		toleration := tolerations[0].(map[string]any)
		if toleration["key"] != RoleLabel || toleration["value"] != "aries" ||
			toleration["operator"] != "Equal" || toleration["effect"] != "NoSchedule" {
			t.Errorf("%s toleration = %v", path, toleration)
		}
	}
}

// node-exporter is a DaemonSet that must reach every node, including the
// sandbox node and the control plane. Pinning it to the monitoring role would
// silently drop host metrics for every other node.
func TestPlacementLeavesNodeExporterOnEveryNode(t *testing.T) {
	rendered, err := PlacementValues("aries")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "node-exporter") {
		t.Error("placement must not constrain prometheus-node-exporter")
	}
}

func TestCheckPrometheusPlacement(t *testing.T) {
	prom := configs.Prometheus{NodeRole: "aries"}
	cluster := configs.Cluster{Master: "u@m", AriesNodes: []string{"u@a"}}
	if err := CheckPrometheusPlacement(cluster, prom); err != nil {
		t.Errorf("valid placement rejected: %v", err)
	}

	skipped := cluster
	skipped.SkipRoleTaints = true
	if CheckPrometheusPlacement(skipped, prom) == nil {
		t.Error("skip_role_taints leaves no role label to place monitoring on")
	}

	if CheckPrometheusPlacement(configs.Cluster{Master: "u@m", SandboxNodes: []string{"u@s"}}, prom) == nil {
		t.Error("no node with the aries role should be rejected")
	}

	// The master is labelled aries.dev/role=master but tainted by kubeadm,
	// not by the role taint, so the generated toleration cannot place pods.
	if CheckPrometheusPlacement(cluster, configs.Prometheus{NodeRole: "master"}) == nil {
		t.Error("node_role master should be rejected")
	}
}

// A node listed in several pools takes the last role; the check must follow
// that rather than the first pool it appears in.
func TestCheckPrometheusPlacementFollowsFinalRole(t *testing.T) {
	cluster := configs.Cluster{Master: "u@m", AriesNodes: []string{"u@x"}, SandboxNodes: []string{"u@x"}}
	if CheckPrometheusPlacement(cluster, configs.Prometheus{NodeRole: "aries"}) == nil {
		t.Error("u@x ends up as sandbox, so no node holds the aries role")
	}
}

// The printed port-forward commands must name Services the chart really
// creates. These follow the charts' fullname helpers, including the 26
// character truncation that turns release "prometheus" into
// prometheus-kube-prometheus.
func TestServiceNamesFollowTheCharts(t *testing.T) {
	cases := []struct{ release, prometheus, grafana string }{
		{"prometheus", "prometheus-kube-prometheus-prometheus", "prometheus-grafana"},
		{"mon", "mon-kube-prometheus-stack-prometheus", "mon-grafana"},
		{"kube-prometheus-stack", "kube-prometheus-stack-prometheus", "kube-prometheus-stack-grafana"},
		// 10 + 1 + 15 = 26: truncation lands exactly after "kube-prometheus".
		{"my-grafana", "my-grafana-kube-prometheus-prometheus", "my-grafana"},
		// Truncation mid-word.
		{"prometheus-mon", "prometheus-mon-kube-promet-prometheus", "prometheus-mon-grafana"},
		// 9 + 1 + 16 = 26 ends on the dash before "stack", which is trimmed.
		{"prom-test", "prom-test-kube-prometheus-prometheus", "prom-test-grafana"},
	}
	for _, tc := range cases {
		if got := PrometheusServiceName(tc.release); got != tc.prometheus {
			t.Errorf("PrometheusServiceName(%q) = %q, want %q", tc.release, got, tc.prometheus)
		}
		if got := GrafanaName(tc.release); got != tc.grafana {
			t.Errorf("GrafanaName(%q) = %q, want %q", tc.release, got, tc.grafana)
		}
	}
}

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"; got != want {
		t.Errorf("sha256 = %s, want %s", got, want)
	}
}

// What reaches a node is the binary, kube.json, system.json and — only for
// what is actually being deployed — the matching config and chart. cluster.json
// names every host and user and must stay on the operator's machine.
func TestStageShipsOnlyWhatIsDeployedAndNeverTopology(t *testing.T) {
	configsDir := filepath.Join("..", "..", "configs")
	chartsDir := filepath.Join("..", "..", "..", "k8s")
	binary := filepath.Join(t.TempDir(), "aries-setup")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	shipped := func(cluster configs.Cluster) []string {
		t.Helper()
		cluster.Master = "u@m"
		stage, err := buildStage(CreateOptions{
			ConfigsDir: configsDir, ChartsDir: chartsDir,
			AriesValuesFiles: []string{"values-incluster.yaml"},
			NodeBinary:       binary, NodeArch: "amd64", Cluster: cluster,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(stage)
		var files []string
		err = filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
			if err == nil && !entry.IsDir() {
				rel, _ := filepath.Rel(stage, path)
				files = append(files, filepath.ToSlash(rel))
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return files
	}

	// The vendored chart is hundreds of files, so assert on the config tree
	// exactly and on the charts by presence.
	configsOnly := func(files []string) string {
		var kept []string
		for _, name := range files {
			if name == "aries-setup" || strings.HasPrefix(name, "configs/") {
				kept = append(kept, name)
			}
		}
		return strings.Join(kept, " ")
	}
	has := func(files []string, want string) bool {
		return slices.Contains(files, want)
	}

	bare := shipped(configs.Cluster{})
	if got := configsOnly(bare); got != "aries-setup configs/kube.json configs/system.json" {
		t.Errorf("with nothing deployed, staged %q", got)
	}
	if len(bare) != 3 {
		t.Errorf("with nothing deployed, staged %d files; no chart should be shipped: %q", len(bare), bare)
	}

	prom := shipped(configs.Cluster{DeployPrometheus: true})
	if got := configsOnly(prom); got != "aries-setup configs/kube.json configs/prometheus/prom_config.json configs/system.json" {
		t.Errorf("with Prometheus, staged configs %q", got)
	}
	for _, want := range []string{
		"charts/prometheus/values.yaml",
		"charts/prometheus/chart/Chart.yaml",
		"charts/grafana/values.yaml", // Grafana always ships with Prometheus
	} {
		if !has(prom, want) {
			t.Errorf("with Prometheus, %s was not staged", want)
		}
	}
	if has(prom, "charts/aries/Chart.yaml") {
		t.Error("the ARIES chart was staged without deploy_aries")
	}

	aries := shipped(configs.Cluster{DeployAries: true})
	if got := configsOnly(aries); got != "aries-setup configs/aries/aries_config.json configs/kube.json configs/system.json" {
		t.Errorf("with ARIES, staged configs %q", got)
	}
	if !has(aries, "charts/aries/Chart.yaml") {
		t.Error("with deploy_aries, the ARIES chart was not staged")
	}
	if has(aries, "charts/prometheus/chart/Chart.yaml") {
		t.Error("the 6.7MB monitoring chart was staged without deploy_prometheus")
	}

	for _, files := range [][]string{bare, prom, aries} {
		for _, name := range files {
			if strings.Contains(name, "cluster.json") {
				t.Errorf("%s must never be staged for a node", name)
			}
		}
	}
}

// The vendored chart and the pinned version must agree, or every override in
// values.yaml is silently dropped: Helm ignores keys a chart does not define.
func TestVendoredChartMatchesThePinnedVersion(t *testing.T) {
	charts := configs.Charts{Dir: filepath.Join("..", "..", "..", "k8s")}
	prom, err := configs.LoadPrometheus(filepath.Join("..", "..", "configs"))
	if err != nil {
		t.Fatal(err)
	}
	vendored, err := chartVersionOf(charts.PrometheusChart())
	if err != nil {
		t.Fatal(err)
	}
	if vendored != prom.ChartVersion {
		t.Errorf("vendored chart is %s but prom_config.json pins %s; re-check the values files, then update chart_version and k8s/prometheus/VENDORED.md",
			vendored, prom.ChartVersion)
	}
}

// appVersion sits next to version in Chart.yaml and is a different thing.
func TestChartVersionIgnoresAppVersion(t *testing.T) {
	dir := t.TempDir()
	content := "apiVersion: v2\nname: x\nappVersion: v0.82.2\nversion: 72.6.2\n"
	if err := os.WriteFile(filepath.Join(dir, "Chart.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := chartVersionOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "72.6.2" {
		t.Errorf("chartVersionOf = %q, want 72.6.2", got)
	}
}

// Grafana is published on a NodePort, so the installer should print a URL the
// operator can open, not a port-forward they do not need.
func TestAccessInstructionsPrintTheNodePortURL(t *testing.T) {
	prom := configs.Prometheus{Namespace: "monitoring", Release: "prometheus"}

	withPort := strings.Join(AccessInstructions(prom, GrafanaAccess{NodePort: 30300, NodeIP: "128.110.219.68"}), "\n")
	if !strings.Contains(withPort, "http://128.110.219.68:30300") {
		t.Errorf("no reachable grafana URL:\n%s", withPort)
	}
	if strings.Contains(withPort, "port-forward svc/prometheus-grafana") {
		t.Error("a port-forward for grafana is redundant once it has a NodePort")
	}
	// The exposure is worth stating, since node addresses are often public.
	if !strings.Contains(withPort, "public") {
		t.Errorf("the NodePort exposure should be called out:\n%s", withPort)
	}

	// Without a discoverable node address the port is still useful.
	noIP := strings.Join(AccessInstructions(prom, GrafanaAccess{NodePort: 30300}), "\n")
	if !strings.Contains(noIP, "<any-node-ip>:30300") {
		t.Errorf("should still name the port:\n%s", noIP)
	}

	// Falling back to a port-forward when the Service is not a NodePort.
	clusterIP := strings.Join(AccessInstructions(prom, GrafanaAccess{}), "\n")
	if !strings.Contains(clusterIP, "port-forward svc/prometheus-grafana 3000:80") {
		t.Errorf("no port-forward fallback:\n%s", clusterIP)
	}

	// Prometheus is never published, in either case.
	for _, out := range []string{withPort, clusterIP} {
		if !strings.Contains(out, "port-forward svc/prometheus-kube-prometheus-prometheus 9090") {
			t.Errorf("prometheus should stay cluster-internal with a forward:\n%s", out)
		}
	}
}

// The values file and what the installer tells the operator must not drift:
// both name port 30300.
func TestGrafanaValuesAndInstructionsAgreeOnThePort(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "k8s", "grafana", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "nodePort: 30300") {
		t.Error("k8s/grafana/values.yaml no longer pins nodePort 30300; update this test and the docs together")
	}
	if !strings.Contains(string(content), "type: NodePort") {
		t.Error("k8s/grafana/values.yaml no longer publishes grafana as a NodePort")
	}
}
