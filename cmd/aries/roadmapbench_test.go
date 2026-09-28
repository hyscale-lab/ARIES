package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/roadmapbench"
	"github.com/hyscale-lab/aries/pkg/config"
)

func TestRoadmapBenchWiringSupportsExistingHarnessPairs(t *testing.T) {
	for _, harness := range []string{"codex", "hermes", "openclaw"} {
		t.Run(harness, func(t *testing.T) {
			cfg := config.Config{
				Benchmark: config.BenchmarkConfig{Type: "roadmapbench"},
				Harness:   config.HarnessConfig{Type: harness},
				Sandbox:   config.SandboxConfig{Type: "docker"},
				Bridge:    config.BridgeConfig{Type: harness + "-ssh"},
				Runtime:   config.RuntimeConfig{Backend: "openai", Mode: "external"},
			}
			if err := validateComponents(cfg); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRoadmapBenchWiringPassesExecutionAndVerifierOptions(t *testing.T) {
	cfg := config.Config{
		Benchmark: config.BenchmarkConfig{Type: "roadmapbench", Root: t.TempDir()},
		Versions: config.Versions{RoadmapBench: config.RoadmapBenchVersions{
			RepositoryURL: "https://huggingface.co/datasets/UnipatAI/RoadmapBench",
			Revision:      "59184e779909300a5a0150b06b945d39da81a099",
		}},
		OutputDir: t.TempDir(),
	}
	const taskID = "opt-3.0.0-roadmap"
	benchmark, err := newBenchmark(cfg, cfg.OutputDir, taskID, taskID+"-001", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := benchmark.(*roadmapbench.Benchmark); !ok {
		t.Fatalf("benchmark type = %T", benchmark)
	}
	if _, err := newBenchmark(cfg, cfg.OutputDir, taskID, "../escaped", nil); err == nil || !strings.Contains(err.Error(), "execution task ID") {
		t.Fatalf("execution ID was not checked: %v", err)
	}
	floor := -time.Second
	cfg.Overrides.VerifierTimeoutFloor = &floor
	if _, err := newBenchmark(cfg, cfg.OutputDir, taskID, taskID, nil); err == nil || !strings.Contains(err.Error(), "verifier timeout floor") {
		t.Fatalf("run verifier floor was not checked: %v", err)
	}
	if _, err := loadPreparationTasks(context.Background(), cfg, []string{taskID}, nil); err == nil || !strings.Contains(err.Error(), "verifier timeout floor") {
		t.Fatalf("setup verifier floor was not checked: %v", err)
	}
	cfg.Versions.RoadmapBench.RepositoryURL = ""
	if err := setupBenchmark(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("setup repository pin was not checked: %v", err)
	}
}
