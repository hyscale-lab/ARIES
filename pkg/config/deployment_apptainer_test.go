package config

import (
	"strings"
	"testing"
)

func TestApptainerDeploymentValidation(t *testing.T) {
	apptainer := func() *ApptainerDeploymentConfig {
		return &ApptainerDeploymentConfig{StateDir: "/scratch/aries/state", ImageDir: "/scratch/aries/images", OverlayDir: "/var/tmp/aries", Images: map[string]string{"example.test/hermes:1": "/scratch/aries/hermes.sif"}}
	}
	placement := func(a *ApptainerDeploymentConfig) DeploymentConfig {
		return DeploymentConfig{Backend: "apptainer", Apptainer: a}
	}
	valid := func() Config {
		return Config{
			Harness: HarnessConfig{Deployment: placement(apptainer())},
			Sandbox: SandboxConfig{Deployment: placement(apptainer())},
			Bridge:  BridgeConfig{Deployment: placement(apptainer())},
		}
	}
	cfg := valid()
	if err := cfg.NormalizeDeployment(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		change func(*Config)
		want   string
	}{
		"missing block":    {func(c *Config) { c.Sandbox.Deployment.Apptainer = nil }, "sandbox.deployment.apptainer is required"},
		"relative state":   {func(c *Config) { c.Harness.Deployment.Apptainer.StateDir = "state" }, "state_dir"},
		"unclean images":   {func(c *Config) { c.Harness.Deployment.Apptainer.ImageDir = "/scratch/../images" }, "image_dir"},
		"relative overlay": {func(c *Config) { c.Sandbox.Deployment.Apptainer.OverlayDir = "tmp" }, "overlay_dir"},
		"relative SIF":     {func(c *Config) { c.Bridge.Deployment.Apptainer.Images = map[string]string{"x:1": "x.sif"} }, "images"},
		"relative binary":  {func(c *Config) { c.Bridge.Deployment.Apptainer.Binary = "bin/apptainer" }, "binary"},
		"docker beside":    {func(c *Config) { c.Bridge.Deployment.Docker = &DockerDeploymentConfig{} }, "docker does not apply to backend apptainer"},
		"docker bridge":    {func(c *Config) { c.Bridge.Deployment = DeploymentConfig{Backend: "docker"} }, "same Apptainer deployment"},
		"docker harness":   {func(c *Config) { c.Harness.Deployment = DeploymentConfig{} }, "same Apptainer deployment"},
		"two state dirs":   {func(c *Config) { c.Bridge.Deployment.Apptainer.StateDir = "/scratch/other" }, "same Apptainer deployment"},
		"apptainer for docker": {func(c *Config) {
			c.Harness.Deployment = DeploymentConfig{Backend: "docker", Apptainer: apptainer()}
		}, "apptainer does not apply to backend docker"},
		"apptainer for kubernetes": {func(c *Config) {
			c.Harness.Deployment = DeploymentConfig{Backend: "kubernetes", Apptainer: apptainer()}
		}, "apptainer does not apply to backend kubernetes"},
		"kubernetes beside": {func(c *Config) {
			c.Bridge.Deployment.Kubernetes = &KubernetesDeploymentConfig{}
		}, "kubernetes does not apply to backend apptainer"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.change(&cfg)
			err := cfg.NormalizeDeployment()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v; want %q", err, tc.want)
			}
		})
	}
}
