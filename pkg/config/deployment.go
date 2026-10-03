package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// DeploymentConfig selects placement independently of the component identity.
// Kubernetes settings are recognized for preflight's not-implemented error.
type DeploymentConfig struct {
	Backend    string                      `json:"backend"`
	Docker     *DockerDeploymentConfig     `json:"docker,omitempty"`
	Kubernetes *KubernetesDeploymentConfig `json:"kubernetes,omitempty"`
}

type DockerDeploymentConfig struct {
	Socket string `json:"socket,omitempty"`
}

type KubernetesDeploymentConfig struct {
	Context          string `json:"context,omitempty"`
	Namespace        string `json:"namespace,omitempty"`
	RuntimeClassName string `json:"runtime_class_name,omitempty"`
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
		c.Bridge.Mode = "embedded"
	}
	if c.Bridge.Mode != "embedded" {
		return fmt.Errorf("bridge.mode %q is unsupported; only embedded is implemented", c.Bridge.Mode)
	}
	c.Sandbox.Type = ""
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
	case "docker":
		if d.Kubernetes != nil {
			return fmt.Errorf("%s.kubernetes requires backend kubernetes", path)
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
		d.Docker = &DockerDeploymentConfig{Socket: filepath.Clean(socket)}
	case "kubernetes":
		if d.Docker != nil {
			return fmt.Errorf("%s.docker requires backend docker", path)
		}
		if d.Kubernetes == nil {
			d.Kubernetes = &KubernetesDeploymentConfig{}
		}
		k := d.Kubernetes
		if strings.TrimSpace(k.Context) != k.Context || strings.ContainsFunc(k.Context, unicode.IsControl) {
			return fmt.Errorf("%s.kubernetes.context must not contain surrounding whitespace or control characters", path)
		}
		if k.Namespace != "" && (len(k.Namespace) > 63 || !kubernetesLabel.MatchString(k.Namespace)) {
			return fmt.Errorf("%s.kubernetes.namespace must be a DNS label", path)
		}
		if k.RuntimeClassName != "" {
			if len(k.RuntimeClassName) > 253 {
				return fmt.Errorf("%s.kubernetes.runtime_class_name must be a DNS subdomain", path)
			}
			for _, label := range strings.Split(k.RuntimeClassName, ".") {
				if !kubernetesLabel.MatchString(label) {
					return fmt.Errorf("%s.kubernetes.runtime_class_name must be a DNS subdomain", path)
				}
			}
		}
	default:
		return fmt.Errorf("%s.backend %q is unsupported", path, d.Backend)
	}
	return nil
}

var kubernetesLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
