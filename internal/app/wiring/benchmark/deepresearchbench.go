package benchmark

import (
	"context"
	"fmt"
	"os"

	"github.com/hyscale-lab/aries/pkg/benchmark/deepresearchbench"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

// NewDeepResearchBench constructs the benchmark for preparation or execution.
func NewDeepResearchBench(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (*deepresearchbench.Benchmark, error) {
	if lookup == nil {
		lookup = environmentAPIKeyLookup
	}
	judgeModel, factModel, jinaAPIKeyEnv, judgeDisabled := deepresearchbenchModels(cfg)
	benchmark, err := deepresearchbench.New(deepresearchbench.Options{
		Root:             cfg.Benchmark.Root,
		TaskIDs:          taskIDs,
		ExecutionTaskIDs: executionIDs,
		OutputDir:        outputRoot,
		Revision:         cfg.Versions.DeepResearchBench.Revision,
		Environment:      environmentFromConfig(cfg.Benchmark.Environment),
		Judge:            judgeModel,
		JudgeDisabled:    judgeDisabled,
		FactJudge:        factModel,
		JinaAPIKeyEnv:    jinaAPIKeyEnv,
		APIKeyLookup:     lookup,
	})
	if err != nil {
		return nil, err
	}
	if reason := benchmark.FactSkipReason(); reason != "" {
		fmt.Fprintf(os.Stderr, "warning: %s\n", reason)
	}
	return benchmark, nil
}

// SetupDeepResearchBench prepares the pinned benchmark data.
func SetupDeepResearchBench(ctx context.Context, cfg config.Config) error {
	return deepresearchbench.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.DeepResearchBench.RepositoryURL, cfg.Versions.DeepResearchBench.Revision)
}

// deepresearchbenchModels defaults absent judge and FACT model settings to the
// main model. FACT inputs remain available when grading is disabled so the
// benchmark can explain why it skipped FACT.
func deepresearchbenchModels(cfg config.Config) (judge, fact core.ModelConfig, jinaAPIKeyEnv string, judgeDisabled bool) {
	if judgeCfg := cfg.Benchmark.Judge; judgeCfg != nil && judgeCfg.Enabled != nil && !*judgeCfg.Enabled {
		judgeDisabled = true
	} else {
		judge = cfg.CoreModel()
		if judgeCfg != nil {
			judge = judgeCfg.CoreModel()
		}
	}
	fact = cfg.CoreModel()
	if factCfg := cfg.Benchmark.Fact; factCfg != nil {
		jinaAPIKeyEnv = factCfg.JinaAPIKeyEnv
		if factCfg.Provider != "" || factCfg.BaseURL != "" || factCfg.ID != "" || factCfg.APIKeyEnv != "" {
			fact = factCfg.CoreModel()
		}
	}
	return judge, fact, jinaAPIKeyEnv, judgeDisabled
}

func environmentFromConfig(cfg *config.BenchmarkEnvironment) core.Environment {
	if cfg == nil {
		return core.Environment{}
	}
	return core.Environment{
		Image:     cfg.Image,
		Workdir:   cfg.Workdir,
		CPU:       cfg.CPU,
		MemoryMB:  cfg.MemoryMB,
		StorageMB: cfg.StorageMB,
		Env:       cfg.Env,
	}
}
