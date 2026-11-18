package harness

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

func TestWithPluginServersAppendsAndRefusesSharedNames(t *testing.T) {
	profile := []core.MCPServerConfig{{Name: "docs", URL: "https://docs.example/mcp"}}
	plugins := []harnesscommon.Plugin{
		{Name: "memory", MCPServers: []core.MCPServerConfig{{Name: "memory", Command: "npx"}}},
	}
	servers, err := withPluginServers(profile, plugins)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "docs" || servers[1].Name != "memory" {
		t.Fatalf("servers = %+v", servers)
	}
	if len(profile) != 1 {
		t.Fatal("the profile's server list must not be modified")
	}
	clash := append(plugins, harnesscommon.Plugin{Name: "other", MCPServers: []core.MCPServerConfig{{Name: "docs", Command: "node"}}})
	if _, err := withPluginServers(profile, clash); err == nil || !strings.Contains(err.Error(), `plugin "other" MCP server "docs" has the name of a server from harness.mcp_servers`) {
		t.Fatalf("clash error = %v", err)
	}
}
