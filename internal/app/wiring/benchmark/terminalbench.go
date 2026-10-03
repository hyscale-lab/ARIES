package benchmark

import (
	"context"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/config"
)

// NewTerminalBench constructs the benchmark for preparation or execution.
func NewTerminalBench(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (*terminalbench.Benchmark, error) {
	var verifierTimeoutFloor time.Duration
	if cfg.Overrides.VerifierTimeoutFloor != nil {
		verifierTimeoutFloor = *cfg.Overrides.VerifierTimeoutFloor
	}
	return terminalbench.New(terminalbench.Options{
		Root:                 cfg.Benchmark.Root,
		TaskIDs:              taskIDs,
		ExecutionTaskIDs:     executionIDs,
		OutputDir:            outputRoot,
		Revision:             cfg.Versions.TerminalBench2.Revision,
		VerifierTimeoutFloor: verifierTimeoutFloor,
	})
}

// SetupTerminalBench prepares the pinned benchmark data.
func SetupTerminalBench(ctx context.Context, cfg config.Config) error {
	return terminalbench.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.TerminalBench2.RepositoryURL, cfg.Versions.TerminalBench2.Revision)
}
