package benchmark

import (
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/benchmark/toolathlon"
	"github.com/hyscale-lab/aries/pkg/config"
)

// NewToolathlon constructs the benchmark for preparation or execution.
func NewToolathlon(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (*toolathlon.Benchmark, error) {
	return toolathlon.New(toolathlonOptions(cfg, taskIDs, executionIDs, outputRoot, lookup))
}

// SetupToolathlon prepares the pinned benchmark data.
func SetupToolathlon(ctx context.Context, cfg config.Config) error {
	return toolathlon.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.Toolathlon.RepositoryURL, cfg.Versions.Toolathlon.Revision)
}

// ValidateToolathlon checks the components a Toolathlon profile needs.
// Toolathlon's tools reach the harness only as an MCP server; both
// harnesses have an MCP client. The gateway itself is a service of each
// task's sandbox that the harness adds to its MCP servers when it starts,
// so a profile's own harness.mcp_servers entries are extra servers and may
// not take its name.
func ValidateToolathlon(cfg config.Config) error {
	if cfg.Harness.Type != "hermes" && cfg.Harness.Type != "openclaw" {
		return fmt.Errorf("benchmark type \"toolathlon\" requires a harness with an MCP client (hermes or openclaw), not %q", cfg.Harness.Type)
	}
	for _, server := range cfg.Harness.MCPServers {
		if server.Name == toolathlon.GatewayServerName {
			return fmt.Errorf("harness.mcp_servers may not name %q: the adapter adds Toolathlon's gateway to the harness itself", toolathlon.GatewayServerName)
		}
	}
	return nil
}

// toolathlonOptions maps the profile onto the adapter. The model ID is
// bookkeeping for Toolathlon's task bundle; the harness owns the model.
// lookup reads the environment variables the profile names for account
// credentials, as it reads a model's API key.
func toolathlonOptions(cfg config.Config, taskIDs, executionIDs []string, outputDir string, lookup func(string) ([]byte, bool)) toolathlon.Options {
	options := toolathlon.Options{
		Root: cfg.Benchmark.Root, TaskIDs: taskIDs, ExecutionTaskIDs: executionIDs, OutputDir: outputDir,
		Revision:         cfg.Versions.Toolathlon.Revision,
		Environment:      environmentFromConfig(cfg.Benchmark.Environment),
		ModelName:        cfg.Model.ID,
		HarnessWebSearch: cfg.Harness.WebSearch.Enabled,
		Concurrency:      cfg.Execution.Concurrency,
	}
	if settings := cfg.Benchmark.Toolathlon; settings != nil {
		options.GatewayPort = settings.GatewayPort
		options.AppHost = settings.AppHost
		options.MaxSteps = settings.MaxSteps
		options.CredentialsEnv = settings.CredentialsEnv
		options.CredentialFilesEnv = settings.CredentialFilesEnv
	}
	options.SecretLookup = lookup
	return options
}
