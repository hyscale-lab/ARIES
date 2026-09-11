package hermes

import (
	"strings"
	"testing"
)

func TestRenderMCPServersRendersSSEServerBlock(t *testing.T) {
	rendered, err := renderMCPServers([]MCPServer{
		{Name: "toolathlon", URL: "http://task-sandbox:10086/sse", Transport: "sse", TimeoutSeconds: 300},
		{Name: "docs", URL: "https://mcp.example.invalid/mcp", Transport: "streamable-http"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "\nmcp_servers:\n" +
		"  toolathlon:\n" +
		"    url: \"http://task-sandbox:10086/sse\"\n" +
		"    transport: \"sse\"\n" +
		"    timeout: 300\n" +
		"  docs:\n" +
		"    url: \"https://mcp.example.invalid/mcp\"\n"
	if string(rendered) != want {
		t.Fatalf("rendered:\n%s\nwant:\n%s", rendered, want)
	}
}

func TestRenderMCPServersOmitsBlockWhenEmpty(t *testing.T) {
	rendered, err := renderMCPServers(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered) != 0 {
		t.Fatalf("rendered %q, want nothing", rendered)
	}
}

func TestRenderMCPServersRejectsUnusableServers(t *testing.T) {
	valid := MCPServer{Name: "toolathlon", URL: "http://task-sandbox:10086/sse", Transport: "sse"}
	cases := map[string][]MCPServer{
		"uppercase name":      {{Name: "Toolathlon", URL: valid.URL, Transport: "sse"}},
		"hyphenated name":     {{Name: "tool-athlon", URL: valid.URL, Transport: "sse"}},
		"empty name":          {{Name: "", URL: valid.URL, Transport: "sse"}},
		"relative url":        {{Name: "toolathlon", URL: "task-sandbox:10086/sse", Transport: "sse"}},
		"url with query":      {{Name: "toolathlon", URL: "http://task-sandbox:10086/sse?x=1", Transport: "sse"}},
		"url with credential": {{Name: "toolathlon", URL: "http://user:pw@task-sandbox:10086/sse", Transport: "sse"}},
		"stdio transport":     {{Name: "toolathlon", URL: valid.URL, Transport: "stdio"}},
		"empty transport":     {{Name: "toolathlon", URL: valid.URL}},
		"negative timeout":    {{Name: "toolathlon", URL: valid.URL, Transport: "sse", TimeoutSeconds: -1}},
		"duplicate name":      {valid, valid},
	}
	for name, servers := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := renderMCPServers(servers); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

// A URL carrying a line break cannot restructure the document: url.Parse
// refuses control characters before yamlString would have escaped them, and
// a quote inside an otherwise valid URL is escaped in the scalar.
func TestRenderMCPServersQuotesInjectionAttempts(t *testing.T) {
	if _, err := renderMCPServers([]MCPServer{{Name: "toolathlon", URL: "http://task-sandbox:10086/sse\"\nevil: true", Transport: "sse"}}); err == nil {
		t.Fatal("expected a URL with a line break to be rejected")
	}
	rendered, err := renderMCPServers([]MCPServer{{Name: "toolathlon", URL: "http://task-sandbox:10086/sse%22", Transport: "sse"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `url: "http://task-sandbox:10086/sse%22"`) {
		t.Fatalf("rendered:\n%s", rendered)
	}
}

func TestNewRejectsInvalidMCPServersBeforeStart(t *testing.T) {
	options := Options{
		Image: "docker.io/nousresearch/hermes-agent:v2026.5.29.2", OutputDir: t.TempDir(),
		MCPServers: []MCPServer{{Name: "toolathlon", URL: "not a url", Transport: "sse"}},
	}
	if _, err := New(options); err == nil {
		t.Fatal("expected New to reject an unusable MCP server")
	}
}
