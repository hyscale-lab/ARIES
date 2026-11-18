package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestApptainerImagesLeaveOutOnlyHermesBase(t *testing.T) {
	images := []string{"example.test/task:1", "docker.io/nousresearch/hermes-agent:v1"}
	cfg := config.Config{Harness: config.HarnessConfig{Type: "hermes"}}
	cfg.Versions.Hermes.Image = "docker.io/nousresearch/hermes-agent:v1"
	if got := apptainerImages(cfg, images); !slices.Equal(got, []string{"example.test/task:1"}) {
		t.Fatalf("Hermes images = %q", got)
	}
	cfg.Harness.Type = "openclaw"
	if got := apptainerImages(cfg, images); !slices.Equal(got, images) {
		t.Fatalf("OpenClaw images = %q", got)
	}
}

func TestApptainerRunsTasksAtOnceButNotUnreachableModelsOrSWEbenchPro(t *testing.T) {
	placement := config.DeploymentConfig{Backend: "apptainer", Apptainer: &config.ApptainerDeploymentConfig{StateDir: "/scratch/aries/state", ImageDir: "/scratch/aries/images"}}
	valid := func() config.Config {
		return config.Config{
			Execution: config.ExecutionConfig{Concurrency: 4},
			Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
			Harness:   config.HarnessConfig{Type: "hermes", Deployment: placement},
			Sandbox:   config.SandboxConfig{Deployment: placement},
			Bridge:    config.BridgeConfig{Type: "hermes-ssh", Deployment: placement},
			Model:     config.ProfileModel{BaseURL: "http://10.150.7.122:30000/v1"},
			Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
		}
	}
	if err := validateComponents(valid()); err != nil {
		t.Fatalf("a valid Apptainer run with four tasks at once: %v", err)
	}
	// A managed server runs in the run's namespace, where loopback is the run's.
	managed := valid()
	managed.Runtime.Mode = "managed"
	managed.Model.BaseURL = "http://127.0.0.1:30000/v1"
	if err := validateComponents(managed); err != nil {
		t.Fatalf("a managed model server on loopback: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(*config.Config)
		want   string
	}{
		"loopback model":            {func(c *config.Config) { c.Model.BaseURL = "http://127.0.0.1:30000/v1" }, "external model server on loopback"},
		"localhost model":           {func(c *config.Config) { c.Model.BaseURL = "http://LocalHost:30000/v1" }, "external model server on loopback"},
		"localhost. model":          {func(c *config.Config) { c.Model.BaseURL = "http://localhost.:30000/v1" }, "external model server on loopback"},
		"IPv6 loopback":             {func(c *config.Config) { c.Model.BaseURL = "http://[::1]:30000/v1" }, "external model server on loopback"},
		"unspecified model":         {func(c *config.Config) { c.Model.BaseURL = "http://0.0.0.0:30000/v1" }, "external model server on loopback"},
		"model in the run's subnet": {func(c *config.Config) { c.Model.BaseURL = "http://10.0.2.15:30000/v1" }, "10.0.2.0/24 is the subnet slirp4netns gives the run's"},
		"model in a task's subnet":  {func(c *config.Config) { c.Model.BaseURL = "http://10.0.3.15:30000/v1" }, "10.0.3.0/24 is the subnet slirp4netns gives each task's"},
		"managed model in a task's subnet": {func(c *config.Config) {
			c.Runtime.Mode = "managed"
			c.Model.BaseURL = "http://10.0.3.2:30000/v1"
		}, "10.0.3.0/24"},
		"managed model on another loopback address": {func(c *config.Config) {
			c.Runtime.Mode = "managed"
			c.Model.BaseURL = "http://127.0.0.2:30000/v1"
		}, "cannot reach a managed model server"},
		"managed model over https": {func(c *config.Config) {
			c.Runtime.Mode = "managed"
			c.Model.BaseURL = "https://127.0.0.1:30000/v1"
		}, "cannot reach a managed model server"},
		"long state_dir": {func(c *config.Config) {
			// Harness, sandbox and bridge share one Apptainer deployment.
			long := &config.ApptainerDeploymentConfig{StateDir: "/scratch/" + strings.Repeat("s", 80), ImageDir: "/scratch/aries/images"}
			c.Harness.Deployment.Apptainer, c.Sandbox.Deployment.Apptainer, c.Bridge.Deployment.Apptainer = long, long, long
		}, "too long"},
		"SWE-bench Pro": {func(c *config.Config) { c.Benchmark.Type = "swebenchpro" }, "SWE-bench Pro"},
		"gRPC bridge":   {func(c *config.Config) { c.Bridge.Type = "hermes-grpc" }, "hermes-grpc"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.change(&cfg)
			if err := validateComponents(cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
