package benchmark

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/benchmark/sweatlas"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

// NewSWEAtlas constructs the benchmark for preparation or execution.
func NewSWEAtlas(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (*sweatlas.Benchmark, error) {
	if lookup == nil {
		lookup = environmentAPIKeyLookup
	}
	judgeModel, judgeDisabled := sweatlasModels(cfg)
	return sweatlas.New(sweatlas.Options{
		Root:             cfg.Benchmark.Root,
		TaskIDs:          taskIDs,
		ExecutionTaskIDs: executionIDs,
		OutputDir:        outputRoot,
		Revision:         cfg.Versions.SWEAtlas.Revision,
		Judge:            judgeModel,
		JudgeDisabled:    judgeDisabled,
		APIKeyLookup:     lookup,
	})
}

// SetupSWEAtlas prepares the pinned benchmark data.
func SetupSWEAtlas(ctx context.Context, cfg config.Config) error {
	return sweatlas.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.SWEAtlas.RepositoryURL, cfg.Versions.SWEAtlas.Revision)
}

func sweatlasModels(cfg config.Config) (judge core.ModelConfig, judgeDisabled bool) {
	judgeCfg := cfg.Benchmark.Judge
	if judgeCfg.Enabled != nil && !*judgeCfg.Enabled {
		return core.ModelConfig{}, true
	}
	return judgeCfg.CoreModel(), false
}
