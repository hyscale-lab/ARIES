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

func TestApptainerRunsOneTaskAtATimeAndNotSWEbenchPro(t *testing.T) {
	placement := config.DeploymentConfig{Backend: "apptainer", Apptainer: &config.ApptainerDeploymentConfig{StateDir: "/scratch/aries/state", ImageDir: "/scratch/aries/images"}}
	valid := func() config.Config {
		return config.Config{
			Execution: config.ExecutionConfig{Concurrency: 1},
			Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
			Harness:   config.HarnessConfig{Type: "hermes", Deployment: placement},
			Sandbox:   config.SandboxConfig{Deployment: placement},
			Bridge:    config.BridgeConfig{Type: "hermes-ssh", Deployment: placement},
			Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
		}
	}
	if err := validateComponents(valid()); err != nil {
		t.Fatalf("a valid Apptainer run: %v", err)
	}
	for name, tc := range map[string]struct {
		change func(*config.Config)
		want   string
	}{
		"concurrency":   {func(c *config.Config) { c.Execution.Concurrency = 2 }, "one task at a time"},
		"SWE-bench Pro": {func(c *config.Config) { c.Benchmark.Type = "swebenchpro" }, "SWE-bench Pro"},
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
