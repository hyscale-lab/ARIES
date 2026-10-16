package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
)

// DeploymentConfig selects placement independently of the component identity.
type DeploymentConfig struct {
	Backend   string                     `json:"backend"`
	Docker    *DockerDeploymentConfig    `json:"docker,omitempty"`
	Apptainer *ApptainerDeploymentConfig `json:"apptainer,omitempty"`
}

type DockerDeploymentConfig struct {
	Socket string `json:"socket,omitempty"`
}

// ApptainerDeploymentConfig places runtimes in Apptainer instances on the
// local host, for hosts where a normal user cannot run Docker. Harness,
// sandbox and bridge share one configuration.
type ApptainerDeploymentConfig struct {
	// Binary is the apptainer executable; empty uses apptainer on PATH.
	Binary string `json:"binary,omitempty"`
	// StateDir holds each runtime's record, environment files and logs.
	StateDir string `json:"state_dir"`
	// ImageDir holds the SIF images that aries setup converts.
	ImageDir string `json:"image_dir"`
	// OverlayDir holds each runtime's writable layer; empty uses StateDir.
	// Node-local disk is much faster than a parallel filesystem.
	OverlayDir string `json:"overlay_dir,omitempty"`
	// Images maps an image reference to a SIF file prepared elsewhere, such
	// as an image ARIES derives with a Docker build.
	Images map[string]string `json:"images,omitempty"`
	// UnenforcedLimits accepts CPU and memory limits that Apptainer cannot
	// enforce without cgroups v2. Without it such limits are refused.
	UnenforcedLimits bool `json:"unenforced_limits,omitempty"`
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
	// Apptainer runtimes of one run share a state directory: the bridge reads
	// the sandboxes' records there before it executes in them.
	placements := []DeploymentConfig{c.Harness.Deployment, c.Sandbox.Deployment, c.Bridge.Deployment}
	for _, placement := range placements {
		if placement.Backend == "apptainer" && (placements[0].Backend != "apptainer" || placements[1].Backend != "apptainer" || placements[2].Backend != "apptainer" ||
			!reflect.DeepEqual(placement.Apptainer, placements[0].Apptainer)) {
			return fmt.Errorf("harness, sandbox and bridge must all use the same Apptainer deployment")
		}
	}
	c.Sandbox.Type = ""
	return nil
}

func (d *DeploymentConfig) normalize(path string) error {
	if d.Backend == "" {
		if d.Docker != nil || d.Apptainer != nil {
			return fmt.Errorf("%s.backend is required with backend options", path)
		}
		d.Backend = "docker"
	}
	switch d.Backend {
	case "docker":
		if d.Apptainer != nil {
			return fmt.Errorf("%s.apptainer is set for backend docker", path)
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
	case "apptainer":
		if d.Docker != nil {
			return fmt.Errorf("%s.docker is set for backend apptainer", path)
		}
		if d.Apptainer == nil {
			return fmt.Errorf("%s.apptainer is required for backend apptainer", path)
		}
		return d.Apptainer.validate(path + ".apptainer")
	default:
		return fmt.Errorf("%s.backend %q is unsupported", path, d.Backend)
	}
	return nil
}

func (a *ApptainerDeploymentConfig) validate(path string) error {
	if a.Binary != "" && (strings.ContainsAny(a.Binary, "\x00\r\n") || strings.TrimSpace(a.Binary) != a.Binary || strings.Contains(a.Binary, "/") && !cleanAbsolute(a.Binary)) {
		return fmt.Errorf("%s.binary must be a command name or a clean absolute path", path)
	}
	for field, value := range map[string]string{"state_dir": a.StateDir, "image_dir": a.ImageDir} {
		if !cleanAbsolute(value) {
			return fmt.Errorf("%s.%s must be a clean absolute path", path, field)
		}
	}
	if a.OverlayDir != "" && !cleanAbsolute(a.OverlayDir) {
		return fmt.Errorf("%s.overlay_dir must be a clean absolute path", path)
	}
	for reference, sif := range a.Images {
		if strings.TrimSpace(reference) == "" || !cleanAbsolute(sif) {
			return fmt.Errorf("%s.images must map image references to clean absolute SIF paths", path)
		}
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsAny(path, "\x00\r\n")
}
