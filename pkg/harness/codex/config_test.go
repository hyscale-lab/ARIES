package codex

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/hyscale-lab/aries/pkg/core"
)

func testModel() core.ModelConfig {
	return core.ModelConfig{Provider: "openai", BaseURL: "http://model:8000/v1/", Model: "model-27B", APIKeyEnv: "ARIES_MODEL_KEY"}
}

func testEndpoint(t *testing.T) core.ToolEndpoint {
	t.Helper()
	return core.ToolEndpoint{Protocol: "ssh", Address: "task-sandbox:2222", Username: "aries", Workdir: "/app", ClientCommand: clientPath, ClientSourceFile: "/host/aries-codex-ssh"}
}

func TestConfigUsesResponsesAndRemoteEnvironmentOnly(t *testing.T) {
	model := testModel()
	model.ContextLength = 65536
	config, err := renderConfig(model, Subagents{})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Model      string `toml:"model"`
		Provider   string `toml:"model_provider"`
		Context    int    `toml:"model_context_window"`
		WebSearch  string `toml:"web_search"`
		LoginShell *bool  `toml:"allow_login_shell"`
		Shell      struct {
			Inherit string   `toml:"inherit"`
			Exclude []string `toml:"exclude"`
		} `toml:"shell_environment_policy"`
		Providers map[string]struct {
			Wire string `toml:"wire_api"`
			Key  string `toml:"env_key"`
			URL  string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if _, err := toml.Decode(string(config), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Provider != "aries" || decoded.Model != model.Model || decoded.Context != model.ContextLength || decoded.Providers["aries"].Wire != "responses" || decoded.Providers["aries"].Key != model.APIKeyEnv || decoded.Providers["aries"].URL != "http://model:8000/v1" || decoded.Shell.Inherit != "all" || len(decoded.Shell.Exclude) != 1 || decoded.Shell.Exclude[0] != model.APIKeyEnv || decoded.WebSearch != "disabled" {
		t.Fatalf("unexpected configuration: %s", config)
	}
	if decoded.LoginShell == nil || *decoded.LoginShell {
		t.Fatal("login shells may replace the task image's PATH")
	}
	endpoint := testEndpoint(t)
	environments, err := renderEnvironments(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	var remote struct {
		Default      string `toml:"default"`
		Local        bool   `toml:"include_local"`
		Environments []struct {
			ID      string   `toml:"id"`
			Program string   `toml:"program"`
			Args    []string `toml:"args"`
			Timeout int      `toml:"initialize_timeout_sec"`
		} `toml:"environments"`
	}
	if _, err := toml.Decode(string(environments), &remote); err != nil {
		t.Fatal(err)
	}
	if remote.Default != "aries" || remote.Local || len(remote.Environments) != 1 {
		t.Fatalf("unsafe environments: %s", environments)
	}
	entry := remote.Environments[0]
	if entry.ID != "aries" || entry.Program != clientPath || entry.Timeout != 30 || strings.Join(entry.Args, "|") != "--address|task-sandbox:2222" {
		t.Fatalf("unexpected environment: %s", environments)
	}
}

func TestUnsupportedModelSettingsFailExplicitly(t *testing.T) {
	for _, mutate := range []func(*core.ModelConfig){
		func(m *core.ModelConfig) { m.Provider = "deepseek" },
		func(m *core.ModelConfig) { m.BaseURL = "http://model/v1?key=secret" },
		func(m *core.ModelConfig) { m.APIKeyEnv = "CODEX_HOME" },
		func(m *core.ModelConfig) { m.APIKeyEnv = "LD_PRELOAD" },
		func(m *core.ModelConfig) { m.APIKeyEnv = "BAD;NAME" },
		func(m *core.ModelConfig) { m.MaxTokens = 100 },
		func(m *core.ModelConfig) { value := 0.7; m.Temperature = &value },
	} {
		model := testModel()
		mutate(&model)
		if _, err := renderConfig(model, Subagents{}); err == nil {
			t.Fatalf("accepted invalid model: %#v", model)
		}
	}
}

func TestWrapperKeepsPromptAsOneArgumentAndKeyOutOfMetadata(t *testing.T) {
	script := agentWrapperScript("ARIES_MODEL_KEY")
	if !bytes.Contains(script, []byte(`export ARIES_MODEL_KEY`)) || !bytes.Contains(script, []byte(`exec /run/aries/codex/codex "$@"`)) {
		t.Fatalf("unexpected wrapper: %s", script)
	}
	if bytes.Contains(script, []byte("ignore-user-config")) {
		t.Fatal("wrapper disables remote environments")
	}
	for _, name := range []string{"HOME", "PATH", "SHELL", "BASH_ENV", "ENV", "CODEX_HOME", "RUST_LOG", "LD_LIBRARY_PATH"} {
		model := testModel()
		model.APIKeyEnv = name
		if _, err := renderConfig(model, Subagents{}); err == nil {
			t.Fatalf("reserved key variable accepted: %s", name)
		}
	}
}

func TestNativeTrajectoryRequiresSuccessfulTurn(t *testing.T) {
	content := []byte("{\"type\":\"thread.started\",\"thread_id\":\"id\"}\n{\"type\":\"item.completed\",\"item\":{\"id\":\"a\",\"type\":\"agent_message\",\"text\":\"done\\nexact\"}}\n{\"type\":\"turn.completed\",\"usage\":{}}\n")
	final, err := finalResponse(content)
	if err != nil || final != "done\nexact" {
		t.Fatalf("final=%q err=%v", final, err)
	}
	for _, bad := range [][]byte{[]byte("not JSON\n"), bytes.ReplaceAll(content, []byte("turn.completed"), []byte("turn.failed")), []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"partial\"}}\n")} {
		if _, err := finalResponse(bad); err == nil {
			t.Fatalf("accepted incomplete trajectory: %s", bad)
		}
	}
}

type renderedDelegation struct {
	Effort string `toml:"model_reasoning_effort"`
	Agents struct {
		Enabled       *bool `toml:"enabled"`
		MaxConcurrent int   `toml:"max_concurrent_threads_per_session"`
	} `toml:"agents"`
	Features struct {
		MultiAgentV2 *struct {
			Enabled bool   `toml:"enabled"`
			Hint    string `toml:"multi_agent_mode_hint_text"`
		} `toml:"multi_agent_v2"`
	} `toml:"features"`
}

func decodeDelegation(t *testing.T, model core.ModelConfig, subagents Subagents) renderedDelegation {
	t.Helper()
	config, err := renderConfig(model, subagents)
	if err != nil {
		t.Fatal(err)
	}
	var decoded renderedDelegation
	if _, err := toml.Decode(string(config), &decoded); err != nil {
		t.Fatalf("%v\n%s", err, config)
	}
	return decoded
}

// Codex enables sub-agents by default, so disabled must be explicit, and
// enabled turns on multi-agent V2 with the proactive delegation hint while the
// requested effort stays what the profile asked for.
func TestDelegationIsExplicitAndKeepsTheRequestedEffort(t *testing.T) {
	model := testModel()
	model.ReasoningEffort = "xhigh"
	off := decodeDelegation(t, model, Subagents{})
	if off.Agents.Enabled == nil || *off.Agents.Enabled || off.Features.MultiAgentV2 != nil || off.Effort != "xhigh" {
		t.Fatalf("disabled delegation = %+v", off)
	}
	on := decodeDelegation(t, model, Subagents{Enabled: true, MaxConcurrent: 4})
	if on.Agents.Enabled == nil || !*on.Agents.Enabled || on.Agents.MaxConcurrent != 4 || on.Effort != "xhigh" {
		t.Fatalf("enabled delegation = %+v", on)
	}
	if v2 := on.Features.MultiAgentV2; v2 == nil || !v2.Enabled || v2.Hint != proactiveDelegationText {
		t.Fatalf("multi-agent V2 = %+v", v2)
	}
	if unset := decodeDelegation(t, testModel(), Subagents{Enabled: true}); unset.Effort != "" || unset.Agents.MaxConcurrent != 0 {
		t.Fatalf("unset effort or concurrency was rendered: %+v", unset)
	}
	model.ReasoningEffort = "off"
	if off := decodeDelegation(t, model, Subagents{}); off.Effort != "none" {
		t.Fatalf("off rendered as %q, want none", off.Effort)
	}
	for _, effort := range []string{"ultra", "persistent", "extreme"} {
		model.ReasoningEffort = effort
		if _, err := renderConfig(model, Subagents{}); err == nil {
			t.Fatalf("effort %q accepted", effort)
		}
	}
	if _, err := renderConfig(testModel(), Subagents{Enabled: true, MaxConcurrent: -1}); err == nil {
		t.Fatal("negative concurrency accepted")
	}
}

// The hint must stay byte-identical to the pinned release's bundled text.
func TestProactiveDelegationTextMatchesPinnedCodexSource(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", ".cache", "codex-rust-v"+supportedVersion, "codex-rs", "prompts", "src", "model_messages", "multi_agent.rs"))
	if errors.Is(err, os.ErrNotExist) {
		t.Skip("pinned Codex source checkout is not present")
	}
	if err != nil {
		t.Fatal(err)
	}
	const declaration = "const PROACTIVE_MULTI_AGENT_MODE_TEXT: &str = "
	start := strings.Index(string(source), declaration)
	if start < 0 {
		t.Fatal("pinned source no longer declares the proactive text")
	}
	rest := string(source)[start+len(declaration):]
	literal := rest[:strings.Index(rest, ";\n")]
	text, err := strconv.Unquote(literal)
	if err != nil {
		t.Fatal(err)
	}
	if text != proactiveDelegationText {
		t.Fatalf("pinned proactive text differs:\n%q", text)
	}
}
