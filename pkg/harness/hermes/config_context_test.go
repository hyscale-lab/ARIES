package hermes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"go.yaml.in/yaml/v3"
)

func vllmModel() core.ModelConfig {
	return core.ModelConfig{Provider: "openai", BaseURL: "http://vllm.local:8000/v1", Model: "Qwen/Qwen3.6-35B-A3B-FP8", APIKeyEnv: "VLLM_API_KEY"}
}

func baseSettings() renderSettings {
	return renderSettings{maxTurns: 90, subagentsEnabled: true}
}

func boolPtr(value bool) *bool        { return &value }
func floatPtr(value float64) *float64 { return &value }

func mustRender(t *testing.T, model core.ModelConfig, settings renderSettings) string {
	t.Helper()
	rendered, err := renderConfig(model, settings)
	if err != nil {
		t.Fatal(err)
	}
	return string(rendered)
}

func TestRenderConfigEmitsGenerationSettingsOnlyWhenSet(t *testing.T) {
	plain := mustRender(t, vllmModel(), baseSettings())
	for _, key := range []string{"context_length:", "max_tokens:", "temperature:"} {
		if strings.Contains(plain, key) {
			t.Fatalf("unset %s was rendered:\n%s", key, plain)
		}
	}
	model := vllmModel()
	model.ContextLength, model.MaxTokens, model.Temperature = 262144, 32768, floatPtr(1)
	text := mustRender(t, model, baseSettings())
	for _, line := range []string{"  context_length: 262144\n", "  max_tokens: 32768\n", "  temperature: 1.0\n"} {
		if !strings.Contains(text, line) {
			t.Fatalf("missing %q:\n%s", line, text)
		}
	}
	bad := map[string]func(*core.ModelConfig){
		"max tokens fills window": func(m *core.ModelConfig) { m.ContextLength, m.MaxTokens = 1000, 1000 },
		"negative context":        func(m *core.ModelConfig) { m.ContextLength = -1 },
		"temperature too high":    func(m *core.ModelConfig) { m.Temperature = floatPtr(2.5) },
		"temperature negative":    func(m *core.ModelConfig) { m.Temperature = floatPtr(-0.1) },
	}
	for name, mutate := range bad {
		model := vllmModel()
		mutate(&model)
		if _, err := renderConfig(model, baseSettings()); err == nil {
			t.Fatalf("%s: invalid generation settings were accepted", name)
		}
	}
}

func TestRenderConfigEmitsCompactionBlockOnlyWhenSet(t *testing.T) {
	if strings.Contains(mustRender(t, vllmModel(), baseSettings()), "compression:") {
		t.Fatal("compression block rendered without a compaction setting")
	}
	settings := baseSettings()
	settings.compaction = &CompactionSettings{ThresholdTokens: 65536}
	text := mustRender(t, vllmModel(), settings)
	if !strings.Contains(text, "\ncompression:\n  threshold_tokens: 65536\n") || strings.Contains(text, "enabled:") {
		t.Fatalf("threshold-only compaction block is wrong:\n%s", text)
	}
	settings.compaction = &CompactionSettings{Enabled: boolPtr(false)}
	if !strings.Contains(mustRender(t, vllmModel(), settings), "\ncompression:\n  enabled: false\n") {
		t.Fatal("disabled compaction was not rendered")
	}
	model := vllmModel()
	model.ContextLength = 65536
	settings.compaction = &CompactionSettings{ThresholdTokens: 65536}
	if _, err := renderConfig(model, settings); err == nil {
		t.Fatal("threshold at the context length was accepted")
	}
	settings.compaction = &CompactionSettings{ThresholdTokens: -1}
	if _, err := renderConfig(vllmModel(), settings); err == nil {
		t.Fatal("negative threshold was accepted")
	}
}

// The profile's extra_body is written verbatim as an indented JSON flow
// mapping. A YAML reader must recover the same object, with the ${ARIES_*}
// references left for Hermes to expand.
func TestRenderConfigWritesExtraBodyAsYAMLReadableJSON(t *testing.T) {
	if strings.Contains(mustRender(t, vllmModel(), baseSettings()), "custom_providers:") {
		t.Fatal("custom_providers rendered without an extra_body")
	}
	settings := baseSettings()
	settings.extraBody = []byte(`{"metadata": {"session_id": "${ARIES_RUN_ID}-${ARIES_TASK_ID}", "enabled": true, "weight": 0.5},
  "chat_template_kwargs": {"preserve_thinking": true},
  "odd": ["a: b", "x<y #z", null, 1e3, "tab\tchar", "q\"q", "brace }"]}`)
	text := mustRender(t, vllmModel(), settings)
	var parsed struct {
		Model struct {
			Provider string `yaml:"provider"`
		} `yaml:"model"`
		CustomProviders []struct {
			Name      string         `yaml:"name"`
			BaseURL   string         `yaml:"base_url"`
			ExtraBody map[string]any `yaml:"extra_body"`
		} `yaml:"custom_providers"`
	}
	if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("rendered config is not valid YAML: %v\n%s", err, text)
	}
	if parsed.Model.Provider != "custom" || len(parsed.CustomProviders) != 1 {
		t.Fatalf("parsed = %#v\n%s", parsed, text)
	}
	entry := parsed.CustomProviders[0]
	if entry.Name != "aries" || entry.BaseURL != "http://vllm.local:8000/v1" {
		t.Fatalf("entry = %#v", entry)
	}
	meta, _ := entry.ExtraBody["metadata"].(map[string]any)
	if meta["session_id"] != "${ARIES_RUN_ID}-${ARIES_TASK_ID}" || meta["enabled"] != true || meta["weight"] != 0.5 {
		t.Fatalf("metadata = %#v", meta)
	}
	odd, _ := entry.ExtraBody["odd"].([]any)
	if len(odd) != 7 || odd[0] != "a: b" || odd[1] != "x<y #z" || odd[2] != nil || odd[4] != "tab\tchar" || odd[5] != `q"q` || odd[6] != "brace }" {
		t.Fatalf("odd = %#v", odd)
	}
	for _, bad := range []string{`[1]`, `"x"`, `{}`, `null`, `{"a":`, ``} {
		settings.extraBody = []byte(bad)
		if _, err := renderConfig(vllmModel(), settings); err == nil && bad != "" {
			t.Fatalf("accepted extra_body %q", bad)
		}
	}
	settings.extraBody = []byte(`{"a": 1}`)
	if _, err := renderConfig(validModel(), settings); err == nil {
		t.Fatal("extra_body under the deepseek provider was accepted")
	}
}

func TestContainerEnvironmentExportsRunAndTaskIDs(t *testing.T) {
	environment, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "run-7", "fix-git-001")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	for _, want := range []string{"\nARIES_RUN_ID=run-7\n", "\nARIES_TASK_ID=fix-git-001"} {
		if !strings.Contains("\n"+joined, want) {
			t.Fatalf("environment %v lacks %q", environment, strings.TrimSpace(want))
		}
	}
	for name, ids := range map[string][2]string{"empty run": {"", "fix-git"}, "unsafe run": {"run 7", "fix-git"}, "empty task": {"run-7", ""}, "unsafe task": {"run-7", "fix;git"}} {
		if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, ids[0], ids[1]); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The optional blocks and the identifying environment reach the container
// through the real Start path.
func TestStartRendersContextBlocksAndExportsIDs(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("EMPTY-but-long-enough"))
	manager.compaction = &CompactionSettings{ThresholdTokens: 65536}
	manager.extraBody = []byte(`{"user": "${ARIES_RUN_ID}-${ARIES_TASK_ID}"}`)
	request := testRequest(t)
	request.RunID = "run-7"
	request.Model = vllmModel()
	request.Model.ContextLength = 262144
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(retained)
	for _, line := range []string{`"user": "${ARIES_RUN_ID}-${ARIES_TASK_ID}"`, `provider: "custom"`, "threshold_tokens: 65536", "context_length: 262144", `${VLLM_API_KEY}`} {
		if !strings.Contains(text, line) {
			t.Fatalf("retained config lacks %q:\n%s", line, text)
		}
	}
	joined := strings.Join(fake.created.Config.Env, "\n")
	for _, want := range []string{"ARIES_RUN_ID=run-7", "ARIES_TASK_ID=fix-git"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("container environment lacks %q: %v", want, fake.created.Config.Env)
		}
	}
}
