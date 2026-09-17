package configs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Charts locates the Helm charts the installer deploys. Dir is k8s/ in the
// repository and charts/ inside a node's staged directory, so every path below
// is resolved relative to it rather than hard-coded.
type Charts struct {
	Dir string
}

// Layout under Charts.Dir. These mirror the repository's k8s/ tree.
const (
	ariesChartSubdir      = "aries"
	prometheusChartSubdir = "prometheus"
	grafanaSubdir         = "grafana"
	// vendoredChart is the upstream chart inside k8s/prometheus. It is kept in
	// its own subdirectory so our values file can sit beside it without
	// colliding with the chart's own values.yaml.
	vendoredChart = "chart"
)

// AriesChart is the ARIES chart directory.
func (c Charts) AriesChart() string { return filepath.Join(c.Dir, ariesChartSubdir) }

// AriesFile resolves a file inside the ARIES chart directory, such as a values
// file or secret.yaml.
func (c Charts) AriesFile(name string) string {
	return filepath.Join(c.Dir, ariesChartSubdir, name)
}

// PrometheusChart is the vendored kube-prometheus-stack chart directory.
func (c Charts) PrometheusChart() string {
	return filepath.Join(c.Dir, prometheusChartSubdir, vendoredChart)
}

// PrometheusValues is the Prometheus-side override file.
func (c Charts) PrometheusValues() string {
	return filepath.Join(c.Dir, prometheusChartSubdir, "values.yaml")
}

// GrafanaValues is the Grafana override file. Grafana is a subchart of
// kube-prometheus-stack, so this is applied to the same release.
func (c Charts) GrafanaValues() string {
	return filepath.Join(c.Dir, grafanaSubdir, "values.yaml")
}

// RequirePrometheus checks the vendored chart and both values files are present
// before Helm is installed or a namespace created, so a missing chart fails in
// a second rather than part-way through.
func (c Charts) RequirePrometheus() error {
	if err := requireDir(c.PrometheusChart(), "vendored kube-prometheus-stack chart"); err != nil {
		return err
	}
	for _, path := range []string{c.PrometheusValues(), c.GrafanaValues()} {
		if err := requireFile(path, "chart values"); err != nil {
			return err
		}
	}
	if err := requireFile(filepath.Join(c.PrometheusChart(), "Chart.yaml"), "vendored chart metadata"); err != nil {
		return err
	}
	return requireNoAppleDouble(c.PrometheusChart())
}

// requireNoAppleDouble rejects a chart directory containing macOS AppleDouble
// files, because Helm's own error for them is close to undiagnosable.
//
// Files on macOS carry extended attributes, and bsdtar serialises each as a
// companion member named "._<file>" holding binary metadata. If a chart is
// copied to a node with such an archive, helm reads every file in crds/ and
// tries to parse those as YAML — failing with "control characters are not
// allowed" and naming a file the operator never created. stage() prevents this
// at the source (see StageLine); this catches a chart that arrived some other
// way, and says what to delete.
func requireNoAppleDouble(chartDir string) error {
	var found []string
	err := filepath.WalkDir(chartDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "._") {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return nil
	}
	shown := found
	if len(shown) > 3 {
		shown = shown[:3]
	}
	return fmt.Errorf("%s holds %d macOS AppleDouble file(s) (%s); helm would try to parse them as chart YAML. Delete them with: find %s -name '._*' -delete",
		chartDir, len(found), strings.Join(shown, ", "), chartDir)
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
