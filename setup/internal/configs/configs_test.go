package configs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The checked-in configs are what a fresh clone runs with; they must load.
func TestCheckedInConfigsLoad(t *testing.T) {
	dir := filepath.Join("..", "..", "configs")
	kube, err := LoadKube(dir)
	if err != nil {
		t.Fatalf("kube.json: %v", err)
	}
	if kube.CNI != CNICalico {
		t.Errorf("checked-in CNI = %q; ARIES isolates sandboxes with NetworkPolicy, which needs calico", kube.CNI)
	}
	if _, err := LoadSystem(dir); err != nil {
		t.Fatalf("system.json: %v", err)
	}

	example := t.TempDir()
	content, err := os.ReadFile(filepath.Join(dir, "cluster.json.example"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(example, "cluster.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCluster(example); err != nil {
		t.Fatalf("cluster.json.example: %v", err)
	}
}

func TestCheckedInPrometheusConfigLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "configs")
	prom, err := LoadPrometheus(dir)
	if err != nil {
		t.Fatalf("prom_config.json: %v", err)
	}
	// The cluster nodes are x86_64; without an amd64 digest Helm cannot be
	// installed on them at all.
	if _, ok := prom.HelmSHA256["amd64"]; !ok {
		t.Error("helm_sha256 must include amd64")
	}
	// The chart and both values files live under k8s/, not beside this config.
	charts := Charts{Dir: filepath.Join("..", "..", "..", "k8s")}
	if err := charts.RequirePrometheus(); err != nil {
		t.Errorf("vendored chart and values: %v", err)
	}
}

func validPrometheus() Prometheus {
	return Prometheus{
		Namespace: "monitoring", Release: "prometheus",
		ChartVersion: "72.6.2",
		NodeRole:     "aries", HelmVersion: "v3.22.0",
		HelmSHA256: map[string]string{"amd64": strings.Repeat("a", 64)},
	}
}

func TestPrometheusValidation(t *testing.T) {
	if err := validPrometheus().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*Prometheus){
		// An unpinned chart would change what gets installed from day to day,
		// and the values keys are only checked against one version.
		"floating chart": func(p *Prometheus) { p.ChartVersion = "latest" },
		"helm 4":         func(p *Prometheus) { p.HelmVersion = "v4.3.0" },
		"release case":   func(p *Prometheus) { p.Release = "Prometheus" },
		"short digest":   func(p *Prometheus) { p.HelmSHA256 = map[string]string{"amd64": "abc"} },
		"no digests":     func(p *Prometheus) { p.HelmSHA256 = nil },
		"odd arch":       func(p *Prometheus) { p.HelmSHA256 = map[string]string{"riscv64": strings.Repeat("a", 64)} },
		"role injection": func(p *Prometheus) { p.NodeRole = "aries; kubectl delete ns aries" },
		"namespace":      func(p *Prometheus) { p.Namespace = "Monitoring" },
	}
	for name, mutate := range cases {
		p := validPrometheus()
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
}

func TestKubeDefaults(t *testing.T) {
	kube, err := LoadKube(writeConfig(t, "kube.json", `{"k8s_version": "v1.34.2"}`))
	if err != nil {
		t.Fatal(err)
	}
	if kube.CNI != CNICalico || kube.PodCIDR != "192.168.0.0/16" || kube.CalicoVersion != DefaultCalicoVersion {
		t.Errorf("defaults = %+v", kube)
	}
	if kube.K8sVersion != "1.34.2" {
		t.Errorf("K8sVersion = %q; a leading v must be stripped so package pins render correctly", kube.K8sVersion)
	}
}

func TestFlannelDefaultsToItsHardCodedCIDR(t *testing.T) {
	kube, err := LoadKube(writeConfig(t, "kube.json", `{"cni": "flannel"}`))
	if err != nil {
		t.Fatal(err)
	}
	if kube.PodCIDR != FlannelPodCIDR {
		t.Errorf("PodCIDR = %q, want %q", kube.PodCIDR, FlannelPodCIDR)
	}
}

// A misspelt key must fail rather than silently fall back to a default.
func TestUnknownFieldIsRejected(t *testing.T) {
	_, err := LoadKube(writeConfig(t, "kube.json", `{"cnii": "flannel"}`))
	if err == nil || !strings.Contains(err.Error(), "cnii") {
		t.Errorf("err = %v, want the unknown field named", err)
	}
}

func TestKubeValidationCatchesLateFailures(t *testing.T) {
	cases := map[string]string{
		"cni":      `{"cni": "weave"}`,
		"pod_cidr": `{"pod_cidr": "192.168.0.0"}`,
		"version":  `{"k8s_version": "1.34"}`,
		"channel":  `{"k8s_version": "1.34.2", "k8s_minor": "v1.33"}`,
		"address":  `{"advertise_address": "master-node"}`,
		"endpoint": `{"control_plane_endpoint": "10.0.0.1"}`,
		"calico":   `{"calico_version": "latest"}`,
	}
	for name, content := range cases {
		if _, err := LoadKube(writeConfig(t, "kube.json", content)); err == nil {
			t.Errorf("%s: %s should be rejected", name, content)
		}
	}
}

func TestMinorOf(t *testing.T) {
	for version, want := range map[string]string{"1.34.2": "v1.34", "v1.34.2": "v1.34", "v1.35.0\n": "v1.35", "garbage": ""} {
		if got := MinorOf(strings.TrimSpace(version)); got != want {
			t.Errorf("MinorOf(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestSystemFileIsOptional(t *testing.T) {
	system, err := LoadSystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if system.CRISocket == "" || system.JoinFile == "" || system.K8sPackageRepo == "" {
		t.Errorf("defaults not applied: %+v", system)
	}
}

// SSH targets are interpolated into shell lines and handed to ssh. A target
// starting with a dash is parsed by ssh as an option — `-oProxyCommand=...`
// runs an arbitrary local command — so it must never get that far.
func TestClusterRejectsTargetsThatAreNotHosts(t *testing.T) {
	for _, target := range []string{
		"-oProxyCommand=touch /tmp/pwned", "user@host; rm -rf /", "user@ host", "$(id)@host", "",
	} {
		cluster := Cluster{Master: "user@master", SandboxNodes: []string{target}}
		if err := cluster.Validate(); err == nil {
			t.Errorf("target %q should be rejected", target)
		}
	}
}

func TestClusterAcceptsRealTargets(t *testing.T) {
	cluster := Cluster{
		Master:       "JXiang@clnode192.clemson.cloudlab.us",
		AriesNodes:   []string{"node-1"},
		HarnessNodes: []string{"user@10.0.0.2"},
		SSHOptions:   []string{"-o", "ConnectTimeout=15"},
	}
	if err := cluster.Validate(); err != nil {
		t.Errorf("valid cluster rejected: %v", err)
	}
}

// Options are passed to ssh one argv entry each; "-o ConnectTimeout=15" as a
// single entry would reach ssh as one malformed argument.
func TestClusterRejectsCombinedSSHOptions(t *testing.T) {
	cluster := Cluster{Master: "user@master", SSHOptions: []string{"-o ConnectTimeout=15"}}
	if err := cluster.Validate(); err == nil {
		t.Error(`"-o ConnectTimeout=15" as one entry should be rejected`)
	}
}

// A value may legitimately contain spaces; only a flag carrying its value in
// the same entry is the mistake.
func TestClusterAcceptsSpacedOptionValues(t *testing.T) {
	cluster := Cluster{Master: "user@master", SSHOptions: []string{"-o", "ProxyCommand=ssh -W %h:%p jump"}}
	if err := cluster.Validate(); err != nil {
		t.Errorf("a ProxyCommand value was rejected: %v", err)
	}
}

func TestMissingClusterFilePointsAtTheExample(t *testing.T) {
	_, err := LoadCluster(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cluster.json.example") {
		t.Errorf("err = %v, want a pointer to cluster.json.example", err)
	}
}

func TestCheckedInAriesConfigLoads(t *testing.T) {
	dir := filepath.Join("..", "..", "configs")
	aries, err := LoadAries(dir)
	if err != nil {
		t.Fatalf("aries_config.json: %v", err)
	}
	// secret.yaml is gitignored, so only the committed values file is checked
	// here; create_cluster checks the whole list before touching a node.
	charts := Charts{Dir: filepath.Join("..", "..", "..", "k8s")}
	if err := charts.RequireAries(nil); err != nil {
		t.Errorf("ARIES chart: %v", err)
	}
	if len(aries.ValuesFiles) == 0 || aries.ValuesFiles[len(aries.ValuesFiles)-1] != "secret.yaml" {
		t.Errorf("values_files = %q; secret.yaml must come last so it wins", aries.ValuesFiles)
	}
}

func TestAriesValidation(t *testing.T) {
	valid := func() Aries {
		return Aries{Namespace: "aries", Release: "aries",
			ValuesFiles: []string{"values-incluster.yaml", "secret.yaml"}, Timeout: "10m"}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	cases := map[string]func(*Aries){
		// These names reach a helm command line, so a path or a shell
		// metacharacter must not survive validation.
		"path escape":    func(a *Aries) { a.ValuesFiles = []string{"../../etc/shadow.yaml"} },
		"absolute path":  func(a *Aries) { a.ValuesFiles = []string{"/etc/passwd.yaml"} },
		"not yaml":       func(a *Aries) { a.ValuesFiles = []string{"values.json"} },
		"injection":      func(a *Aries) { a.ValuesFiles = []string{"a.yaml; rm -rf /"} },
		"no values":      func(a *Aries) { a.ValuesFiles = nil },
		"namespace case": func(a *Aries) { a.Namespace = "Aries" },
		"bad timeout":    func(a *Aries) { a.Timeout = "10 minutes" },
		// The chart's cluster-scoped names derive from fullnameOverride, so an
		// unrelated release name is a likely mistake.
		"stray release": func(a *Aries) { a.Release = "monitoring" },
	}
	for name, mutate := range cases {
		a := valid()
		mutate(&a)
		if a.Validate() == nil {
			t.Errorf("%s: accepted %+v", name, a)
		}
	}
}

// The vendored chart must never carry AppleDouble files, and RequirePrometheus
// must say so plainly rather than letting helm fail on "control characters are
// not allowed" naming a file nobody created.
func TestRequirePrometheusRejectsAppleDoubleFiles(t *testing.T) {
	charts := Charts{Dir: filepath.Join("..", "..", "..", "k8s")}
	if err := charts.RequirePrometheus(); err != nil {
		t.Fatalf("the committed chart is not clean: %v", err)
	}

	// Rebuild the minimum chart shape in a temp dir and poison it.
	dir := t.TempDir()
	chart := filepath.Join(dir, "prometheus", "chart", "crds")
	if err := os.MkdirAll(chart, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		filepath.Join(dir, "prometheus", "chart", "Chart.yaml"),
		filepath.Join(dir, "prometheus", "values.yaml"),
	} {
		if err := os.WriteFile(f, []byte("version: 1.0.0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "grafana"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "grafana", "values.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	poisoned := Charts{Dir: dir}
	if err := poisoned.RequirePrometheus(); err != nil {
		t.Fatalf("clean temp chart rejected: %v", err)
	}
	// A resource fork, as bsdtar would leave it.
	if err := os.WriteFile(filepath.Join(chart, "._crd-alertmanagerconfigs.yaml"), []byte("\x00\x05\x16\x07"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := poisoned.RequirePrometheus()
	if err == nil {
		t.Fatal("an AppleDouble file in the chart was accepted")
	}
	if !strings.Contains(err.Error(), "AppleDouble") || !strings.Contains(err.Error(), "-delete") {
		t.Errorf("error should name the problem and the fix, got: %v", err)
	}
}
