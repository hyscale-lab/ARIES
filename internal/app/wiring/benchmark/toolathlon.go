package benchmark

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/benchmark/toolathlon"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

// taskSandboxAlias is the sandbox's name on its task network.
const taskSandboxAlias = "task-sandbox"

// NewToolathlon constructs the benchmark for preparation or execution.
func NewToolathlon(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, _ func(string) ([]byte, bool)) (*toolathlon.Benchmark, error) {
	return toolathlon.New(toolathlonOptions(cfg, taskIDs, executionIDs, outputRoot))
}

// SetupToolathlon prepares the pinned benchmark data.
func SetupToolathlon(ctx context.Context, cfg config.Config) error {
	return toolathlon.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.Toolathlon.RepositoryURL, cfg.Versions.Toolathlon.Revision)
}

// ValidateToolathlon checks the components a Toolathlon profile needs.
// Toolathlon's tools reach the harness only as an MCP server, and the
// adapter is wired for the Hermes harness. The gateway itself is added to
// that harness's MCP servers by ToolathlonMCPServers, so a profile's own
// harness.mcp_servers entries are extra servers and may not take its name.
func ValidateToolathlon(cfg config.Config) error {
	if cfg.Harness.Type != "hermes" {
		return errors.New("benchmark type \"toolathlon\" requires the hermes harness")
	}
	for _, server := range cfg.Harness.MCPServers {
		if server.Name == toolathlon.GatewayServerName {
			return fmt.Errorf("harness.mcp_servers may not name %q: the adapter adds Toolathlon's gateway to the harness itself", toolathlon.GatewayServerName)
		}
	}
	return nil
}

// ToolathlonMCPServers is the harness's MCP server list for a Toolathlon
// profile: the gateway the adapter starts, at the sandbox's alias on the
// gateway port, then the profile's harness.mcp_servers entries.
func ToolathlonMCPServers(cfg config.Config) []core.MCPServerConfig {
	gateway := toolathlon.Gateway(taskSandboxAlias, toolathlonGatewayPort(cfg))
	out := make([]core.MCPServerConfig, 0, len(cfg.Harness.MCPServers)+1)
	out = append(out, core.MCPServerConfig{Name: gateway.Name, URL: gateway.URL, Transport: gateway.Transport, TimeoutSeconds: gateway.TimeoutSeconds})
	return append(out, cfg.Harness.MCPServers...)
}

// toolathlonGatewayPort is the port the adapter will start the gateway on
// for this profile.
func toolathlonGatewayPort(cfg config.Config) int {
	if settings := cfg.Benchmark.Toolathlon; settings != nil && settings.GatewayPort != 0 {
		return settings.GatewayPort
	}
	return toolathlon.DefaultGatewayPort
}

// toolathlonOptions maps the profile onto the adapter. The model ID is
// bookkeeping for Toolathlon's task bundle; the harness owns the model.
func toolathlonOptions(cfg config.Config, taskIDs, executionIDs []string, outputDir string) toolathlon.Options {
	options := toolathlon.Options{
		Root: cfg.Benchmark.Root, TaskIDs: taskIDs, ExecutionTaskIDs: executionIDs, OutputDir: outputDir,
		Revision:         cfg.Versions.Toolathlon.Revision,
		Environment:      environmentFromConfig(cfg.Benchmark.Environment),
		ModelName:        cfg.Model.ID,
		HarnessWebSearch: cfg.Harness.WebSearch.Enabled,
	}
	if settings := cfg.Benchmark.Toolathlon; settings != nil {
		options.GatewayPort = settings.GatewayPort
		options.AppHost = settings.AppHost
		options.MaxSteps = settings.MaxSteps
	}
	return options
}
