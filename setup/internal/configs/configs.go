// Package configs loads and validates aries-setup's JSON configuration.
//
// Configuration is split by where it is needed:
//
//	kube.json     cluster-wide Kubernetes settings   read on every node
//	system.json   upstream URLs and paths            read on every node
//	cluster.json  SSH topology and role pools        read on the operator's machine only
//
// cluster.json names hosts and users, so it is gitignored and create_cluster
// never copies it to a node.
package configs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Supported CNIs.
const (
	CNICalico  = "calico"
	CNIFlannel = "flannel"
	CNINone    = "none"
)

// DefaultCalicoVersion is pinned rather than tracking latest, so the CNI a
// cluster gets does not depend on the day it was built.
const DefaultCalicoVersion = "v3.32.2"

// FlannelPodCIDR is the network Flannel's upstream manifest hard-codes.
const FlannelPodCIDR = "10.244.0.0/16"

var (
	patchVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)
	minorChannel = regexp.MustCompile(`^v\d+\.\d+$`)
	calicoTag    = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	// sshTarget accepts [user@]host with no whitespace and no leading dash.
	// A leading dash would be parsed by ssh as an option, and these values are
	// interpolated into shell command lines.
	sshTarget = regexp.MustCompile(`^([A-Za-z0-9._-]+@)?[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// Kube holds the Kubernetes settings every node must agree on.
type Kube struct {
	// K8sVersion pins an exact patch, e.g. "1.34.2". Empty tracks the upstream
	// stable release at install time.
	K8sVersion string `json:"k8s_version"`
	// K8sMinor forces the package channel, e.g. "v1.34". Empty derives it from
	// K8sVersion, or from the upstream stable pointer.
	K8sMinor string `json:"k8s_minor"`
	// CNI is calico, flannel or none. ARIES isolates task sandboxes with
	// NetworkPolicy, which flannel accepts and does not enforce.
	CNI string `json:"cni"`
	// PodCIDR defaults per CNI.
	PodCIDR       string `json:"pod_cidr"`
	CalicoVersion string `json:"calico_version"`
	// AdvertiseAddress is the API server address workers dial. Empty detects
	// the source address of the master's default route.
	AdvertiseAddress     string `json:"advertise_address"`
	ControlPlaneEndpoint string `json:"control_plane_endpoint"`
	// SingleNode removes the control-plane taint so workloads schedule there.
	SingleNode   bool `json:"single_node"`
	OpenFirewall bool `json:"open_firewall"`
}

// System holds upstream locations and node-local paths.
type System struct {
	CRISocket          string `json:"cri_socket"`
	K8sStableURL       string `json:"k8s_stable_url"`
	K8sPackageRepo     string `json:"k8s_package_repo"`
	DockerRepo         string `json:"docker_repo"`
	CalicoManifestBase string `json:"calico_manifest_base"`
	FlannelManifest    string `json:"flannel_manifest"`
	JoinFile           string `json:"join_file"`
}

// Cluster is the operator-side topology create_cluster walks over SSH.
type Cluster struct {
	Master       string   `json:"master"`
	AriesNodes   []string `json:"aries_nodes"`
	HarnessNodes []string `json:"harness_nodes"`
	SandboxNodes []string `json:"sandbox_nodes"`
	// Workers are joined but neither labelled nor tainted.
	Workers []string `json:"workers"`
	// SkipRoleTaints joins every node as a plain worker. Named in the negative
	// so that leaving it out keeps the default of labelling and tainting.
	SkipRoleTaints  bool     `json:"skip_role_taints"`
	SSHKey          string   `json:"ssh_key"`
	SSHOptions      []string `json:"ssh_options"`
	FetchKubeconfig bool     `json:"fetch_kubeconfig"`
	// DeployPrometheus runs setup_prometheus on the master once the role pools
	// are labelled, installing the vendored kube-prometheus-stack chart (with
	// Grafana) per prometheus/prom_config.json.
	DeployPrometheus bool `json:"deploy_prometheus"`
	// DeployAries runs setup_aries on the master, installing the ARIES chart
	// per aries/aries_config.json. The chart needs an image the cluster can
	// pull and a filled-in k8s/aries/secret.yaml.
	DeployAries bool `json:"deploy_aries"`
}

// LoadKube reads, defaults and validates dir/kube.json.
func LoadKube(dir string) (Kube, error) {
	var kube Kube
	if err := decodeStrict(filepath.Join(dir, "kube.json"), &kube); err != nil {
		return Kube{}, err
	}
	kube.applyDefaults()
	return kube, kube.Validate()
}

// LoadSystem reads and defaults dir/system.json. The file is optional; every
// field has a default.
func LoadSystem(dir string) (System, error) {
	var system System
	path := filepath.Join(dir, "system.json")
	if _, err := os.Stat(path); err == nil {
		if err := decodeStrict(path, &system); err != nil {
			return System{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return System{}, err
	}
	system.applyDefaults()
	return system, nil
}

// LoadCluster reads and validates dir/cluster.json.
func LoadCluster(dir string) (Cluster, error) {
	var cluster Cluster
	path := filepath.Join(dir, "cluster.json")
	if err := decodeStrict(path, &cluster); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Cluster{}, fmt.Errorf("%s not found; start from cluster.json.example", path)
		}
		return Cluster{}, err
	}
	return cluster, cluster.Validate()
}

// decodeStrict rejects unknown fields, so a misspelt key fails loudly instead
// of silently taking its default.
func decodeStrict(path string, target any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func (k *Kube) applyDefaults() {
	if k.CNI == "" {
		k.CNI = CNICalico
	}
	if k.CalicoVersion == "" {
		k.CalicoVersion = DefaultCalicoVersion
	}
	if k.PodCIDR == "" {
		switch k.CNI {
		case CNICalico:
			k.PodCIDR = "192.168.0.0/16"
		default:
			k.PodCIDR = FlannelPodCIDR
		}
	}
	k.K8sVersion = strings.TrimPrefix(k.K8sVersion, "v")
}

// Validate checks Kube for values that would fail late, on a node, minutes in.
func (k Kube) Validate() error {
	var problems []string
	switch k.CNI {
	case CNICalico, CNIFlannel, CNINone:
	default:
		problems = append(problems, fmt.Sprintf("cni %q must be calico, flannel or none", k.CNI))
	}
	if _, _, err := net.ParseCIDR(k.PodCIDR); err != nil {
		problems = append(problems, fmt.Sprintf("pod_cidr %q is not a CIDR", k.PodCIDR))
	}
	if k.K8sVersion != "" && !patchVersion.MatchString(k.K8sVersion) {
		problems = append(problems, fmt.Sprintf("k8s_version %q must look like 1.34.2", k.K8sVersion))
	}
	if k.K8sMinor != "" && !minorChannel.MatchString(k.K8sMinor) {
		problems = append(problems, fmt.Sprintf("k8s_minor %q must look like v1.34", k.K8sMinor))
	}
	if k.K8sVersion != "" && k.K8sMinor != "" && MinorOf(k.K8sVersion) != k.K8sMinor {
		problems = append(problems, fmt.Sprintf("k8s_version %s is not in channel %s", k.K8sVersion, k.K8sMinor))
	}
	if !calicoTag.MatchString(k.CalicoVersion) {
		problems = append(problems, fmt.Sprintf("calico_version %q must look like v3.32.2", k.CalicoVersion))
	}
	if k.AdvertiseAddress != "" && net.ParseIP(k.AdvertiseAddress) == nil {
		problems = append(problems, fmt.Sprintf("advertise_address %q is not an IP address", k.AdvertiseAddress))
	}
	if k.ControlPlaneEndpoint != "" {
		if _, _, err := net.SplitHostPort(k.ControlPlaneEndpoint); err != nil {
			problems = append(problems, fmt.Sprintf("control_plane_endpoint %q must be host:port", k.ControlPlaneEndpoint))
		}
	}
	return joinProblems("kube.json", problems)
}

// MinorOf returns the package channel of a patch version: "1.34.2" -> "v1.34".
func MinorOf(version string) string {
	parts := strings.SplitN(strings.TrimPrefix(version, "v"), ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return "v" + parts[0] + "." + parts[1]
}

func (s *System) applyDefaults() {
	defaults := System{
		CRISocket:          "unix:///run/containerd/containerd.sock",
		K8sStableURL:       "https://dl.k8s.io/release/stable.txt",
		K8sPackageRepo:     "https://pkgs.k8s.io/core:/stable:",
		DockerRepo:         "https://download.docker.com/linux",
		CalicoManifestBase: "https://raw.githubusercontent.com/projectcalico/calico",
		FlannelManifest:    "https://github.com/flannel-io/flannel/releases/latest/download/kube-flannel.yml",
		JoinFile:           "/etc/aries/kubeadm-join.sh",
	}
	fill := func(field *string, fallback string) {
		if *field == "" {
			*field = fallback
		}
	}
	fill(&s.CRISocket, defaults.CRISocket)
	fill(&s.K8sStableURL, defaults.K8sStableURL)
	fill(&s.K8sPackageRepo, defaults.K8sPackageRepo)
	fill(&s.DockerRepo, defaults.DockerRepo)
	fill(&s.CalicoManifestBase, defaults.CalicoManifestBase)
	fill(&s.FlannelManifest, defaults.FlannelManifest)
	fill(&s.JoinFile, defaults.JoinFile)
}

// Validate checks every SSH target, since each one is interpolated into ssh
// command lines.
func (c Cluster) Validate() error {
	var problems []string
	if c.Master == "" {
		problems = append(problems, "master is required")
	}
	check := func(field, target string) {
		if !sshTarget.MatchString(target) {
			problems = append(problems, fmt.Sprintf("%s entry %q is not a [user@]host SSH target", field, target))
		}
	}
	if c.Master != "" {
		check("master", c.Master)
	}
	pools := []struct {
		field   string
		targets []string
	}{
		{"aries_nodes", c.AriesNodes}, {"harness_nodes", c.HarnessNodes},
		{"sandbox_nodes", c.SandboxNodes}, {"workers", c.Workers},
	}
	for _, pool := range pools {
		for _, target := range pool.targets {
			check(pool.field, target)
		}
	}
	for _, option := range c.SSHOptions {
		// Each entry is one argv element. A flag carrying its value in the same
		// entry ("-o ConnectTimeout=15", as a shell variable would hold it) reaches
		// ssh as one malformed argument. A value entry may contain spaces:
		// "ProxyCommand=ssh -W %h:%p jump" is legitimate.
		if option == "" || strings.TrimSpace(option) != option ||
			(strings.HasPrefix(option, "-") && strings.ContainsAny(option, " \t")) {
			problems = append(problems, fmt.Sprintf(`ssh_options entry %q must be one argument; write flags and values as separate entries, e.g. ["-o", "ConnectTimeout=15"]`, option))
		}
	}
	return joinProblems("cluster.json", problems)
}

func joinProblems(file string, problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s", file, strings.Join(problems, "; "))
}
