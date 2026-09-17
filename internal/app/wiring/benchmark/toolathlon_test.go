package benchmark

import (
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/benchmark/toolathlon"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
)

// The adapter starts Toolathlon's gateway at the sandbox's alias on the
// gateway port and adds it to the harness's MCP servers itself, ahead of any
// server the profile names; the profile may not name one after it, and a
// harness without an MCP client is refused.
func TestToolathlonGatewayIsAddedToTheHarness(t *testing.T) {
	base := func(servers ...core.MCPServerConfig) config.Config {
		return config.Config{
			Benchmark: config.BenchmarkConfig{Type: "toolathlon"},
			Harness:   config.HarnessConfig{Type: "hermes", MCPServers: servers},
		}
	}
	docs := core.MCPServerConfig{Name: "docs", URL: "https://docs.example/mcp", Transport: "streamable-http", TimeoutSeconds: 30}
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string
	}{
		{name: "no profile servers", cfg: base()},
		{name: "another server beside the gateway", cfg: base(docs)},
		{name: "the gateway's name taken", cfg: base(core.MCPServerConfig{Name: "toolathlon", URL: "http://task-sandbox:10086/sse", Transport: "sse"}), want: `harness.mcp_servers may not name "toolathlon"`},
		{name: "openclaw harness", cfg: func() config.Config {
			cfg := base()
			cfg.Harness.Type = "openclaw"
			return cfg
		}()},
		{name: "a harness without an MCP client", cfg: func() config.Config {
			cfg := base()
			cfg.Harness.Type = "other"
			return cfg
		}(), want: "requires a harness with an MCP client"},
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

	gateway := core.MCPServerConfig{Name: "toolathlon", URL: "http://task-sandbox:10086/sse", Transport: "sse", TimeoutSeconds: toolathlon.GatewayCallTimeoutSeconds}
	if got := ToolathlonMCPServers(base()); !reflect.DeepEqual(got, []core.MCPServerConfig{gateway}) {
		t.Fatalf("servers = %+v, want the gateway alone", got)
	}
	if got := ToolathlonMCPServers(base(docs)); !reflect.DeepEqual(got, []core.MCPServerConfig{gateway, docs}) {
		t.Fatalf("servers = %+v, want the gateway then the profile's", got)
	}
	// A profile that moves the gateway port moves the entry with it.
	moved := base()
	moved.Benchmark.Toolathlon = &config.ToolathlonConfig{GatewayPort: 20086}
	if got := ToolathlonMCPServers(moved); len(got) != 1 || got[0].URL != "http://task-sandbox:20086/sse" {
		t.Fatalf("moved port: servers = %+v", got)
	}
}
