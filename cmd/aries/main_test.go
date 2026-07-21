package main

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/config"
)

func TestBuildExperimentUsesExplicitTypeSwitches(t *testing.T) {
	valid := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "terminalbench2", Root: terminalbench.DefaultRoot, Tasks: []string{"fix-git"}},
		Harness:   config.HarnessConfig{Type: "openclaw"},
		Sandbox:   config.SandboxConfig{Type: "docker"},
		Bridge:    config.BridgeConfig{Type: "openclaw-ssh"},
		OutputDir: "runs",
	}

	tests := []struct {
		name    string
		change  func(*config.Config)
		wantErr string
	}{
		{"known but not wired", func(*config.Config) {}, "M3-M5 components are not implemented"},
		{"benchmark", func(c *config.Config) { c.Benchmark.Type = "other" }, `unsupported benchmark type "other"`},
		{"harness", func(c *config.Config) { c.Harness.Type = "other" }, `unsupported harness type "other"`},
		{"sandbox", func(c *config.Config) { c.Sandbox.Type = "other" }, `unsupported sandbox type "other"`},
		{"bridge", func(c *config.Config) { c.Bridge.Type = "other" }, `unsupported bridge type "other"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			test.change(&cfg)
			err := buildExperiment(cfg)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("buildExperiment() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestRunRejectsUnknownSetupComponent(t *testing.T) {
	err := run([]string{"setup", "other"})
	if err == nil || !strings.Contains(err.Error(), `unsupported setup component "other"`) {
		t.Fatalf("run() error = %v", err)
	}
}
