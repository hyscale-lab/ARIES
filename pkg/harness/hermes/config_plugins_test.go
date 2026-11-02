package hermes

import (
	"reflect"
	"slices"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"go.yaml.in/yaml/v3"
)

// Plugin skills become external skill directories read through Hermes's
// skills toolset, which the explicit API toolset list then names; plugin MCP
// servers keep their working directory and fixed headers.
func TestRenderConfigRendersPluginSkillsAndServerFields(t *testing.T) {
	settings := baseSettings()
	settings.skillDirs = []string{"/run/aries/plugins/demo/skills"}
	settings.mcpServers = []core.MCPServerConfig{
		{Name: "local", Command: "/run/aries/plugins/demo/bin/server", Cwd: "/run/aries/plugins/demo"},
		{Name: "remote", URL: "https://mcp.example.com/mcp", Headers: map[string]string{"X-Tenant": "public"}},
	}
	rendered, err := renderConfig(validModel(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Skills struct {
			ExternalDirs []string `yaml:"external_dirs"`
		} `yaml:"skills"`
		PlatformToolsets struct {
			APIServer []string `yaml:"api_server"`
		} `yaml:"platform_toolsets"`
		MCPServers map[string]map[string]any `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(rendered, &config); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Skills.ExternalDirs, []string{"/run/aries/plugins/demo/skills"}) {
		t.Fatalf("external_dirs = %v", config.Skills.ExternalDirs)
	}
	if !slices.Contains(config.PlatformToolsets.APIServer, "skills") {
		t.Fatalf("api_server toolsets = %v, want skills", config.PlatformToolsets.APIServer)
	}
	if config.MCPServers["local"]["cwd"] != "/run/aries/plugins/demo" {
		t.Fatalf("local server = %v", config.MCPServers["local"])
	}
	if headers, _ := config.MCPServers["remote"]["headers"].(map[string]any); headers["X-Tenant"] != "public" {
		t.Fatalf("remote server = %v", config.MCPServers["remote"])
	}

	plain, err := renderConfig(validModel(), baseSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var without map[string]any
	if err := yaml.Unmarshal(plain, &without); err != nil {
		t.Fatal(err)
	}
	if _, present := without["skills"]; present {
		t.Fatal("a run without plugin skills must not render skills")
	}
}
