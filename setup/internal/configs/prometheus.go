package configs

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// PrometheusDir is the configs subdirectory holding the Prometheus settings.
const PrometheusDir = "prometheus"

var (
	chartVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	helmVersion  = regexp.MustCompile(`^v3\.\d+\.\d+$`)
	sha256Hex    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	// labelValue is a Kubernetes label value, which a node role must be to be
	// usable in a nodeSelector.
	labelValue = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	// dnsLabel is a namespace or Helm release name.
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// Prometheus pins the monitoring stack setup_prometheus installs. The chart
// itself is vendored at k8s/prometheus/chart, so there is no repository to
// fetch from and no chart_repo here.
type Prometheus struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	// ChartVersion is the version the vendored chart is expected to be.
	// setup_prometheus compares it against the chart's own Chart.yaml and
	// refuses to install on a mismatch, because Helm ignores values keys a
	// chart does not define rather than rejecting them: a silently re-vendored
	// chart would quietly drop every override.
	ChartVersion string `json:"chart_version"`
	// NodeRole is the aries.dev/role whose nodes run Prometheus, Alertmanager,
	// Grafana, the operator and kube-state-metrics. node-exporter still runs
	// on every node.
	NodeRole string `json:"node_role"`
	// HelmVersion and HelmSHA256 pin the Helm binary. The expected digest lives
	// here rather than being downloaded beside the tarball: a checksum fetched
	// from the same place as the file only detects corruption, not tampering.
	HelmVersion string            `json:"helm_version"`
	HelmSHA256  map[string]string `json:"helm_sha256"`
}

// LoadPrometheus reads and validates dir/prometheus/prom_config.json.
func LoadPrometheus(dir string) (Prometheus, error) {
	var prometheus Prometheus
	if err := decodeStrict(filepath.Join(dir, PrometheusDir, "prom_config.json"), &prometheus); err != nil {
		return Prometheus{}, err
	}
	return prometheus, prometheus.Validate()
}

// Validate checks every field that reaches a command line or a node selector.
func (p Prometheus) Validate() error {
	var problems []string
	if !dnsLabel.MatchString(p.Namespace) {
		problems = append(problems, fmt.Sprintf("namespace %q must be a DNS label", p.Namespace))
	}
	if !dnsLabel.MatchString(p.Release) {
		problems = append(problems, fmt.Sprintf("release %q must be a DNS label", p.Release))
	}
	if !chartVersion.MatchString(p.ChartVersion) {
		problems = append(problems, fmt.Sprintf("chart_version %q must be an exact version like 72.6.2", p.ChartVersion))
	}
	if !labelValue.MatchString(p.NodeRole) {
		problems = append(problems, fmt.Sprintf("node_role %q must be a Kubernetes label value", p.NodeRole))
	}
	if !helmVersion.MatchString(p.HelmVersion) {
		problems = append(problems, fmt.Sprintf("helm_version %q must be a Helm 3 release like v3.22.0", p.HelmVersion))
	}
	if len(p.HelmSHA256) == 0 {
		problems = append(problems, "helm_sha256 needs a digest for each node architecture")
	}
	for arch, digest := range p.HelmSHA256 {
		if arch != "amd64" && arch != "arm64" {
			problems = append(problems, fmt.Sprintf("helm_sha256 architecture %q must be amd64 or arm64", arch))
		}
		if !sha256Hex.MatchString(digest) {
			problems = append(problems, fmt.Sprintf("helm_sha256[%s] must be 64 lowercase hex characters", arch))
		}
	}
	return joinProblems("prom_config.json", problems)
}
