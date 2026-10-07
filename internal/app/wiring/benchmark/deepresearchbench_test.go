package benchmark

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

func TestDeepResearchModelDefaultsAndDisabledJudge(t *testing.T) {
	cfg := config.Config{
		Runtime: config.RuntimeConfig{Backend: "openai"},
		Model:   config.ProfileModel{ID: "main", BaseURL: "https://example.test/v1", APIKeyEnv: "MAIN_KEY"},
	}
	cfg.Benchmark.Fact = &config.FactConfig{JinaAPIKeyEnv: "JINA_KEY"}
	judge, fact, jina, disabled := deepresearchbenchModels(cfg)
	if judge != cfg.CoreModel() || fact != cfg.CoreModel() || jina != "JINA_KEY" || disabled {
		t.Fatalf("default models: judge=%+v fact=%+v jina=%q disabled=%v", judge, fact, jina, disabled)
	}
	enabled := false
	cfg.Benchmark.Judge = &config.JudgeConfig{Enabled: &enabled}
	judge, fact, jina, disabled = deepresearchbenchModels(cfg)
	if judge != (core.ModelConfig{}) || fact != cfg.CoreModel() || jina != "JINA_KEY" || !disabled {
		t.Fatal("disabled judge must retain FACT inputs for skip reporting")
	}
}

func TestDeepResearchNilLookupUsesEnvironment(t *testing.T) {
	cfg := config.Config{
		Runtime: config.RuntimeConfig{Backend: "openai"},
		Model:   config.ProfileModel{ID: "main", BaseURL: "https://example.test/v1", APIKeyEnv: "ARIES_WIRING_TEST_KEY"},
	}
	cfg.Benchmark.Root = t.TempDir()
	cfg.Benchmark.Environment = &config.BenchmarkEnvironment{Image: "test:1", Workdir: "/work", CPU: 2, MemoryMB: 512, StorageMB: 1024, Env: map[string]string{"LANG": "C"}}
	cfg.Benchmark.Fact = &config.FactConfig{JinaAPIKeyEnv: "ARIES_WIRING_TEST_JINA"}
	cfg.Versions.DeepResearchBench.Revision = "revision"
	t.Setenv("ARIES_WIRING_TEST_KEY", "test-only-key")
	t.Setenv("ARIES_WIRING_TEST_JINA", "")
	benchmark, err := NewDeepResearchBench(cfg, t.TempDir(), []string{"1"}, []string{"1-001"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(benchmark.FactSkipReason(), "ARIES_WIRING_TEST_JINA") {
		t.Fatalf("FACT skip reason = %q", benchmark.FactSkipReason())
	}
	// An explicit lookup takes precedence over process environment.
	_, err = NewDeepResearchBench(cfg, t.TempDir(), []string{"1"}, nil, func(string) ([]byte, bool) { return nil, false })
	if err == nil {
		t.Fatal("missing explicit judge key should fail construction")
	}
}
