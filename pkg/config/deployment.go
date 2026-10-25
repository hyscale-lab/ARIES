package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
)

// DeploymentConfig selects placement independently of the component identity.
type DeploymentConfig struct {
	Backend    string                      `json:"backend"`
	Docker     *DockerDeploymentConfig     `json:"docker,omitempty"`
	Kubernetes *KubernetesDeploymentConfig `json:"kubernetes,omitempty"`
}

type DockerDeploymentConfig struct {
	Socket string `json:"socket,omitempty"`
	// PrometheusURL, set on sandbox.deployment only, reads every runtime's
	// measurements from Prometheus, which scrapes the ARIES chart's standalone
	// cAdvisor on the Docker host, instead of from the Docker stats API. That is
	// how the Kubernetes backend is measured, so the two can be compared.
	PrometheusURL string `json:"prometheus_url,omitempty"`
}

// KubernetesDeploymentConfig places a component's runtimes as pods. ARIES
// reaches the cluster through kubectl and whatever context that resolves.
type KubernetesDeploymentConfig struct {
	// Namespace holds every pod and policy ARIES creates. It defaults to "aries"
	// and must be the same for harness, sandbox and bridge.
	Namespace string `json:"namespace,omitempty"`
	// NodeSelector pins pods to nodes carrying every listed label, such as
	// Invitro's loader-nodetype=worker. Empty leaves pods unpinned.
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	// PodCIDR and ServiceCIDR name the cluster's own networks, which a
	// network-allowed sandbox is kept off. They apply to sandbox.deployment.
	PodCIDR     string `json:"pod_cidr,omitempty"`
	ServiceCIDR string `json:"service_cidr,omitempty"`
	// PrometheusURL is the Prometheus server pod resource measurements are read
	// from, reachable from where ARIES runs. It applies to sandbox.deployment;
	// without it, measurements are reported as unsupported.
	PrometheusURL string `json:"prometheus_url,omitempty"`
	// ServiceAccount lets the bridge pod exec into sandbox pods. It applies to
	// bridge.deployment and defaults to "aries-bridge", which the chart creates.
	ServiceAccount string `json:"service_account,omitempty"`
	// ImagePullSecret names a registry credential in Namespace.
	ImagePullSecret string `json:"image_pull_secret,omitempty"`
	// Images maps an image ARIES builds locally, such as versions.bridge.image,
	// to the reference the cluster pulls it by.
	Images map[string]string `json:"images,omitempty"`
}

// NormalizeDeployment applies compatibility defaults and validates placement.
// It does no I/O and also supports configurations constructed by command callers.
// Calling it repeatedly preserves the normalized configuration.
func (c *Config) NormalizeDeployment() error {
	if c.Sandbox.Type != "" && c.Sandbox.Type != "docker" {
		return fmt.Errorf("sandbox.type: unsupported legacy value %q", c.Sandbox.Type)
	}
	if c.Sandbox.Type == "docker" && c.Sandbox.Deployment.Backend != "" && c.Sandbox.Deployment.Backend != "docker" {
		return fmt.Errorf("sandbox.type conflicts with sandbox.deployment.backend %q", c.Sandbox.Deployment.Backend)
	}
	if err := c.Harness.Deployment.normalize("harness.deployment"); err != nil {
		return err
	}
	if err := c.Sandbox.Deployment.normalize("sandbox.deployment"); err != nil {
		return err
	}
	if c.Harness.Deployment.Backend == "docker" && c.Sandbox.Deployment.Backend == "docker" && c.Harness.Deployment.Docker.Socket != c.Sandbox.Deployment.Docker.Socket {
		return fmt.Errorf("harness.deployment.docker.socket and sandbox.deployment.docker.socket must select the same local Docker daemon")
	}
	if c.Bridge.Mode == "" {
		c.Bridge.Mode = "managed"
	}
	if c.Bridge.Mode == "embedded" {
		return fmt.Errorf("bridge.mode embedded is no longer supported; migrate to mode managed with bridge.deployment.backend docker")
	}
	if c.Bridge.Mode != "managed" {
		return fmt.Errorf("bridge.mode %q is unsupported; use managed", c.Bridge.Mode)
	}
	if err := c.Bridge.Deployment.normalize("bridge.deployment"); err != nil {
		return err
	}
	if c.Bridge.Deployment.Backend == "docker" && c.Sandbox.Deployment.Backend == "docker" && c.Bridge.Deployment.Docker.Socket != c.Sandbox.Deployment.Docker.Socket {
		return fmt.Errorf("bridge.deployment.docker.socket and sandbox.deployment.docker.socket must select the same local Docker daemon")
	}
	for _, placement := range []struct {
		path string
		cfg  *DeploymentConfig
	}{{"harness.deployment", &c.Harness.Deployment}, {"bridge.deployment", &c.Bridge.Deployment}} {
		if placement.cfg.Docker != nil && placement.cfg.Docker.PrometheusURL != "" {
			return fmt.Errorf("%s.docker: prometheus_url applies only to sandbox.deployment", placement.path)
		}
	}
	if err := c.normalizeKubernetes(); err != nil {
		return err
	}
	c.Sandbox.Type = ""
	return nil
}

// normalizeKubernetes checks the one composition the Kubernetes provider
// supports: all three components in a single namespace. The bridge pod execs
// into sandbox pods and harness pods dial the bridge pod, so a component left
// on Docker or in another namespace could not be reached.
func (c *Config) normalizeKubernetes() error {
	placements := []struct {
		path string
		cfg  *DeploymentConfig
	}{{"harness.deployment", &c.Harness.Deployment}, {"sandbox.deployment", &c.Sandbox.Deployment}, {"bridge.deployment", &c.Bridge.Deployment}}
	selected := 0
	for _, placement := range placements {
		if placement.cfg.Backend == "kubernetes" {
			selected++
		}
	}
	if selected == 0 {
		return nil
	}
	if selected != len(placements) {
		return fmt.Errorf("harness, sandbox and bridge must all use the kubernetes deployment backend together")
	}
	namespace := c.Sandbox.Deployment.Kubernetes.Namespace
	for _, placement := range placements {
		k := placement.cfg.Kubernetes
		if k.Namespace != namespace {
			return fmt.Errorf("%s.kubernetes.namespace must match sandbox.deployment.kubernetes.namespace %q", placement.path, namespace)
		}
		if placement.cfg != &c.Sandbox.Deployment && (k.PodCIDR != "" || k.ServiceCIDR != "") {
			return fmt.Errorf("%s.kubernetes: pod_cidr and service_cidr apply only to sandbox.deployment", placement.path)
		}
		if placement.cfg != &c.Sandbox.Deployment && k.PrometheusURL != "" {
			return fmt.Errorf("%s.kubernetes: prometheus_url applies only to sandbox.deployment", placement.path)
		}
		if placement.cfg != &c.Bridge.Deployment && k.ServiceAccount != "" {
			return fmt.Errorf("%s.kubernetes: service_account applies only to bridge.deployment", placement.path)
		}
	}
	if c.Bridge.Deployment.Kubernetes.ServiceAccount == "" {
		c.Bridge.Deployment.Kubernetes.ServiceAccount = "aries-bridge"
	}
	return nil
}

func (d *DeploymentConfig) normalize(path string) error {
	if d.Backend == "" {
		if d.Docker != nil || d.Kubernetes != nil {
			return fmt.Errorf("%s.backend is required with backend options", path)
		}
		d.Backend = "docker"
	}
	switch d.Backend {
	case "kubernetes":
		if d.Docker != nil {
			return fmt.Errorf("%s.docker does not apply to backend kubernetes", path)
		}
		if d.Kubernetes == nil {
			d.Kubernetes = &KubernetesDeploymentConfig{}
		}
		if d.Kubernetes.Namespace == "" {
			d.Kubernetes.Namespace = "aries"
		}
		for _, cidr := range []string{d.Kubernetes.PodCIDR, d.Kubernetes.ServiceCIDR} {
			if cidr == "" {
				continue
			}
			if _, err := netip.ParsePrefix(cidr); err != nil {
				return fmt.Errorf("%s.kubernetes: %q is not a CIDR", path, cidr)
			}
		}
		if address := d.Kubernetes.PrometheusURL; address != "" {
			u, err := url.Parse(address)
			if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("%s.kubernetes.prometheus_url must be an http or https URL", path)
			}
		}
		for local, remote := range d.Kubernetes.Images {
			if strings.TrimSpace(local) == "" || strings.TrimSpace(remote) == "" || strings.ContainsAny(local+remote, " \t\r\n\x00") {
				return fmt.Errorf("%s.kubernetes.images entries must map one image reference to another", path)
			}
		}
	case "docker":
		if d.Kubernetes != nil {
			return fmt.Errorf("%s.kubernetes does not apply to backend docker", path)
		}
		socket := ""
		if d.Docker != nil {
			socket = d.Docker.Socket
		}
		if socket == "" {
			socket = "/var/run/docker.sock"
		}
		if strings.HasPrefix(socket, "unix://") {
			u, err := url.Parse(socket)
			if err != nil || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
				return fmt.Errorf("%s.docker.socket must be a local absolute Unix socket path", path)
			}
			socket = u.Path
		}
		if !filepath.IsAbs(socket) || strings.ContainsAny(socket, "\x00\r\n") || strings.TrimSpace(socket) != socket || filepath.Clean(socket) == "/" {
			return fmt.Errorf("%s.docker.socket must be a local absolute Unix socket path", path)
		}
		prometheus := ""
		if d.Docker != nil {
			prometheus = d.Docker.PrometheusURL
		}
		if prometheus != "" {
			u, err := url.Parse(prometheus)
			if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("%s.docker.prometheus_url must be an http or https URL", path)
			}
		}
		d.Docker = &DockerDeploymentConfig{Socket: filepath.Clean(socket), PrometheusURL: prometheus}
	default:
		return fmt.Errorf("%s.backend %q is unsupported", path, d.Backend)
	}
	return nil
}
