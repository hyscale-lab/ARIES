package benchmark

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

// The adapter starts Toolathlon's gateway at the sandbox's alias on the
// gateway port; a profile whose MCP server points anywhere else would start
// Hermes with no Toolathlon tools, so the endpoint is checked, not just the
// presence of a server.
func TestValidateToolathlonRequiresTheGateway(t *testing.T) {
	base := func(servers ...core.MCPServerConfig) config.Config {
		return config.Config{
			Benchmark: config.BenchmarkConfig{Type: "toolathlon"},
			Harness:   config.HarnessConfig{Type: "hermes", MCPServers: servers},
		}
	}
	gateway := core.MCPServerConfig{Name: "toolathlon", URL: "http://task-sandbox:10086/sse", Transport: "sse"}
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "gateway on the default port", cfg: base(gateway)},
		{name: "gateway beside another server", cfg: base(core.MCPServerConfig{Name: "docs", URL: "https://docs.example/mcp", Transport: "streamable-http"}, gateway)},
		{name: "no server", cfg: base(), want: "requires the hermes harness with a harness.mcp_servers entry"},
		{name: "unrelated host", cfg: base(core.MCPServerConfig{Name: "toolathlon", URL: "http://gateway.example:10086/sse", Transport: "sse"}), want: "requires a harness.mcp_servers entry at http://task-sandbox:10086"},
		{name: "wrong port", cfg: base(core.MCPServerConfig{Name: "toolathlon", URL: "http://task-sandbox:10087/sse", Transport: "sse"}), want: "requires a harness.mcp_servers entry at http://task-sandbox:10086"},
		{name: "https form", cfg: base(core.MCPServerConfig{Name: "toolathlon", URL: "https://task-sandbox:10086/sse", Transport: "sse"}), want: "requires a harness.mcp_servers entry at http://task-sandbox:10086"},
		{name: "openclaw harness", cfg: func() config.Config {
			cfg := base(gateway)
			cfg.Harness.Type = "openclaw"
			return cfg
		}(), want: "requires the hermes harness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateToolathlon(tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	// A profile that moves the gateway port must point its server at it.
	moved := base(core.MCPServerConfig{Name: "toolathlon", URL: "http://task-sandbox:20086/sse", Transport: "sse"})
	moved.Benchmark.Toolathlon = &config.ToolathlonConfig{GatewayPort: 20086}
	if err := ValidateToolathlon(moved); err != nil {
		t.Fatalf("moved port: %v", err)
	}
	moved.Benchmark.Toolathlon.GatewayPort = 10086
	if err := ValidateToolathlon(moved); err == nil || !strings.Contains(err.Error(), "http://task-sandbox:10086") {
		t.Fatalf("moved port mismatch: %v", err)
	}
}
