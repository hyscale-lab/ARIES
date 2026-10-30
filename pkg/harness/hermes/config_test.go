package hermes

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"go.yaml.in/yaml/v3"
)

func validModel() core.ModelConfig {
	return core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"}
}

func validEndpoint() core.ToolEndpoint {
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.17.0.1:41234", Username: "aries",
	}
}

func TestRenderTerminalNamesTheSandboxWorkdir(t *testing.T) {
	rendered, err := renderTerminal("ssh", "/workspace/dumps/workspace", 180)
	if err != nil {
		t.Fatal(err)
	}
	terminal := decodeConfigYAML(t, rendered)["terminal"]
	want := map[string]any{"backend": "ssh", "cwd": "/workspace/dumps/workspace", "timeout": 180}
	if !reflect.DeepEqual(terminal, want) {
		t.Fatalf("terminal = %#v, want %#v", terminal, want)
	}
	for _, workdir := range []string{"", "relative", "/app/", "/a b", "/a/../b", "/a;b"} {
		if _, err := renderTerminal("ssh", workdir, 180); err == nil {
			t.Fatalf("accepted workdir %q", workdir)
		}
	}
	if _, err := renderTerminal("ssh", "/app", 0); err == nil {
		t.Fatal("accepted a zero terminal timeout")
	}
}

// The gRPC endpoint selects the ARIES plugin backend in config.yaml, which the
// Gateway treats as authoritative.
func TestRenderTerminalSelectsTheAriesBackend(t *testing.T) {
	rendered, err := renderTerminal("aries", "/app", 180)
	if err != nil {
		t.Fatal(err)
	}
	terminal := decodeConfigYAML(t, rendered)["terminal"]
	want := map[string]any{"backend": "aries", "cwd": "/app", "timeout": 180}
	if !reflect.DeepEqual(terminal, want) {
		t.Fatalf("terminal = %#v, want %#v", terminal, want)
	}
}

// The credential must reach the container as a ${NAME} reference that Hermes
// expands at run time, never as a value written into the rendered config.
func TestRenderConfigReferencesCredentialByName(t *testing.T) {
	rendered, err := renderConfig(validModel(), baseSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := decodeConfigYAML(t, rendered)
	model := config["model"].(map[string]any)
	if model["api_key"] != "${DEEPSEEK_API_KEY}" {
		t.Fatalf("credential reference = %#v", model)
	}
	for _, forbidden := range []string{"sk-", "terminal:", "backend:"} {
		if strings.Contains(string(rendered), forbidden) {
			t.Fatalf("config unexpectedly contains %q:\n%s", forbidden, rendered)
		}
	}
	if config["agent"].(map[string]any)["max_turns"] != 90 || !slices.Contains(config["platform_toolsets"].(map[string]any)["api_server"].([]any), any("terminal")) {
		t.Fatalf("missing agent or toolset settings: %#v", config)
	}
}

func TestRenderConfigNormalizesSGLangAndRejectsBadInput(t *testing.T) {
	model := validModel()
	model.Provider = "sglang"
	model.BaseURL = "http://host:30000/v1/"
	rendered, err := renderConfig(model, renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil)

	if err != nil {
		t.Fatal(err)
	}
	if decodeConfigYAML(t, rendered)["model"].(map[string]any)["base_url"] != "http://host:30000/v1" {
		t.Fatalf("SGLang base URL was not normalized:\n%s", rendered)
	}
	bad := map[string]func(*core.ModelConfig){
		"provider":  func(m *core.ModelConfig) { m.Provider = "anthropic" },
		"base url":  func(m *core.ModelConfig) { m.BaseURL = "ftp://host" },
		"model id":  func(m *core.ModelConfig) { m.Model = " " },
		"key env":   func(m *core.ModelConfig) { m.APIKeyEnv = "1BAD" },
		"url query": func(m *core.ModelConfig) { m.BaseURL = "https://api.deepseek.com?x=1" },
	}
	for name, mutate := range bad {
		model := validModel()
		mutate(&model)
		if _, err := renderConfig(model, renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil); err == nil {
			t.Fatalf("%s: invalid model was accepted", name)
		}
	}
	if _, err := renderConfig(validModel(), renderSettings{maxTurns: 0, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil); err == nil {

		t.Fatal("non-positive max turns was accepted")
	}
}

// A value that could terminate its own YAML scalar must be escaped, not emitted.
func TestRenderConfigQuotesInjectionAttempts(t *testing.T) {
	model := validModel()
	model.Model = `x" \nevil: true`
	rendered, err := renderConfig(model, baseSettings(), nil)
	if err != nil {
		t.Fatal(err)
	}
	config := decodeConfigYAML(t, rendered)
	nativeModel := config["model"].(map[string]any)
	if nativeModel["default"] != model.Model {
		t.Fatalf("model ID changed: %#v", nativeModel)
	}
	if _, exists := nativeModel["evil"]; exists {
		t.Fatal("model ID injected a field")
	}
	if _, exists := config["evil"]; exists {
		t.Fatal("model ID injected a section")
	}
}

func TestRenderConfigAddsVoiceSTTProvider(t *testing.T) {
	voiceSTT := VoiceSTTOptions{Provider: "openai", Model: "gpt-4o-mini-transcribe", Language: "en"}
	rendered, err := renderConfig(validModel(), baseSettings(), &voiceSTT)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"enabled": true, "provider": "openai",
		"openai": map[string]any{"model": "gpt-4o-mini-transcribe"},
		"local":  map[string]any{"model": "gpt-4o-mini-transcribe", "language": "en"},
	}
	if got := decodeConfigYAML(t, rendered)["stt"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("stt = %#v, want %#v", got, want)
	}
}

// Opaque JSON strings must survive the YAML boundary without restructuring it.
func TestRenderConfigPreservesControlCharactersInExtraBody(t *testing.T) {
	for _, value := range []string{"a\x00b", "a\x07b", "a\x0bb", "a\x1bb", "a\x7fb", "a\u0085b", "a\u2028b", "a\u2029b", "a\n\r\tb", "deepseek-v4/flash_1"} {
		settings := baseSettings()
		settings.extraBody, _ = json.Marshal(map[string]string{"value": value})
		config := decodeConfigYAML(t, []byte(mustRender(t, vllmModel(), settings)))
		body := config["custom_providers"].([]any)[0].(map[string]any)["extra_body"].(map[string]any)
		if body["value"] != value {
			t.Fatalf("JSON string %q changed to %#v", value, body["value"])
		}
	}
}

// The readiness probe cannot distinguish a malformed config from a slow start,
// so a control byte must be refused where the cause is still visible.
func TestValidateModelRejectsControlCharactersInModelID(t *testing.T) {
	for _, value := range []string{"a\x00b", "a\x07b", "a\x1bb", "a\x7fb", "a\u0085b"} {
		model := validModel()
		model.Model = value
		if err := validateModel(model); err == nil {
			t.Fatalf("model ID %q was accepted", value)
		}
	}
}

// Hermes selects its SSH backend purely from the environment, so this is the
// contract that replaces Agent_Bench's exec-bridge patch.
func TestContainerEnvironmentSelectsNativeSSHBackend(t *testing.T) {
	environment, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "http://search.example:8123", "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HERMES_HOME":                                stateContainerPath,
		"TERMINAL_ENV":                               "ssh",
		"API_SERVER_HOST":                            "0.0.0.0",
		"API_SERVER_PORT":                            "8642",
		"HERMES_YOLO_MODE":                           "true",
		"TERMINAL_SSH_HOST":                          "172.17.0.1",
		"TERMINAL_SSH_PORT":                          "41234",
		"TERMINAL_SSH_USER":                          "aries",
		"TERMINAL_CWD":                               "/aries/workspace",
		"TERMINAL_TIMEOUT":                           "180",
		"ARIES_RUN_ID":                               "run-1",
		"ARIES_TASK_ID":                              "fix-git",
		"HERMES_WRITE_SAFE_ROOT":                     "",
		"HERMES_OTEL_DASHBOARD_LIVE":                 "true",
		"HERMES_OTEL_DASHBOARD_LIVE_MAX_SPANS":       "10000",
		"HERMES_OTEL_DASHBOARD_LIVE_RETENTION_HOURS": "0",
		"HERMES_OTEL_CONTENT_CAPTURE":                "off",
	}
	got := map[string]string{}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	if len(got) != len(want) {
		t.Fatalf("environment=%v", environment)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s=%q, want %q", name, got[name], value)
		}
	}
}

func TestRenderConfigAlwaysEnablesOTelPlugin(t *testing.T) {
	for _, subagents := range []bool{false, true} {
		settings := baseSettings()
		settings.subagentsEnabled = subagents
		config := decodeConfigYAML(t, []byte(mustRender(t, vllmModel(), settings)))
		if got := config["plugins"].(map[string]any)["enabled"]; !reflect.DeepEqual(got, []any{"hermes_otel"}) {
			t.Fatalf("enabled plugins = %#v", got)
		}
	}
}

func TestContainerEnvironmentRejectsUnusableEndpoints(t *testing.T) {
	cases := map[string]func(*core.ToolEndpoint){
		"DNS":            func(endpoint *core.ToolEndpoint) { endpoint.Address = "bad..test:2222" },
		"IPv6":           func(endpoint *core.ToolEndpoint) { endpoint.Address = "[::1]:2222" },
		"wildcard":       func(endpoint *core.ToolEndpoint) { endpoint.Address = "0.0.0.0:2222" },
		"multicast":      func(endpoint *core.ToolEndpoint) { endpoint.Address = "224.0.0.1:2222" },
		"zero port":      func(endpoint *core.ToolEndpoint) { endpoint.Address = "127.0.0.1:0" },
		"overflow port":  func(endpoint *core.ToolEndpoint) { endpoint.Address = "127.0.0.1:65536" },
		"protocol":       func(e *core.ToolEndpoint) { e.Protocol = "http" },
		"username":       func(e *core.ToolEndpoint) { e.Username = "root" },
		"client command": func(e *core.ToolEndpoint) { e.ClientCommand = "/opt/aries/bin/aries-ssh-client" },
		"client source":  func(e *core.ToolEndpoint) { e.ClientSourceFile = "/tmp/aries-ssh-client" },
		"address":        func(e *core.ToolEndpoint) { e.Address = "no-port" },
		"port":           func(e *core.ToolEndpoint) { e.Address = "10.0.0.1:ssh" },
	}
	for name, mutate := range cases {
		endpoint := validEndpoint()
		mutate(&endpoint)
		if _, err := containerEnvironment(endpoint, "/aries/workspace", 180, false, "http://search.example:8123", "run-1", "fix-git"); err == nil {
			t.Fatalf("%s: invalid endpoint was accepted", name)
		}
	}
	for _, workdir := range []string{"", "relative", "/has space", "/trailing/", "/a/../b"} {
		if _, err := containerEnvironment(validEndpoint(), workdir, 180, false, "http://search.example:8123", "run-1", "fix-git"); err == nil {
			t.Fatalf("workdir %q was accepted", workdir)
		}
	}
	if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 0, false, "http://search.example:8123", "run-1", "fix-git"); err == nil {
		t.Fatal("non-positive terminal timeout was accepted")
	}
}

// Disabled web search must leave today's toolset list and environment
// unchanged — a regression guard for callers that never opt in.
func TestRenderConfigOmitsWebToolsetWhenDisabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil)

	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "- web") || strings.Contains(text, "\nweb:") {
		t.Fatalf("config unexpectedly enables the web toolset:\n%s", text)
	}
}

func TestRenderConfigAddsWebToolsetWhenEnabled(t *testing.T) {
	settings := baseSettings()
	settings.webSearchEnabled = true
	config := decodeConfigYAML(t, []byte(mustRender(t, validModel(), settings)))
	if !slices.Contains(config["platform_toolsets"].(map[string]any)["api_server"].([]any), any("web")) || config["web"].(map[string]any)["search_backend"] != "searxng" {
		t.Fatalf("web settings = %#v", config)
	}
}

// extract_backend must only appear when a Tavily key is actually staged —
// otherwise a web_extract call would hit Hermes with no explicit backend
// rather than the clear "search-only backend" error SearXNG-only gives.
func TestRenderConfigOmitsExtractBackendWithoutExtractKey(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: true, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil)

	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "extract_backend") {
		t.Fatalf("config unexpectedly sets extract_backend:\n%s", rendered)
	}
}

func TestRenderConfigAddsExtractBackendWhenEnabled(t *testing.T) {
	settings := baseSettings()
	settings.webSearchEnabled, settings.extractEnabled = true, true
	config := decodeConfigYAML(t, []byte(mustRender(t, validModel(), settings)))
	if !slices.Contains(config["platform_toolsets"].(map[string]any)["api_server"].([]any), any("web")) || !reflect.DeepEqual(config["web"], map[string]any{"search_backend": "searxng", "extract_backend": "tavily"}) {
		t.Fatalf("web settings = %#v", config)
	}
}

// extract_backend must never be rendered when web search itself is off, even
// if a caller passes extractEnabled=true by mistake.
func TestRenderConfigOmitsExtractBackendWhenWebSearchDisabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: true, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil)

	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "- web") || strings.Contains(text, "\nweb:") || strings.Contains(text, "extract_backend") {
		t.Fatalf("config unexpectedly enables web/extract while web search is disabled:\n%s", text)
	}
}

func TestRenderConfigDisablesDelegationToolsetWhenSubagentsDisabled(t *testing.T) {
	config := decodeConfigYAML(t, []byte(mustRender(t, validModel(), renderSettings{maxTurns: 90})))
	if got := config["agent"].(map[string]any)["disabled_toolsets"]; !reflect.DeepEqual(got, []any{"delegation"}) {
		t.Fatalf("disabled toolsets = %#v", got)
	}
}

func TestRenderConfigOmitsDisabledToolsetsWhenSubagentsEnabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil)

	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "disabled_toolsets") {
		t.Fatalf("config unexpectedly disables toolsets while subagents are enabled:\n%s", text)
	}
}

func TestRenderConfigSetsMaxConcurrentChildrenWhenLimited(t *testing.T) {
	settings := baseSettings()
	settings.maxConcurrentSubagents = 2
	config := decodeConfigYAML(t, []byte(mustRender(t, validModel(), settings)))
	if got := config["delegation"]; !reflect.DeepEqual(got, map[string]any{"max_concurrent_children": 2}) {
		t.Fatalf("delegation = %#v", got)
	}
}

func TestRenderConfigOmitsDelegationBlockWhenNoLimitSet(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}, nil)

	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "\ndelegation:\n") {
		t.Fatalf("config unexpectedly sets a delegation block with no limit configured:\n%s", text)
	}
}

func TestRenderConfigIgnoresMaxConcurrentChildrenWhenSubagentsDisabled(t *testing.T) {

	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: false, maxConcurrentSubagents: 2}, nil)

	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "\ndelegation:\n") {
		t.Fatalf("config unexpectedly sets a delegation block while subagents are disabled:\n%s", text)
	}
}

func TestContainerEnvironmentSetsSearXNGURLWhenWebSearchEnabled(t *testing.T) {
	disabled, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "http://search.example:8123", "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range disabled {
		if strings.HasPrefix(entry, "SEARXNG_URL=") {
			t.Fatalf("SEARXNG_URL set despite web search being disabled: %v", disabled)
		}
	}
	enabled, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, true, "http://search.example:8123", "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	want := "SEARXNG_URL=" + "http://search.example:8123"
	found := false
	for _, entry := range enabled {
		if entry == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("environment=%v, want entry %q", enabled, want)
	}
}

// Gateway launches without CLI-only toolset flags; config selects its tools.
func TestGatewayLauncherExportsKeyAndAvoidsToolsetsFlag(t *testing.T) {
	script := string(gatewayLauncherScript("DEEPSEEK_API_KEY", false))
	for _, want := range []string{
		"DEEPSEEK_API_KEY=\"$(cat " + modelKeyPath + ")\"",
		"export DEEPSEEK_API_KEY",
		`exec hermes gateway run --no-supervise --external-supervisor`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("launcher is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "--toolsets") {
		t.Fatalf("launcher passes --toolsets:\n%s", script)
	}
	if strings.Contains(script, tavilyAPIKeyEnv) || strings.Contains(script, extractKeyPath) {
		t.Fatalf("launcher exports the extract key despite extract being disabled:\n%s", script)
	}
}

// The SSH launcher must not change with the gRPC support.
func TestGatewayLauncherForSSHIsUnchanged(t *testing.T) {
	want := `#!/bin/sh
set -eu
if [ ! -f /run/aries/hermes/model.key ]; then
  echo "ARIES: Hermes model key is missing" >&2
  exit 1
fi
DEEPSEEK_API_KEY="$(cat /run/aries/hermes/model.key)"
export DEEPSEEK_API_KEY
API_SERVER_KEY="$(cat /run/aries/gateway.key)"
export API_SERVER_KEY
exec hermes gateway run --no-supervise --external-supervisor
`
	if script := string(gatewayLauncherScript("DEEPSEEK_API_KEY", false)); script != want {
		t.Fatalf("SSH launcher changed:\n%s", script)
	}
}

func TestGatewayLauncherExportsExtractKeyWhenEnabled(t *testing.T) {
	script := string(gatewayLauncherScript("DEEPSEEK_API_KEY", true))
	for _, want := range []string{
		"DEEPSEEK_API_KEY=\"$(cat " + modelKeyPath + ")\"",
		"export DEEPSEEK_API_KEY",
		tavilyAPIKeyEnv + "=\"$(cat " + extractKeyPath + ")\"",
		"export " + tavilyAPIKeyEnv,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("launcher is missing %q:\n%s", want, script)
		}
	}
}

// OpenAI-compatible backends use Hermes's "custom" provider; DeepSeek remains native.
func TestRenderConfigMapsOpenAICompatibleBackendsToCustomProvider(t *testing.T) {
	for _, provider := range []string{"sglang", "openai"} {
		model := vllmModel()
		model.Provider = provider
		config := decodeConfigYAML(t, []byte(mustRender(t, model, baseSettings())))
		if config["model"].(map[string]any)["provider"] != "custom" || hermesProvider(provider) != "custom" {
			t.Fatalf("%s provider = %#v", provider, config["model"])
		}
	}
	config := decodeConfigYAML(t, []byte(mustRender(t, validModel(), baseSettings())))
	if config["model"].(map[string]any)["provider"] != "deepseek" || hermesProvider("deepseek") != "deepseek" {
		t.Fatal("DeepSeek provider was rewritten")
	}
}

func TestRenderConfig_MCPServers(t *testing.T) {
	servers := []core.MCPServerConfig{
		{Name: "filesystem", Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/workspace"}, Env: map[string]string{"DEBUG": "1"}, SecretEnv: map[string]string{"API_KEY": "TOOLATHLON_API_KEY"}},
		{Name: "remote-sse", URL: "https://mcp.example.com/sse"},
	}
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 10, mcpServers: servers}, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := decodeConfigYAML(t, rendered)
	if _, exists := config["custom_tools"]; exists {
		t.Fatal("rendered invalid custom_tools field")
	}
	want := map[string]any{
		"filesystem": map[string]any{"command": "npx", "args": []any{"-y", "@modelcontextprotocol/server-filesystem", "/workspace"}, "env": map[string]any{"DEBUG": "1", "API_KEY": "${TOOLATHLON_API_KEY}"}},
		"remote-sse": map[string]any{"url": "https://mcp.example.com/sse"},
	}
	if got := config["mcp_servers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP servers = %#v, want %#v", got, want)
	}
}

func TestGatewayLauncherScriptExportsMCPHostVariables(t *testing.T) {
	script := string(gatewayLauncherScript("DEEPSEEK_API_KEY", false, "TOOLATHLON_API_KEY", "OTHER_KEY"))
	for _, required := range []string{
		"TOOLATHLON_API_KEY=\"$(cat /run/aries/hermes/mcp_TOOLATHLON_API_KEY.key)\"",
		"export TOOLATHLON_API_KEY",
		"OTHER_KEY=\"$(cat /run/aries/hermes/mcp_OTHER_KEY.key)\"",
		"export OTHER_KEY",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("gatewayLauncherScript missing MCP export %q: %s", required, script)
		}
	}
}

func TestSearchRequiresResolvedEndpoint(t *testing.T) {
	if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, true, "", "run-1", "fix-git"); err == nil {
		t.Fatal("accepted search without a declared endpoint")
	}
	if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "", "run-1", "fix-git"); err != nil {
		t.Fatal(err)
	}
}

func TestRenderConfig_MCPServerTransportAndTimeout(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 10, mcpServers: []core.MCPServerConfig{
		{Name: "gateway", URL: "http://task-sandbox:10086/sse", Transport: "sse", TimeoutSeconds: 1200},
		{Name: "streaming", URL: "https://mcp.example.com/mcp", Transport: "streamable-http"},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"gateway":   map[string]any{"url": "http://task-sandbox:10086/sse", "transport": "sse", "timeout": 1200},
		"streaming": map[string]any{"url": "https://mcp.example.com/mcp"},
	}
	if got := decodeConfigYAML(t, rendered)["mcp_servers"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("MCP servers = %#v, want %#v", got, want)
	}
}

// A task without sandbox tools leaves terminal, file and code_execution out of
// the api_server platform toolsets and names them in the agent's
// disabled_toolsets, keeps web search when the profile enables it, and keeps
// the MCP servers; the default is unchanged.
func TestRenderConfig_NoSandboxToolsLeavesOnlyMCPAndWeb(t *testing.T) {
	servers := []core.MCPServerConfig{{Name: "toolathlon", URL: "http://task-sandbox:10086/sse", Transport: "sse"}}
	for _, test := range []struct {
		withheld, web      bool
		toolsets, disabled []any
	}{
		{true, false, []any{}, []any{"delegation", "terminal", "file", "code_execution"}},
		{true, true, []any{"web"}, []any{"delegation", "terminal", "file", "code_execution"}},
		{false, false, []any{"terminal", "file", "code_execution"}, []any{"delegation"}},
	} {
		rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 10, webSearchEnabled: test.web, mcpServers: servers, noSandboxTools: test.withheld}, nil)
		if err != nil {
			t.Fatal(err)
		}
		config := decodeConfigYAML(t, rendered)
		tools := config["platform_toolsets"].(map[string]any)["api_server"]
		disabled := config["agent"].(map[string]any)["disabled_toolsets"]
		if !reflect.DeepEqual(tools, test.toolsets) || !reflect.DeepEqual(disabled, test.disabled) {
			t.Fatalf("tools/denylist = %#v / %#v", tools, disabled)
		}
		if _, ok := config["mcp_servers"].(map[string]any)["toolathlon"]; !ok {
			t.Fatal("missing task MCP server")
		}
		if _, ok := config["web"]; ok != test.web {
			t.Fatalf("web selection = %#v", config["web"])
		}
	}
}

func validGRPCEndpoint() core.ToolEndpoint {
	return core.ToolEndpoint{
		Protocol: "grpc", Address: "172.17.0.1:41234", Username: "aries",
	}
}

// The gRPC endpoint selects the ARIES plugin backend without an SSH target.
// The plugin reads its target from its config entry.
func TestContainerEnvironmentSelectsTheAriesBackendUnderGRPC(t *testing.T) {
	environment, err := containerEnvironment(validGRPCEndpoint(), "/aries/workspace", 180, false, "", "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HERMES_HOME":                                stateContainerPath,
		"TERMINAL_ENV":                               "aries",
		"API_SERVER_HOST":                            "0.0.0.0",
		"API_SERVER_PORT":                            "8642",
		"HERMES_YOLO_MODE":                           "true",
		"TERMINAL_CWD":                               "/aries/workspace",
		"TERMINAL_TIMEOUT":                           "180",
		"ARIES_RUN_ID":                               "run-1",
		"ARIES_TASK_ID":                              "fix-git",
		"HERMES_WRITE_SAFE_ROOT":                     "",
		"HERMES_OTEL_DASHBOARD_LIVE":                 "true",
		"HERMES_OTEL_DASHBOARD_LIVE_MAX_SPANS":       "10000",
		"HERMES_OTEL_DASHBOARD_LIVE_RETENTION_HOURS": "0",
		"HERMES_OTEL_CONTENT_CAPTURE":                "off",
	}
	got := map[string]string{}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		got[name] = value
	}
	if len(got) != len(want) {
		t.Fatalf("environment=%v", environment)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("%s=%q, want %q", name, got[name], value)
		}
	}
}

func TestContainerEnvironmentRejectsUnusableGRPCEndpoints(t *testing.T) {
	cases := map[string]func(*core.ToolEndpoint){
		"staged client command": func(e *core.ToolEndpoint) { e.ClientCommand = "/usr/bin/ssh" },
		"staged client source":  func(e *core.ToolEndpoint) { e.ClientSourceFile = "/tmp/client" },
		"username":              func(e *core.ToolEndpoint) { e.Username = "root" },
		"unknown protocol":      func(e *core.ToolEndpoint) { e.Protocol = "quic" },
		"address":               func(e *core.ToolEndpoint) { e.Address = "no-port" },
	}
	for name, mutate := range cases {
		endpoint := validGRPCEndpoint()
		mutate(&endpoint)
		if _, err := containerEnvironment(endpoint, "/aries/workspace", 180, false, "", "run-1", "fix-git"); err == nil {
			t.Fatalf("%s: invalid endpoint was accepted", name)
		}
	}
}

// The gRPC launcher uses the plugin baked into the image and stages no client.
// The SSH launcher remains unchanged.
func TestGatewayLauncherDoesNotCheckAStagedClient(t *testing.T) {
	launcher := string(gatewayLauncherScript("DEEPSEEK_API_KEY", false))
	for _, unwanted := range []string{"seam", "python3", "plugins/aries", "gRPC client"} {
		if strings.Contains(launcher, unwanted) {
			t.Fatalf("gRPC launcher contains %q:\n%s", unwanted, launcher)
		}
	}
}

// The gRPC route lists the plugin in plugins.enabled. Hermes also auto-loads
// it on the SSH route, as a bundled backend, but the backend stays ssh there
// (TestARIESHermesImageKeepsTheSSHRouteOnTheShell).
func TestRenderConfigEnablesThePluginOnlyForTheAriesBackend(t *testing.T) {
	settings := baseSettings()
	settings.grpcTarget = validGRPCEndpoint().Address
	config := decodeConfigYAML(t, []byte(mustRender(t, validModel(), settings)))
	plugins := config["plugins"].(map[string]any)
	if got := plugins["enabled"]; !reflect.DeepEqual(got, []any{otelPluginName, "aries"}) {
		t.Fatalf("enabled plugins = %#v", got)
	}
	want := map[string]any{"aries": map[string]any{"settings": map[string]any{"grpc_target": validGRPCEndpoint().Address}}}
	if got := plugins["entries"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("plugin entries = %#v, want %#v", got, want)
	}
	sshConfig := decodeConfigYAML(t, []byte(mustRender(t, validModel(), baseSettings())))
	if entries, ok := sshConfig["plugins"].(map[string]any)["entries"]; ok {
		t.Fatalf("SSH plugin has entries = %#v", entries)
	}
}

func decodeConfigYAML(t *testing.T, rendered []byte) map[string]any {
	t.Helper()
	var config map[string]any
	if err := yaml.Unmarshal(rendered, &config); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, rendered)
	}
	return config
}
