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
	rendered, err := renderConfig(validModel(), 90)
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
	rendered, err := renderConfig(model, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), `base_url: "http://host:30000/v1"`) {
		t.Fatalf("SGLang base URL was not normalized:\n%s", rendered)
	}
	bad := map[string]func(*core.ModelConfig){
		"provider":  func(m *core.ModelConfig) { m.Provider = "openai" },
		"base url":  func(m *core.ModelConfig) { m.BaseURL = "ftp://host" },
		"model id":  func(m *core.ModelConfig) { m.Model = " " },
		"key env":   func(m *core.ModelConfig) { m.APIKeyEnv = "1BAD" },
		"url query": func(m *core.ModelConfig) { m.BaseURL = "https://api.deepseek.com?x=1" },
	}
	for name, mutate := range bad {
		model := validModel()
		mutate(&model)
		if _, err := renderConfig(model, 10); err == nil {
			t.Fatalf("%s: invalid model was accepted", name)
		}
	}
	if _, err := renderConfig(validModel(), 0); err == nil {
		t.Fatal("non-positive max turns was accepted")
	}
}

// A value that could terminate its own YAML scalar must be escaped, not emitted.
func TestRenderConfigQuotesInjectionAttempts(t *testing.T) {
	model := validModel()
	model.Model = `x" \nevil: true`
	rendered, err := renderConfig(model, 10)
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
	environment, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HERMES_HOME":       stateContainerPath,
		"TERMINAL_ENV":      "ssh",
		"TERMINAL_SSH_HOST": "172.17.0.1",
		"TERMINAL_SSH_PORT": "41234",
		"TERMINAL_SSH_USER": "aries",
		"TERMINAL_SSH_KEY":  identityContainerFS,
		"TERMINAL_CWD":      "/aries/workspace",
		"TERMINAL_TIMEOUT":  "180",
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
		if _, err := containerEnvironment(endpoint, "/aries/workspace", 180); err == nil {
			t.Fatalf("%s: invalid endpoint was accepted", name)
		}
	}
	for _, workdir := range []string{"", "relative", "/has space", "/trailing/", "/a/../b"} {
		if _, err := containerEnvironment(validEndpoint(), workdir, 180); err == nil {
			t.Fatalf("workdir %q was accepted", workdir)
		}
	}
	if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 0); err == nil {
		t.Fatal("non-positive terminal timeout was accepted")
	}
}

// --toolsets is unusable on the pinned Hermes build, so the wrapper must not
// pass it; toolsets come from the rendered config instead.
func TestAgentWrapperExportsKeyAndAvoidsToolsetsFlag(t *testing.T) {
	script := string(agentWrapperScript("DEEPSEEK_API_KEY"))
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
}
