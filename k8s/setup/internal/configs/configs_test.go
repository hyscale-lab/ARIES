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
	// Both values files live under k8s/, not beside this config.
	charts := Charts{Dir: filepath.Join("..", "..", "..")}
	if err := charts.RequirePrometheus(); err != nil {
		t.Errorf("values files: %v", err)
	}
}

func validPrometheus() Prometheus {
	return Prometheus{
		Namespace: "monitoring", Release: "prometheus",
		ChartRef:     "oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack",
		ChartVersion: "72.6.2", ChartDigest: "sha256:" + strings.Repeat("b", 64),
		HelmVersion: "v3.22.0",
		HelmSHA256:  map[string]string{"amd64": strings.Repeat("a", 64)},
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
		// The digest is what fixes the bytes; without it a re-pushed tag
		// would change what gets installed.
		"no digest":         func(p *Prometheus) { p.ChartDigest = "" },
		"short digest":      func(p *Prometheus) { p.ChartDigest = "sha256:abc" },
		"https repo":        func(p *Prometheus) { p.ChartRef = "https://prometheus-community.github.io/helm-charts" },
		"tag in ref":        func(p *Prometheus) { p.ChartRef += ":72.6.2" },
		"injection":         func(p *Prometheus) { p.ChartRef = "oci://ghcr.io/x; rm -rf /" },
		"helm 4":            func(p *Prometheus) { p.HelmVersion = "v4.3.0" },
		"release case":      func(p *Prometheus) { p.Release = "Prometheus" },
		"short helm digest": func(p *Prometheus) { p.HelmSHA256 = map[string]string{"amd64": "abc"} },
		"no digests":        func(p *Prometheus) { p.HelmSHA256 = nil },
		"odd arch":          func(p *Prometheus) { p.HelmSHA256 = map[string]string{"riscv64": strings.Repeat("a", 64)} },
		"namespace":         func(p *Prometheus) { p.Namespace = "Monitoring" },
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
		cluster := Cluster{Master: "user@master", Workers: []string{target}}
		if err := cluster.Validate(); err == nil {
			t.Errorf("target %q should be rejected", target)
		}
	}
}

func TestClusterAcceptsRealTargets(t *testing.T) {
	cluster := Cluster{
		Master:     "JXiang@clnode192.clemson.cloudlab.us",
		Workers:    []string{"node-1", "user@10.0.0.2"},
		Runner:     "user@10.0.0.9",
		SSHOptions: []string{"-o", "ConnectTimeout=15"},
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
	// The default needs no credentials: the bridge image is public. secret.yaml
	// is gitignored, so every other listed file must be committed, and
	// secret.yaml, when a private registry adds it, must come last so it wins.
	charts := Charts{Dir: filepath.Join("..", "..", "..")}
	var committed []string
	for i, name := range aries.ValuesFiles {
		if name == "secret.yaml" {
			if i != len(aries.ValuesFiles)-1 {
				t.Errorf("values_files = %q; secret.yaml must come last so it wins", aries.ValuesFiles)
			}
			continue
		}
		committed = append(committed, name)
	}
	if err := charts.RequireAries(committed); err != nil {
		t.Errorf("ARIES chart: %v", err)
	}
}

func TestAriesValidation(t *testing.T) {
	valid := func() Aries {
		return Aries{Namespace: "aries", Release: "aries",
			ValuesFiles: []string{"values-cluster.yaml", "secret.yaml"}, Timeout: "10m"}
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

// The runner must stay outside the cluster: listing it as the master or a
// worker would join the load generator to the cluster it measures.
func TestClusterRejectsARunnerThatIsANode(t *testing.T) {
	for name, cluster := range map[string]Cluster{
		"runner is master": {Master: "u@m", Runner: "u@m"},
		"runner is worker": {Master: "u@m", Workers: []string{"u@w"}, Runner: "u@w"},
		"runner injection": {Master: "u@m", Runner: "-oProxyCommand=id"},
	} {
		if err := cluster.Validate(); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	// runner_dir reaches a remote rm -rf of its subdirectories.
	for _, dir := range []string{"../etc", "/root", "a/b", "-rf", ".."} {
		if err := (Cluster{Master: "u@m", Runner: "u@r", RunnerDir: dir}).Validate(); err == nil {
			t.Errorf("runner_dir %q should be rejected", dir)
		}
	}
	if err := (Cluster{Master: "u@m", Runner: "u@r", RunnerDir: "aries-runs"}).Validate(); err != nil {
		t.Errorf("valid runner rejected: %v", err)
	}
}

// Role pools are gone. A cluster.json still listing them must fail loudly
// rather than silently join fewer nodes than it names.
func TestClusterRejectsRetiredRolePools(t *testing.T) {
	dir := t.TempDir()
	content := `{"master":"u@m","aries_nodes":["u@a"],"workers":[]}`
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCluster(dir); err == nil || !strings.Contains(err.Error(), "aries_nodes") {
		t.Errorf("a retired role pool should be rejected by name, got %v", err)
	}
}

// The housekeeping interval reaches the kubelet's command line, so only a
// plain whole-second duration is accepted, and it defaults to the 1s that
// matches ARIES's cAdvisor scrape.
func TestKubeletHousekeepingInterval(t *testing.T) {
	var kube Kube
	kube.applyDefaults()
	if kube.KubeletHousekeepingInterval != "1s" {
		t.Fatalf("default = %q, want 1s", kube.KubeletHousekeepingInterval)
	}
	for _, value := range []string{"1s", "10s", "120s"} {
		kube.KubeletHousekeepingInterval = value
		if err := kube.Validate(); err != nil {
			t.Errorf("%q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"0s", "500ms", "1m", "1s --anonymous-auth=true", "1s\nEVIL=1", "-1s"} {
		kube.KubeletHousekeepingInterval = value
		if err := kube.Validate(); err == nil {
			t.Errorf("%q should be rejected", value)
		}
	}
}
