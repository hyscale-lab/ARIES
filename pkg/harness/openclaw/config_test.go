package openclaw

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func testEndpoint() core.ToolEndpoint {
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries", Network: "aries-net-test",
		ClientCommand: "/opt/aries/bin/aries-ssh", ClientSourceFile: "/host/aries-ssh",
		IdentityFile: "/run/aries/ssh/id_ed25519", IdentitySourceFile: "/host/id_ed25519",
		KnownHostsFile: "/run/aries/ssh/known_hosts", KnownHostsSourceFile: "/host/known_hosts",
	}
}

func testModel() core.ModelConfig {
	return core.ModelConfig{Provider: "deepseek", BaseURL: "http://fake-model:8080/v1", Model: "deterministic-model", APIKeyEnv: "ARIES_FAKE_API_KEY"}
}

func TestRenderConfigLocksProviderSharedSSHAndPlaceholder(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("dummy-secret")) || !bytes.Contains(content, []byte(`"apiKey": "${ARIES_FAKE_API_KEY}"`)) {
		t.Fatalf("API key placeholder = %s", content)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	provider := configuration.Models.Providers["aries"]
	sandbox := configuration.Agents.Defaults.Sandbox
	if configuration.Gateway.Mode != "local" || configuration.Models.Mode != "merge" || provider.API != "openai-completions" || provider.BaseURL != testModel().BaseURL {
		t.Fatalf("provider config = %#v", configuration)
	}
	if configuration.Gateway.Auth.Mode != "token" || configuration.Gateway.Auth.Token != "${OPENCLAW_GATEWAY_TOKEN}" || configuration.Gateway.Remote.Token != "${OPENCLAW_GATEWAY_TOKEN}" {
		t.Fatalf("gateway config = %#v", configuration.Gateway)
	}
	if configuration.Agents.Defaults.Model.Primary != "aries/deterministic-model" || sandbox.Mode != "all" || sandbox.Scope != "shared" || sandbox.Backend != "ssh" || sandbox.WorkspaceAccess != "rw" {
		t.Fatalf("agent config = %#v", configuration.Agents.Defaults)
	}
	if sandbox.SSH.Target != "aries@172.22.0.1:39425" || sandbox.SSH.Command != "/opt/aries/bin/aries-ssh" || sandbox.SSH.WorkspaceRoot != workspaceRoot || !sandbox.SSH.StrictHostKeyChecking || sandbox.SSH.UpdateHostKeys {
		t.Fatalf("SSH config = %#v", sandbox.SSH)
	}
	if got := strings.Join(configuration.Tools.Deny, ","); got != "read,write,edit,apply_patch,sessions_spawn,sessions_yield" {
		t.Fatalf("tool deny list = %q", got)
	}
}

func TestRenderConfigSelectsSGLangProviderWithoutSerializingKey(t *testing.T) {
	model := testModel()
	model.Provider = "sglang"
	model.APIKeyEnv = "SGLANG_API_KEY"
	content, err := renderConfig(model, testEndpoint(), false, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	provider, ok := configuration.Models.Providers["sglang"]
	if !ok || len(configuration.Models.Providers) != 1 || provider.API != "openai-completions" || provider.APIKey != "${SGLANG_API_KEY}" || configuration.Agents.Defaults.Model.Primary != "sglang/deterministic-model" {
		t.Fatalf("configuration = %#v", configuration)
	}
	if bytes.Contains(content, []byte("dummy-local-key")) {
		t.Fatalf("serialized key: %s", content)
	}
}

func TestRenderConfigNormalizesAndStrictlyValidatesSGLangBaseURL(t *testing.T) {
	model := testModel()
	model.Provider = "sglang"
	model.BaseURL += "/"
	content, err := renderConfig(model, testEndpoint(), false, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if got := configuration.Models.Providers["sglang"].BaseURL; got != testModel().BaseURL {
		t.Fatalf("normalized base URL = %q", got)
	}
	for _, invalid := range []string{"http://host/v1/v1", "http://host/v1?", "http://host/v%31"} {
		model.BaseURL = invalid
		if _, err := renderConfig(model, testEndpoint(), false, false, false, 0, nil); err == nil {
			t.Fatalf("accepted SGLang base URL %q", invalid)
		}
	}
}

func TestRenderConfigRejectsInvalidInputs(t *testing.T) {
	for name, mutate := range map[string]func(*core.ModelConfig, *core.ToolEndpoint){
		"provider": func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.Provider = "other" },
		"base URL": func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.BaseURL = "file:///tmp/model" },
		"model":    func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.Model = "\n" },
		"key env":  func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.APIKeyEnv = "bad-name" },
		"protocol": func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.Protocol = "http" },
		"network":  func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.Network = "" },
		"address":  func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.Address = "task-sandbox:2222" },
		"identity": func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.IdentityFile = "/wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			model, endpoint := testModel(), testEndpoint()
			mutate(&model, &endpoint)
			if _, err := renderConfig(model, endpoint, false, false, false, 0, nil); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}

func TestRenderConfigAcceptsLowercaseEnvironmentName(t *testing.T) {
	model := testModel()
	model.APIKeyEnv = "aries_fake_api_key"
	if _, err := renderConfig(model, testEndpoint(), false, false, false, 0, nil); err != nil {
		t.Fatalf("renderConfig() rejected a valid environment name: %v", err)
	}
}

func TestRenderConfigOmitsWebSearchWhenDisabled(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Note: "sandbox" is not checked here as a raw substring — it's also the
	// key for the unrelated, always-present agents.defaults.sandbox (SSH)
	// block. tools.Sandbox is checked below via the parsed struct instead.
	if bytes.Contains(content, []byte(`"web"`)) || bytes.Contains(content, []byte(`"plugins"`)) {
		t.Fatalf("disabled web search leaked into rendered config: %s", content)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Tools.Web != nil || configuration.Plugins != nil || configuration.Tools.Sandbox != nil {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestRenderConfigEnablesSearXNGWebSearch(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), true, false, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Tools.Web == nil || configuration.Tools.Web.Search == nil || configuration.Tools.Web.Search.Provider != "searxng" {
		t.Fatalf("tools.web.search = %#v", configuration.Tools.Web)
	}
	if configuration.Plugins == nil {
		t.Fatal("plugins block missing")
	}
	entry, ok := configuration.Plugins.Entries["searxng"]
	if !ok || entry.Config.WebSearch.BaseURL != searxngBaseURL {
		t.Fatalf("plugins.entries.searxng = %#v", configuration.Plugins.Entries)
	}
	if configuration.Tools.Sandbox == nil {
		t.Fatal("tools.sandbox gate missing: web_search/web_fetch would be invisible to a sandboxed session")
	}
	alsoAllow := configuration.Tools.Sandbox.Tools.AlsoAllow
	if len(alsoAllow) != 2 || alsoAllow[0] != "web_search" || alsoAllow[1] != "web_fetch" {
		t.Fatalf("tools.sandbox.tools.alsoAllow = %#v", alsoAllow)
	}
}

func TestRenderConfigEnablesTavilyExtractAlongsideSearXNGSearch(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), true, true, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	// Search stays SearXNG: extract must not change tools.web.search.provider.
	if configuration.Tools.Web == nil || configuration.Tools.Web.Search == nil || configuration.Tools.Web.Search.Provider != "searxng" {
		t.Fatalf("tools.web.search = %#v", configuration.Tools.Web)
	}
	searxngEntry, ok := configuration.Plugins.Entries["searxng"]
	if !ok || searxngEntry.Config == nil || searxngEntry.Config.WebSearch.BaseURL != searxngBaseURL {
		t.Fatalf("plugins.entries.searxng = %#v", configuration.Plugins.Entries)
	}
	tavilyEntry, ok := configuration.Plugins.Entries["tavily"]
	if !ok || !tavilyEntry.Enabled || tavilyEntry.Config != nil {
		t.Fatalf("plugins.entries.tavily = %#v", configuration.Plugins.Entries)
	}
	alsoAllow := configuration.Tools.Sandbox.Tools.AlsoAllow
	if len(alsoAllow) != 3 || alsoAllow[2] != "tavily_extract" {
		t.Fatalf("tools.sandbox.tools.alsoAllow = %#v, want tavily_extract appended", alsoAllow)
	}
	for _, disallowed := range alsoAllow {
		if disallowed == "tavily_search" {
			t.Fatalf("tavily_search must stay invisible: alsoAllow = %#v", alsoAllow)
		}
	}
}

func TestRenderConfigIgnoresExtractWhenWebSearchDisabled(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, true, false, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte(`"web"`)) || bytes.Contains(content, []byte(`"plugins"`)) {
		t.Fatalf("extract leaked into rendered config despite web search being disabled: %s", content)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Tools.Web != nil || configuration.Plugins != nil || configuration.Tools.Sandbox != nil {
		t.Fatalf("configuration = %#v", configuration)
	}
}

func TestRenderConfigAllowsSubagentsWhenEnabled(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, false, true, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(configuration.Tools.Deny, ","); got != "read,write,edit,apply_patch" {
		t.Fatalf("tool deny list = %q, want sessions_spawn/sessions_yield omitted", got)
	}
}

func TestRenderConfigSetsMaxConcurrentSubagentsWhenEnabled(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, false, true, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Agents.Defaults.Subagents == nil || configuration.Agents.Defaults.Subagents.MaxConcurrent != 2 {
		t.Fatalf("agents.defaults.subagents = %#v, want maxConcurrent 2", configuration.Agents.Defaults.Subagents)
	}
}

func TestRenderConfigOmitsSubagentsBlockWhenNoLimitSet(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, false, true, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Agents.Defaults.Subagents != nil {
		t.Fatalf("agents.defaults.subagents = %#v, want omitted", configuration.Agents.Defaults.Subagents)
	}
}

func TestRenderConfigIgnoresMaxConcurrentWhenSubagentsDisabled(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint(), false, false, false, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Agents.Defaults.Subagents != nil {
		t.Fatalf("agents.defaults.subagents = %#v, want omitted when subagents disabled", configuration.Agents.Defaults.Subagents)
	}
}

func TestLauncherUsesFileSecretAndDirectExec(t *testing.T) {
	script := string(launcherScript("ARIES_FAKE_API_KEY", "OPENAI_API_KEY", false))
	for _, required := range []string{"model_key=$(cat /run/aries/model.key)", "gateway_key=$(cat /run/aries/gateway.key)", "export ARIES_FAKE_API_KEY=\"$model_key\"", "export OPENCLAW_GATEWAY_TOKEN=\"$gateway_key\"", "exec \"$@\""} {
		if !strings.Contains(script, required) {
			t.Fatalf("launcher missing %q: %s", required, script)
		}
	}
	for _, required := range []string{"realtime_key=$(cat /run/aries/realtime.key)", "export OPENAI_API_KEY=\"$realtime_key\"", "unset realtime_key"} {
		if !strings.Contains(script, required) {
			t.Fatalf("launcher missing realtime export %q: %s", required, script)
		}
	}
	agentScript := string(launcherScript("ARIES_FAKE_API_KEY", "", false))
	if strings.Contains(agentScript, "realtime.key") || strings.Contains(agentScript, "OPENAI_API_KEY") {
		t.Fatalf("agent launcher exports realtime key: %s", agentScript)
	}
}

func TestLauncherExportsTavilyKeyWhenExtractEnabled(t *testing.T) {
	script := string(launcherScript("ARIES_FAKE_API_KEY", "", true))
	for _, required := range []string{"tavily_key=$(cat /run/aries/tavily.key)", "export TAVILY_API_KEY=\"$tavily_key\"", "unset tavily_key"} {
		if !strings.Contains(script, required) {
			t.Fatalf("launcher missing tavily export %q: %s", required, script)
		}
	}
	disabledScript := string(launcherScript("ARIES_FAKE_API_KEY", "", false))
	if strings.Contains(disabledScript, "tavily.key") || strings.Contains(disabledScript, "TAVILY_API_KEY") {
		t.Fatalf("launcher exports tavily key when extract is disabled: %s", disabledScript)
	}
}
