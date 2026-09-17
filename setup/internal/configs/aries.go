package configs

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// AriesDir is the configs subdirectory holding the ARIES deployment settings.
const AriesDir = "aries"

// valuesFileName keeps a values entry to a plain file name beside the chart.
// A path separator or .. would let a config file name something outside the
// chart directory, and these names reach a helm command line.
var valuesFileName = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)

// Aries pins how setup_aries installs the ARIES chart.
type Aries struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	// ValuesFiles are file names inside k8s/aries, applied in order, so later
	// files win. Put secret.yaml last.
	ValuesFiles []string `json:"values_files"`
	// Timeout is passed to helm --timeout. The ARIES pod only has to pull an
	// image and idle, so this is far shorter than the monitoring stack's.
	Timeout string `json:"timeout"`
}

// LoadAries reads and validates dir/aries/aries_config.json.
func LoadAries(dir string) (Aries, error) {
	var aries Aries
	if err := decodeStrict(filepath.Join(dir, AriesDir, "aries_config.json"), &aries); err != nil {
		return Aries{}, err
	}
	aries.applyDefaults()
	return aries, aries.Validate()
}

func (a *Aries) applyDefaults() {
	if a.Timeout == "" {
		a.Timeout = "10m"
	}
}

// Validate checks every field that reaches a helm command line.
func (a Aries) Validate() error {
	var problems []string
	if !dnsLabel.MatchString(a.Namespace) {
		problems = append(problems, fmt.Sprintf("namespace %q must be a DNS label", a.Namespace))
	}
	if !dnsLabel.MatchString(a.Release) {
		problems = append(problems, fmt.Sprintf("release %q must be a DNS label", a.Release))
	}
	if len(a.ValuesFiles) == 0 {
		problems = append(problems, "values_files needs at least one file, e.g. values-incluster.yaml")
	}
	for _, name := range a.ValuesFiles {
		if !valuesFileName.MatchString(name) {
			problems = append(problems, fmt.Sprintf("values_files entry %q must be a .yaml file name beside the chart, with no path", name))
		}
	}
	if !helmTimeout.MatchString(a.Timeout) {
		problems = append(problems, fmt.Sprintf("timeout %q must be a Go-style duration like 10m", a.Timeout))
	}
	// The chart pins resource names to "aries" via fullnameOverride, and its
	// ClusterRole and ClusterRoleBinding are cluster-scoped, so two releases
	// would fight over them. Flag the likely mistake rather than let the second
	// install silently adopt the first one's cluster resources.
	if a.Release != "aries" && !strings.Contains(a.Release, "aries") {
		problems = append(problems, fmt.Sprintf("release %q does not contain \"aries\"; the chart's cluster-scoped names derive from fullnameOverride, so set that too if this is intentional", a.Release))
	}
	return joinProblems("aries_config.json", problems)
}

var helmTimeout = regexp.MustCompile(`^\d+(ns|us|ms|s|m|h)$`)
