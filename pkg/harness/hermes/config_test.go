package hermes

import (
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func validModel() core.ModelConfig {
	return core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"}
}

func validEndpoint() core.ToolEndpoint {
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.17.0.1:41234", Username: "aries", Network: "aries-net",
		IdentityFile: identityContainerFS, IdentitySourceFile: "/tmp/id_ed25519",
	}
}

// The credential must reach the container as a ${NAME} reference that Hermes
// expands at run time, never as a value written into the rendered config.
func TestRenderConfigReferencesCredentialByName(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if !strings.Contains(text, `api_key: "${DEEPSEEK_API_KEY}"`) {
		t.Fatalf("config does not reference the credential by name:\n%s", text)
	}
	for _, forbidden := range []string{"sk-", "terminal:", "backend:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("config unexpectedly contains %q:\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, "max_turns: 90") || !strings.Contains(text, "- terminal") {
		t.Fatalf("config is missing agent or toolset settings:\n%s", text)
	}
}

func TestRenderConfigNormalizesSGLangAndRejectsBadInput(t *testing.T) {
	model := validModel()
	model.Provider = "sglang"
	model.BaseURL = "http://host:30000/v1/"
	rendered, err := renderConfig(model, renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `base_url: "http://host:30000/v1"`) {
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
		if _, err := renderConfig(model, renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}); err == nil {
			t.Fatalf("%s: invalid model was accepted", name)
		}
	}
	if _, err := renderConfig(validModel(), renderSettings{maxTurns: 0, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0}); err == nil {
		t.Fatal("non-positive max turns was accepted")
	}
}

// A value that could terminate its own YAML scalar must be escaped, not emitted.
func TestRenderConfigQuotesInjectionAttempts(t *testing.T) {
	model := validModel()
	model.Model = `x" \nevil: true`
	rendered, err := renderConfig(model, renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	// The model ID carries a literal backslash-n. It must reach the file as a
	// doubled backslash, not as the `\n` escape a YAML parser would decode
	// into a line break that starts a new key.
	if !strings.Contains(string(rendered), `\\nevil: true`) {
		t.Fatalf("model ID escaped its scalar:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), `\"`) {
		t.Fatalf("model ID quote was not escaped:\n%s", rendered)
	}
}

// A control byte cannot reach yamlString through renderConfig because
// validateModel rejects it first, so the renderer is covered directly: a raw
// control byte in a double-quoted scalar is invalid YAML, and Hermes would fail
// to parse its own config well after the cause.
func TestYAMLStringEscapesControlCharacters(t *testing.T) {
	for _, test := range []struct{ name, value, want string }{
		{"nul", "a\x00b", `"a\x00b"`},
		{"bell", "a\x07b", `"a\x07b"`},
		{"vertical tab", "a\x0bb", `"a\x0Bb"`},
		{"escape", "a\x1bb", `"a\x1Bb"`},
		{"delete", "a\x7fb", `"a\x7Fb"`},
		{"next line", "a\u0085b", `"a\x85b"`},
		{"line separator", "a\u2028b", `"a\Lb"`},
		{"paragraph separator", "a\u2029b", `"a\Pb"`},
		{"short forms retained", "a\n\r\tb", `"a\n\r\tb"`},
		{"printable untouched", "deepseek-v4/flash_1", `"deepseek-v4/flash_1"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := yamlString(test.value); got != test.want {
				t.Fatalf("yamlString(%q) = %s, want %s", test.value, got, test.want)
			}
		})
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
	environment, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HERMES_HOME":            stateContainerPath,
		"TERMINAL_ENV":           "ssh",
		"TERMINAL_SSH_HOST":      "172.17.0.1",
		"TERMINAL_SSH_PORT":      "41234",
		"TERMINAL_SSH_USER":      "aries",
		"TERMINAL_SSH_KEY":       identityContainerFS,
		"TERMINAL_CWD":           "/aries/workspace",
		"TERMINAL_TIMEOUT":       "180",
		"ARIES_RUN_ID":           "run-1",
		"ARIES_TASK_ID":          "fix-git",
		"HERMES_WRITE_SAFE_ROOT": "",
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

func TestContainerEnvironmentRejectsUnusableEndpoints(t *testing.T) {
	cases := map[string]func(*core.ToolEndpoint){
		"protocol":       func(e *core.ToolEndpoint) { e.Protocol = "http" },
		"username":       func(e *core.ToolEndpoint) { e.Username = "root" },
		"network":        func(e *core.ToolEndpoint) { e.Network = "" },
		"identity":       func(e *core.ToolEndpoint) { e.IdentitySourceFile = "" },
		"client command": func(e *core.ToolEndpoint) { e.ClientCommand = "/opt/aries/bin/aries-ssh" },
		"client source":  func(e *core.ToolEndpoint) { e.ClientSourceFile = "/tmp/aries-ssh" },
		"address":        func(e *core.ToolEndpoint) { e.Address = "no-port" },
		"port":           func(e *core.ToolEndpoint) { e.Address = "10.0.0.1:ssh" },
	}
	for name, mutate := range cases {
		endpoint := validEndpoint()
		mutate(&endpoint)
		if _, err := containerEnvironment(endpoint, "/aries/workspace", 180, false, "run-1", "fix-git"); err == nil {
			t.Fatalf("%s: invalid endpoint was accepted", name)
		}
	}
	for _, workdir := range []string{"", "relative", "/has space", "/trailing/", "/a/../b"} {
		if _, err := containerEnvironment(validEndpoint(), workdir, 180, false, "run-1", "fix-git"); err == nil {
			t.Fatalf("workdir %q was accepted", workdir)
		}
	}
	if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 0, false, "run-1", "fix-git"); err == nil {
		t.Fatal("non-positive terminal timeout was accepted")
	}
}

// Disabled web search must leave today's toolset list and environment
// unchanged — a regression guard for callers that never opt in.
func TestRenderConfigOmitsWebToolsetWhenDisabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "- web") || strings.Contains(text, "\nweb:") {
		t.Fatalf("config unexpectedly enables the web toolset:\n%s", text)
	}
}

func TestRenderConfigAddsWebToolsetWhenEnabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: true, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	for _, want := range []string{"    - web\n", "\nweb:\n  search_backend: \"searxng\"\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("config is missing %q:\n%s", want, text)
		}
	}
}

// extract_backend must only appear when a Tavily key is actually staged —
// otherwise a web_extract call would hit Hermes with no explicit backend
// rather than the clear "search-only backend" error SearXNG-only gives.
func TestRenderConfigOmitsExtractBackendWithoutExtractKey(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: true, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "extract_backend") {
		t.Fatalf("config unexpectedly sets extract_backend:\n%s", rendered)
	}
}

func TestRenderConfigAddsExtractBackendWhenEnabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: true, extractEnabled: true, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	for _, want := range []string{"    - web\n", "  search_backend: \"searxng\"\n", "  extract_backend: \"tavily\"\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("config is missing %q:\n%s", want, text)
		}
	}
}

// extract_backend must never be rendered when web search itself is off, even
// if a caller passes extractEnabled=true by mistake.
func TestRenderConfigOmitsExtractBackendWhenWebSearchDisabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: true, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "- web") || strings.Contains(text, "\nweb:") || strings.Contains(text, "extract_backend") {
		t.Fatalf("config unexpectedly enables web/extract while web search is disabled:\n%s", text)
	}
}

func TestRenderConfigDisablesDelegationToolsetWhenSubagentsDisabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: false, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if !strings.Contains(text, "\ndisabled_toolsets:\n  - delegation\n") {
		t.Fatalf("config is missing disabled_toolsets: [delegation]:\n%s", text)
	}
}

func TestRenderConfigOmitsDisabledToolsetsWhenSubagentsEnabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "disabled_toolsets") {
		t.Fatalf("config unexpectedly disables toolsets while subagents are enabled:\n%s", text)
	}
}

func TestRenderConfigSetsMaxConcurrentChildrenWhenLimited(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 2})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if !strings.Contains(text, "\ndelegation:\n  max_concurrent_children: 2\n") {
		t.Fatalf("config is missing delegation.max_concurrent_children:\n%s", text)
	}
}

func TestRenderConfigOmitsDelegationBlockWhenNoLimitSet(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "\ndelegation:\n") {
		t.Fatalf("config unexpectedly sets a delegation block with no limit configured:\n%s", text)
	}
}

func TestRenderConfigIgnoresMaxConcurrentChildrenWhenSubagentsDisabled(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 90, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: false, maxConcurrentSubagents: 2})
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	if strings.Contains(text, "\ndelegation:\n") {
		t.Fatalf("config unexpectedly sets a delegation block while subagents are disabled:\n%s", text)
	}
}

func TestContainerEnvironmentSetsSearXNGURLWhenWebSearchEnabled(t *testing.T) {
	disabled, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range disabled {
		if strings.HasPrefix(entry, "SEARXNG_URL=") {
			t.Fatalf("SEARXNG_URL set despite web search being disabled: %v", disabled)
		}
	}
	enabled, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, true, "run-1", "fix-git")
	if err != nil {
		t.Fatal(err)
	}
	want := "SEARXNG_URL=" + searxngBaseURL
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

// --toolsets is unusable on the pinned Hermes build, so the wrapper must not
// pass it; toolsets come from the rendered config instead.
func TestAgentWrapperExportsKeyAndAvoidsToolsetsFlag(t *testing.T) {
	script := string(agentWrapperScript("DEEPSEEK_API_KEY", false))
	for _, want := range []string{
		"DEEPSEEK_API_KEY=\"$(cat " + modelKeyPath + ")\"",
		"export DEEPSEEK_API_KEY",
		`exec hermes --ignore-rules --yolo --model "$1" --provider "$2" -z "$3"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "--toolsets") {
		t.Fatalf("wrapper passes --toolsets:\n%s", script)
	}
	if strings.Contains(script, tavilyAPIKeyEnv) || strings.Contains(script, extractKeyPath) {
		t.Fatalf("wrapper exports the extract key despite extract being disabled:\n%s", script)
	}
}

func TestAgentWrapperExportsExtractKeyWhenEnabled(t *testing.T) {
	script := string(agentWrapperScript("DEEPSEEK_API_KEY", true))
	for _, want := range []string{
		"DEEPSEEK_API_KEY=\"$(cat " + modelKeyPath + ")\"",
		"export DEEPSEEK_API_KEY",
		tavilyAPIKeyEnv + "=\"$(cat " + extractKeyPath + ")\"",
		"export " + tavilyAPIKeyEnv,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("wrapper is missing %q:\n%s", want, script)
		}
	}
}

// Neither pinned Hermes version knows an "sglang" or plain "openai" provider,
// and the one-shot rejects an unknown name, so both backends must render as
// Hermes's generic "custom" provider. DeepSeek is built in and stays as written.
func TestRenderConfigMapsOpenAICompatibleBackendsToCustomProvider(t *testing.T) {
	for _, provider := range []string{"sglang", "openai"} {
		model := validModel()
		model.Provider = provider
		model.BaseURL = "http://vllm.local:8000/v1"
		rendered, err := renderConfig(model, renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
		if err != nil {
			t.Fatal(err)
		}
		text := string(rendered)
		if !strings.Contains(text, `provider: "custom"`) || strings.Contains(text, `provider: "`+provider+`"`) {
			t.Fatalf("%s backend was not rendered as the custom provider:\n%s", provider, text)
		}
		if got := hermesProvider(provider); got != "custom" {
			t.Fatalf("hermesProvider(%s) = %q", provider, got)
		}
	}
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 10, webSearchEnabled: false, extractEnabled: false, subagentsEnabled: true, maxConcurrentSubagents: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `provider: "deepseek"`) || hermesProvider("deepseek") != "deepseek" {
		t.Fatal("deepseek provider was rewritten")
	}
}
