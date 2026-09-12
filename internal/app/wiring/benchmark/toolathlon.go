package benchmark

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/hyscale-lab/aries/pkg/benchmark/toolathlon"
	"github.com/hyscale-lab/aries/pkg/config"
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
// Toolathlon's tools reach the harness only as an MCP server, and only the
// Hermes harness renders one.
func ValidateToolathlon(cfg config.Config) error {
	if cfg.Harness.Type != "hermes" || len(cfg.Harness.MCPServers) == 0 {
		return errors.New("benchmark type \"toolathlon\" requires the hermes harness with a harness.mcp_servers entry for the gateway")
	}
	// And that server must be the gateway the adapter starts -- the
	// sandbox's alias on the gateway port. Any other endpoint would
	// start Hermes with no Toolathlon tools at all.
	if !hasToolathlonGateway(cfg) {
		return fmt.Errorf("benchmark type \"toolathlon\" requires a harness.mcp_servers entry at http://%s:%d, the gateway the adapter starts", taskSandboxAlias, toolathlonGatewayPort(cfg))
	}
	return nil
}

// toolathlonGatewayPort is the port the adapter will start the gateway on
// for this profile.
func toolathlonGatewayPort(cfg config.Config) int {
	if settings := cfg.Benchmark.Toolathlon; settings != nil && settings.GatewayPort != 0 {
		return settings.GatewayPort
	}
	return toolathlon.DefaultGatewayPort
}

// hasToolathlonGateway reports whether one of the profile's MCP servers is
// the sandbox's gateway: the sandbox's network alias on the gateway port.
func hasToolathlonGateway(cfg config.Config) bool {
	port := strconv.Itoa(toolathlonGatewayPort(cfg))
	for _, server := range cfg.Harness.MCPServers {
		parsed, err := url.Parse(server.URL)
		if err != nil {
			continue
		}
		if parsed.Hostname() == taskSandboxAlias && parsed.Port() == port {
			return true
		}
	}
	return false
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
