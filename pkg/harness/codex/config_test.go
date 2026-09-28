package codex

import (
	"bytes"
	"os"
	"path/filepath"
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
	dir := t.TempDir()
	identity := filepath.Join(dir, "identity")
	hosts := filepath.Join(dir, "known_hosts")
	for name, value := range map[string]string{identity: "private-ssh-identity", hosts: "task-sandbox ssh-ed25519 host-key"} {
		if err := os.WriteFile(name, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return core.ToolEndpoint{Protocol: "ssh", Address: "task-sandbox:2222", Username: "aries", Network: "aries-task-net", Workdir: "/app", ClientCommand: clientPath, ClientSourceFile: "/host/aries-codex-ssh", IdentityFile: identityPath, IdentitySourceFile: identity, KnownHostsFile: knownHostsPath, KnownHostsSourceFile: hosts}
}

func TestConfigUsesResponsesAndRemoteEnvironmentOnly(t *testing.T) {
	model := testModel()
	model.ContextLength = 65536
	config, err := renderConfig(model)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Model     string `toml:"model"`
		Provider  string `toml:"model_provider"`
		Context   int    `toml:"model_context_window"`
		WebSearch string `toml:"web_search"`
		Shell     struct {
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
	if decoded.Provider != "aries" || decoded.Model != model.Model || decoded.Context != model.ContextLength || decoded.Providers["aries"].Wire != "responses" || decoded.Providers["aries"].Key != model.APIKeyEnv || decoded.Providers["aries"].URL != "http://model:8000/v1" || decoded.Shell.Inherit != "core" || len(decoded.Shell.Exclude) != 1 || decoded.Shell.Exclude[0] != model.APIKeyEnv || decoded.WebSearch != "disabled" {
		t.Fatalf("unexpected configuration: %s", config)
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
	if entry.ID != "aries" || entry.Program != clientPath || entry.Timeout != 30 || strings.Join(entry.Args, "|") != "--address|task-sandbox:2222|--user|aries|--identity|/run/aries/ssh/id_ed25519|--known-hosts|/run/aries/ssh/known_hosts" {
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
		if _, err := renderConfig(model); err == nil {
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
		if _, err := renderConfig(model); err == nil {
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
