package openclaw

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Plugin skills become extra skill roots, and a plugin's MCP servers keep
// their working directory and fixed headers in OpenClaw's own fields.
func TestRenderConfigRendersPluginSkillsAndServerFields(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), ModeAgent, "http://search.example:8123", false, false, false, 0, MCPOptions{
		Servers: []core.MCPServerConfig{
			{Name: "local", Command: "/run/aries/plugins/demo/bin/server", Cwd: "/run/aries/plugins/demo"},
			{Name: "remote", URL: "https://mcp.example.com/mcp", Transport: "streamable-http", Headers: map[string]string{"X-Tenant": "public"}},
		},
		SkillDirs: []string{"/run/aries/plugins/demo/skills"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rendered struct {
		Skills struct {
			Load struct {
				ExtraDirs []string `json:"extraDirs"`
			} `json:"load"`
		} `json:"skills"`
		MCP struct {
			Servers map[string]openClawMCPServer `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(content, &rendered); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rendered.Skills.Load.ExtraDirs, []string{"/run/aries/plugins/demo/skills"}) {
		t.Fatalf("extraDirs = %v", rendered.Skills.Load.ExtraDirs)
	}
	if local := rendered.MCP.Servers["local"]; local.Cwd != "/run/aries/plugins/demo" || local.Transport != "stdio" {
		t.Fatalf("local server = %+v", local)
	}
	if remote := rendered.MCP.Servers["remote"]; remote.Headers["X-Tenant"] != "public" || remote.Transport != "streamable-http" {
		t.Fatalf("remote server = %+v", remote)
	}

	without, err := renderConfig(testModel(), testEndpoint(), ModeAgent, "http://search.example:8123", false, false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var plain map[string]any
	if err := json.Unmarshal(without, &plain); err != nil {
		t.Fatal(err)
	}
	if _, present := plain["skills"]; present {
		t.Fatal("a run without plugins must not render skills")
	}
}
