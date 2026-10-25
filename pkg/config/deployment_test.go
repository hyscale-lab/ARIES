package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestDeploymentNormalization(t *testing.T) {
	for _, legacy := range []string{"", "docker"} {
		cfg := Config{Sandbox: SandboxConfig{Type: legacy}}
		if err := cfg.NormalizeDeployment(); err != nil {
			t.Fatal(err)
		}
		if cfg.Harness.Deployment.Backend != "docker" || cfg.Sandbox.Deployment.Docker.Socket != "/var/run/docker.sock" || cfg.Bridge.Mode != "managed" || cfg.Bridge.Deployment.Backend != "docker" || cfg.Sandbox.Type != "" {
			t.Fatalf("normalization: %#v", cfg)
		}
		before := cfg
		if err := cfg.NormalizeDeployment(); err != nil || !reflect.DeepEqual(before, cfg) {
			t.Fatalf("not idempotent: %v", err)
		}
	}
	cfg := Config{Harness: HarnessConfig{Deployment: DeploymentConfig{Backend: "docker", Docker: &DockerDeploymentConfig{Socket: "unix:///var/run/../run/docker.sock"}}}}
	if err := cfg.NormalizeDeployment(); err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Deployment.Docker.Socket != cfg.Sandbox.Deployment.Docker.Socket {
		t.Fatal("equivalent sockets differ")
	}
}

func TestDeploymentDecodeAndValidation(t *testing.T) {
	cases := []struct{ name, harness, sandbox, mode, want string }{
		{"legacy compatible", `{"backend":"docker"}`, `{"type":"docker","deployment":{"backend":"docker"}}`, "managed", ""},
		{"legacy conflict", `{}`, `{"type":"docker","deployment":{"backend":"remote"}}`, "managed", "sandbox.type"},
		{"unknown legacy", `{}`, `{"type":"other"}`, "managed", "sandbox.type"},
		{"unknown backend", `{"backend":"remote"}`, `{}`, "managed", "harness.deployment.backend"},
		{"missing backend", `{"docker":{}}`, `{}`, "managed", "harness.deployment.backend"},
		{"remote", `{"backend":"docker","docker":{"socket":"tcp://localhost:2375"}}`, `{}`, "managed", "harness.deployment.docker.socket"},
		{"relative", `{"backend":"docker","docker":{"socket":"docker.sock"}}`, `{}`, "managed", "harness.deployment.docker.socket"},
		{"unix host", `{"backend":"docker","docker":{"socket":"unix://remote/run/docker.sock"}}`, `{}`, "managed", "harness.deployment.docker.socket"},
		{"different daemons", `{"backend":"docker","docker":{"socket":"/other/docker.sock"}}`, `{}`, "managed", "same local Docker daemon"},
		{"unknown field", `{"backend":"docker","docker":{"host":"localhost"}}`, `{}`, "managed", "unknown field"},
		{"docker Prometheus", `{}`, `{"deployment":{"backend":"docker","docker":{"prometheus_url":"http://10.96.1.2:9090"}}}`, "managed", ""},
		{"harness docker Prometheus", `{"backend":"docker","docker":{"prometheus_url":"http://10.96.1.2:9090"}}`, `{}`, "managed", "prometheus_url applies only to sandbox.deployment"},
		{"malformed docker Prometheus", `{}`, `{"deployment":{"backend":"docker","docker":{"prometheus_url":"10.96.1.2:9090"}}}`, "managed", "docker.prometheus_url must be an http or https URL"},
		{"embedded bridge", `{}`, `{}`, "embedded", "migrate"},
		{"external bridge", `{}`, `{}`, "external", "bridge.mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(validConfig, `"harness":{"type":"openclaw"}`, `"harness":{"type":"openclaw","deployment":`+tc.harness+`}`, 1)
			input = strings.Replace(input, `"sandbox":{"type":"docker"}`, `"sandbox":`+tc.sandbox, 1)
			input = strings.Replace(input, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"openclaw-ssh","mode":"`+tc.mode+`"}`, 1)
			_, err := Decode(strings.NewReader(input))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error=%v; want %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}

		})
	}
}

func TestManagedBridgePlacementValidation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deployment DeploymentConfig
		want       string
	}{
		{"process rejected", DeploymentConfig{Backend: "process"}, "unsupported"},
		{"docker", DeploymentConfig{Backend: "docker"}, ""},
		{"process Docker options", DeploymentConfig{Backend: "process", Docker: &DockerDeploymentConfig{}}, "unsupported"},
		{"missing backend", DeploymentConfig{Docker: &DockerDeploymentConfig{}}, "backend is required"},
		{"different daemon", DeploymentConfig{Backend: "docker", Docker: &DockerDeploymentConfig{Socket: "/other/docker.sock"}}, "same local Docker daemon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Bridge: BridgeConfig{Deployment: tc.deployment}}
			err := c.NormalizeDeployment()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v; want %s", err, tc.want)
			}
		})
	}
	c := Config{Harness: HarnessConfig{Deployment: DeploymentConfig{Backend: "process"}}}
	if err := c.NormalizeDeployment(); err == nil {
		t.Fatal("process accepted for harness")
	}
}

func TestKubernetesDeploymentNormalization(t *testing.T) {
	kubernetes := func(k KubernetesDeploymentConfig) DeploymentConfig {
		return DeploymentConfig{Backend: "kubernetes", Kubernetes: &k}
	}
	cfg := Config{}
	cfg.Harness.Deployment = DeploymentConfig{Backend: "kubernetes"}
	cfg.Sandbox.Deployment = kubernetes(KubernetesDeploymentConfig{PodCIDR: "192.168.0.0/16", ServiceCIDR: "10.96.0.0/12", PrometheusURL: "http://10.96.1.2:9090"})
	cfg.Bridge.Deployment = kubernetes(KubernetesDeploymentConfig{Images: map[string]string{"aries-bridge": "registry.test/aries-bridge"}})
	if err := cfg.NormalizeDeployment(); err != nil {
		t.Fatal(err)
	}
	if cfg.Harness.Deployment.Kubernetes.Namespace != "aries" || cfg.Sandbox.Deployment.Kubernetes.Namespace != "aries" || cfg.Bridge.Deployment.Kubernetes.ServiceAccount != "aries-bridge" {
		t.Fatalf("defaults = %+v %+v %+v", cfg.Harness.Deployment.Kubernetes, cfg.Sandbox.Deployment.Kubernetes, cfg.Bridge.Deployment.Kubernetes)
	}
	if err := cfg.NormalizeDeployment(); err != nil {
		t.Fatalf("normalization is not idempotent: %v", err)
	}
	for name, test := range map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"mixed backends":  {func(c *Config) { c.Bridge.Deployment = DeploymentConfig{Backend: "docker"} }, "all use the kubernetes deployment backend"},
		"namespace":       {func(c *Config) { c.Harness.Deployment = kubernetes(KubernetesDeploymentConfig{Namespace: "other"}) }, "must match sandbox.deployment.kubernetes.namespace"},
		"harness CIDR":    {func(c *Config) { c.Harness.Deployment = kubernetes(KubernetesDeploymentConfig{PodCIDR: "10.0.0.0/8"}) }, "apply only to sandbox.deployment"},
		"sandbox account": {func(c *Config) { c.Sandbox.Deployment = kubernetes(KubernetesDeploymentConfig{ServiceAccount: "x"}) }, "applies only to bridge.deployment"},
		"bridge Prometheus": {func(c *Config) {
			c.Bridge.Deployment = kubernetes(KubernetesDeploymentConfig{PrometheusURL: "http://10.96.1.2:9090"})
		}, "prometheus_url applies only to sandbox.deployment"},
		"malformed Prometheus": {func(c *Config) {
			c.Sandbox.Deployment = kubernetes(KubernetesDeploymentConfig{PrometheusURL: "10.96.1.2:9090"})
		}, "prometheus_url must be an http or https URL"},
		"malformed CIDR": {func(c *Config) { c.Sandbox.Deployment = kubernetes(KubernetesDeploymentConfig{PodCIDR: "not-a-cidr"}) }, "is not a CIDR"},
		"docker options": {func(c *Config) { c.Sandbox.Deployment.Docker = &DockerDeploymentConfig{} }, "does not apply to backend kubernetes"},
		"kubernetes on docker": {func(c *Config) {
			c.Harness.Deployment, c.Bridge.Deployment = DeploymentConfig{}, DeploymentConfig{}
			c.Sandbox.Deployment = DeploymentConfig{Backend: "docker", Kubernetes: &KubernetesDeploymentConfig{}}
		}, "does not apply to backend docker"},
	} {
		c := Config{}
		c.Harness.Deployment = DeploymentConfig{Backend: "kubernetes"}
		c.Sandbox.Deployment = DeploymentConfig{Backend: "kubernetes"}
		c.Bridge.Deployment = DeploymentConfig{Backend: "kubernetes"}
		test.mutate(&c)
		if err := c.NormalizeDeployment(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want %q", name, err, test.want)
		}
	}
}
