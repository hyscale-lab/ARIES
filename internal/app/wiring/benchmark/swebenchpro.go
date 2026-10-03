package benchmark

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/benchmark/swebenchpro"
	"github.com/hyscale-lab/aries/pkg/config"
)

// NewSWEbenchPro constructs the benchmark for preparation or execution.
func NewSWEbenchPro(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (*swebenchpro.Benchmark, error) {
	return swebenchpro.New(swebenchpro.Options{
		Root:              cfg.Benchmark.Root,
		TaskIDs:           taskIDs,
		ExecutionTaskIDs:  executionIDs,
		OutputDir:         outputRoot,
		DatasetRevision:   cfg.Versions.SWEbenchPro.DatasetRevision,
		EvaluatorRevision: cfg.Versions.SWEbenchPro.EvaluatorRevision,
	})
}

// SetupSWEbenchPro prepares the pinned benchmark data.
func SetupSWEbenchPro(ctx context.Context, cfg config.Config) error {
	return swebenchpro.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.SWEbenchPro.DatasetRepositoryURL, cfg.Versions.SWEbenchPro.DatasetRevision, cfg.Versions.SWEbenchPro.EvaluatorRepositoryURL, cfg.Versions.SWEbenchPro.EvaluatorRevision)
}
