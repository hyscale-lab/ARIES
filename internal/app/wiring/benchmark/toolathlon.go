package benchmark

import (
	"context"
	"errors"

	"github.com/hyscale-lab/aries/pkg/benchmark/toolathlon"
	"github.com/hyscale-lab/aries/pkg/config"
)

// NewToolathlon constructs the benchmark for preparation or execution.
func NewToolathlon(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, _ func(string) ([]byte, bool)) (*toolathlon.Benchmark, error) {
	return toolathlon.New(toolathlonOptions(cfg, taskIDs, executionIDs, outputRoot))
}

// SetupToolathlon prepares the pinned benchmark data.
func SetupToolathlon(ctx context.Context, cfg config.Config) error {
	return toolathlon.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.Toolathlon.RepositoryURL, cfg.Versions.Toolathlon.Revision)
}

// ValidateToolathlon checks the components a Toolathlon profile needs.
// Toolathlon's tools reach the harness only as an MCP server, and only the
// Hermes harness renders one.
func ValidateToolathlon(cfg config.Config) error {
	if cfg.Harness.Type != "hermes" || len(cfg.Harness.MCPServers) == 0 {
		return errors.New("benchmark type \"toolathlon\" requires the hermes harness with a harness.mcp_servers entry for the gateway")
	}
	return nil
}

// toolathlonOptions maps the profile onto the adapter. The model ID is
// bookkeeping for Toolathlon's task bundle; the harness owns the model.
func toolathlonOptions(cfg config.Config, taskIDs, executionIDs []string, outputDir string) toolathlon.Options {
	options := toolathlon.Options{
		Root: cfg.Benchmark.Root, TaskIDs: taskIDs, ExecutionTaskIDs: executionIDs, OutputDir: outputDir,
		Revision:    cfg.Versions.Toolathlon.Revision,
		Environment: environmentFromConfig(cfg.Benchmark.Environment),
		ModelName:   cfg.Model.ID,
	}
	if settings := cfg.Benchmark.Toolathlon; settings != nil {
		options.GatewayPort = settings.GatewayPort
		options.AppHost = settings.AppHost
		options.MaxSteps = settings.MaxSteps
	}
	return options
}
