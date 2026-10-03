package configs

import (
	"fmt"
	"os"
	"path/filepath"
)

// Charts locates the Helm charts and values files the installer deploys. Dir
// is k8s/ in the repository and charts/ inside a node's staged directory, so
// every path below is resolved relative to it rather than hard-coded.
//
// Only the ARIES chart is a directory here. kube-prometheus-stack is pulled
// from its registry (see Prometheus.Chart), so only our values files for it
// are local.
type Charts struct {
	Dir string
}

// Layout under Charts.Dir. These mirror the repository's k8s/ tree.
const (
	ariesChartSubdir = "aries"
	prometheusSubdir = "prometheus"
	grafanaSubdir    = "grafana"
)

// AriesChart is the ARIES chart directory.
func (c Charts) AriesChart() string { return filepath.Join(c.Dir, ariesChartSubdir) }

// AriesFile resolves a file inside the ARIES chart directory, such as a values
// file or secret.yaml.
func (c Charts) AriesFile(name string) string {
	return filepath.Join(c.Dir, ariesChartSubdir, name)
}

// PrometheusValues is the Prometheus-side override file.
func (c Charts) PrometheusValues() string {
	return filepath.Join(c.Dir, prometheusSubdir, "values.yaml")
}

// GrafanaValues is the Grafana override file. Grafana is a subchart of
// kube-prometheus-stack, so this is applied to the same release.
func (c Charts) GrafanaValues() string {
	return filepath.Join(c.Dir, grafanaSubdir, "values.yaml")
}

// RequirePrometheus checks both values files are present before Helm is
// installed or a namespace created, so a missing file fails in a second rather
// than part-way through.
func (c Charts) RequirePrometheus() error {
	for _, path := range []string{c.PrometheusValues(), c.GrafanaValues()} {
		if err := requireFile(path, "chart values"); err != nil {
			return err
		}
	}
	return nil
}

// RequireAries checks the ARIES chart and the named values files are present.
func (c Charts) RequireAries(valuesFiles []string) error {
	if err := requireDir(c.AriesChart(), "ARIES chart"); err != nil {
		return err
	}
	if err := requireFile(filepath.Join(c.AriesChart(), "Chart.yaml"), "ARIES chart metadata"); err != nil {
		return err
	}
	for _, name := range valuesFiles {
		if err := requireFile(c.AriesFile(name), "ARIES chart values"); err != nil {
			return err
		}
	}
	return nil
}

func requireDir(path, what string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: %s is not a directory", what, path)
	}
	return nil
}

func requireFile(path, what string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s: %s is a directory", what, path)
	}
	return nil
}
