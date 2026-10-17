package app

import (
	"context"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// testBenchmarkLoader keeps catalog-load effects separate from occurrence effects
// in application lifecycle tests.
func testBenchmarkLoader(
	load func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error),
	occurrence func(config.Config, string, string, string, func(string) ([]byte, bool)) (runner.Benchmark, error),
) func(context.Context, config.Config, string, []string, func(string) ([]byte, bool)) (PreparedBenchmark, error) {
	return func(ctx context.Context, cfg config.Config, output string, ids []string, lookup func(string) ([]byte, bool)) (PreparedBenchmark, error) {
		var tasks []core.Task
		var err error
		if load != nil {
			tasks, err = load(ctx, cfg, ids, lookup)
		}
		catalog := PreparedBenchmark{Tasks: tasks}
		if occurrence != nil {
			catalog.NewOccurrence = func(logicalID, occurrenceID string) (runner.Benchmark, error) {
				return occurrence(cfg, output, logicalID, occurrenceID, lookup)
			}
		}
		return catalog, err
	}
}

func replaceTestOccurrence(load func(context.Context, config.Config, string, []string, func(string) ([]byte, bool)) (PreparedBenchmark, error), occurrence func(config.Config, string, string, string, func(string) ([]byte, bool)) (runner.Benchmark, error)) func(context.Context, config.Config, string, []string, func(string) ([]byte, bool)) (PreparedBenchmark, error) {
	return func(ctx context.Context, cfg config.Config, output string, ids []string, lookup func(string) ([]byte, bool)) (PreparedBenchmark, error) {
		catalog, err := load(ctx, cfg, output, ids, lookup)
		catalog.NewOccurrence = func(logicalID, occurrenceID string) (runner.Benchmark, error) {
			return occurrence(cfg, output, logicalID, occurrenceID, lookup)
		}
		return catalog, err
	}
}

func testPreparedBenchmark(t *testing.T, wiring Wiring) PreparedBenchmark {
	t.Helper()
	catalog, err := wiring.LoadBenchmark(context.Background(), config.Config{}, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
