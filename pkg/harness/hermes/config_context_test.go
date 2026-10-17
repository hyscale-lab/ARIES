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
	rendered, err := renderConfig(model, settings, nil)
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
	config := decodeConfigYAML(t, []byte(text))
	nativeModel := config["model"].(map[string]any)
	body := config["custom_providers"].([]any)[0].(map[string]any)["extra_body"].(map[string]any)
	if nativeModel["context_length"] != 262144 || nativeModel["max_tokens"] != 32768 || body["temperature"] != float64(1) {
		t.Fatalf("generation settings = %#v / %#v", nativeModel, body)
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
		if _, err := renderConfig(model, baseSettings(), nil); err == nil {
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
	var parsed struct {
		Compression map[string]any `yaml:"compression"`
	}
	if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatal(err)
	}
	_, hasEnabled := parsed.Compression["enabled"]
	if parsed.Compression["threshold_tokens"] != 65536 || hasEnabled {
		t.Fatalf("threshold-only compaction block is wrong:\n%s", text)
	}
	settings.compaction = &CompactionSettings{Enabled: boolPtr(false)}
	disabled := decodeConfigYAML(t, []byte(mustRender(t, vllmModel(), settings)))["compression"].(map[string]any)
	if disabled["enabled"] != false || len(disabled) != 1 {
		t.Fatalf("disabled compaction = %#v", disabled)
	}
	settings.compaction = &CompactionSettings{}
	empty := decodeConfigYAML(t, []byte(mustRender(t, vllmModel(), settings)))
	if value, exists := empty["compression"]; !exists || value != nil {
		t.Fatalf("empty compaction = %#v", empty)
	}
	model := vllmModel()
	model.ContextLength = 65536
	settings.compaction = &CompactionSettings{ThresholdTokens: 65536}
	if _, err := renderConfig(model, settings, nil); err == nil {
		t.Fatal("threshold at the context length was accepted")
	}
	settings.compaction = &CompactionSettings{ThresholdTokens: -1}
	if _, err := renderConfig(vllmModel(), settings, nil); err == nil {
		t.Fatal("negative threshold was accepted")
	}
}

// A YAML reader must recover the opaque JSON object, with the ${ARIES_*}
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
	if len(odd) != 7 || odd[0] != "a: b" || odd[1] != "x<y #z" || odd[2] != nil || odd[3] != float64(1000) || odd[4] != "tab\tchar" || odd[5] != `q"q` || odd[6] != "brace }" {
		t.Fatalf("odd = %#v", odd)
	}
	for _, bad := range []string{`[1]`, `"x"`, `{}`, `null`, `{"a":`, `{"a":1} {"b":2}`, `{"a":1} trailing`, ``} {
		settings.extraBody = []byte(bad)
		if _, err := renderConfig(vllmModel(), settings, nil); err == nil && bad != "" {
			t.Fatalf("accepted extra_body %q", bad)
		}
	}
	settings.extraBody = []byte(`{"a": 1}`)
	if _, err := renderConfig(validModel(), settings, nil); err == nil {
		t.Fatal("extra_body under the deepseek provider was accepted")
	}
}

func TestContainerEnvironmentExportsRunAndTaskIDs(t *testing.T) {
	environment, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "", "run-7", "fix-git-001")
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
		if _, err := containerEnvironment(validEndpoint(), "/aries/workspace", 180, false, "", ids[0], ids[1]); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// The optional blocks and the identifying environment reach the container
// through the real Start path.
func TestStartRendersContextBlocksAndExportsIDs(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("EMPTY-but-long-enough"))
	manager.options.Compaction = &CompactionSettings{ThresholdTokens: 65536}
	manager.options.ExtraBody = []byte(`{"user": "${ARIES_RUN_ID}-${ARIES_TASK_ID}"}`)
	request := testRequest(t)
	request.RunID = "run-7"
	request.Model = vllmModel()
	request.Model.ContextLength = 262144
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	retained, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	config := decodeConfigYAML(t, retained)
	nativeModel := config["model"].(map[string]any)
	body := config["custom_providers"].([]any)[0].(map[string]any)["extra_body"].(map[string]any)
	if nativeModel["provider"] != "custom" || nativeModel["api_key"] != "${VLLM_API_KEY}" || nativeModel["context_length"] != 262144 || config["compression"].(map[string]any)["threshold_tokens"] != 65536 || body["user"] != "${ARIES_RUN_ID}-${ARIES_TASK_ID}" {
		t.Fatalf("retained config = %#v", config)
	}
	joined := strings.Join(fake.created.Env, "\n")
	for _, want := range []string{"ARIES_RUN_ID=run-7", "ARIES_TASK_ID=fix-git"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("container environment lacks %q: %v", want, fake.created.Env)
		}
	}
}

// Temperature must reach the custom request overrides, including explicit zero.
func TestRenderTemperatureUsesRequestExtraBody(t *testing.T) {
	for _, backend := range []string{"openai", "sglang"} {
		for _, temperature := range []float64{0, 0.7} {
			model := vllmModel()
			model.Provider, model.Temperature = backend, floatPtr(temperature)
			settings := baseSettings()
			settings.extraBody = []byte(`{"metadata":{"weight":1234567890123456789}}`)
			text := mustRender(t, model, settings)
			var parsed struct {
				Model     map[string]any `yaml:"model"`
				Providers []struct {
					Extra map[string]any `yaml:"extra_body"`
				} `yaml:"custom_providers"`
			}
			if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
				t.Fatal(err)
			}
			if _, exists := parsed.Model["temperature"]; exists {
				t.Fatal("temperature rendered in ignored model field")
			}
			if len(parsed.Providers) != 1 || parsed.Providers[0].Extra["temperature"] != temperature {
				t.Fatalf("request overrides: %#v", parsed.Providers)
			}
			if !strings.Contains(text, "1234567890123456789") {
				t.Fatal("extra_body number lost precision")
			}
			settings.extraBody = []byte(`{"temperature":0.2}`)
			if _, err := renderConfig(model, settings, nil); err == nil {
				t.Fatal("ambiguous temperature accepted")
			}
		}
	}
	model := validModel()
	model.Temperature = floatPtr(0)
	if _, err := renderConfig(model, baseSettings(), nil); err == nil {
		t.Fatal("unsupported DeepSeek temperature accepted")
	}
}

func TestRenderConfigPreservesJSONNumericPrecision(t *testing.T) {
	settings := baseSettings()
	settings.extraBody = []byte(`{"integer":9007199254740993,"huge":18446744073709551617,"decimal":0.1234567890123456789012345,"scientific":1e3,"nested":[0.0,"1e3"]}`)
	text := mustRender(t, vllmModel(), settings)
	var parsed struct {
		Providers []struct {
			Body yaml.Node `yaml:"extra_body"`
		} `yaml:"custom_providers"`
	}
	if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatal(err)
	}
	body := parsed.Providers[0].Body
	want := map[string][2]string{
		"integer":    {"!!int", "9007199254740993"},
		"huge":       {"!!int", "18446744073709551617"},
		"decimal":    {"!!float", "0.1234567890123456789012345"},
		"scientific": {"!!float", "1e3"},
	}
	for index := 0; index < len(body.Content); index += 2 {
		key, value := body.Content[index].Value, body.Content[index+1]
		if key == "nested" {
			if len(value.Content) != 2 || value.Content[0].Tag != "!!float" || value.Content[0].Value != "0.0" || value.Content[1].Tag != "!!str" || value.Content[1].Value != "1e3" {
				t.Fatalf("nested numbers changed: %#v", value.Content)
			}
			continue
		}
		if got := [2]string{value.Tag, value.Value}; got != want[key] {
			t.Fatalf("%s = %#v, want %#v", key, got, want[key])
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("request numbers omitted: %#v", want)
	}
}

func TestRenderReasoningUsesNativeRequestOverrides(t *testing.T) {
	for _, test := range []struct {
		provider, model, effort, wireEffort, thinking string
	}{
		{"deepseek", "deepseek-flash", "off", "", "disabled"},
		{"deepseek", "deepseek-flash", "high", "high", "enabled"},
		{"openai", "gpt-6-luna", "low", "low", ""},
		{"sglang", "a-local-model", "off", "none", ""},
	} {
		t.Run(test.provider+"/"+test.effort, func(t *testing.T) {
			model := vllmModel()
			model.Provider, model.Model, model.ReasoningEffort = test.provider, test.model, test.effort
			settings := baseSettings()
			if test.provider != "deepseek" {
				settings.extraBody = []byte(`{"request_id":9007199254740993}`)
			}
			rendered, err := renderConfig(model, settings, nil)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				NativeModel map[string]any `yaml:"model"`
				Custom      []struct {
					BaseURL string         `yaml:"base_url"`
					Extra   map[string]any `yaml:"extra_body"`
				} `yaml:"custom_providers"`
			}
			if err := yaml.Unmarshal(rendered, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.NativeModel["provider"] != "custom" || decoded.NativeModel["default"] != test.model || len(decoded.Custom) != 1 || decoded.Custom[0].BaseURL != model.BaseURL {
				t.Fatalf("reasoning changed model or route: %s", rendered)
			}
			body := decoded.Custom[0].Extra
			if test.wireEffort == "" {
				if _, exists := body["reasoning_effort"]; exists {
					t.Fatal("disabled DeepSeek reasoning retained effort")
				}
			} else if body["reasoning_effort"] != test.wireEffort {
				t.Fatalf("wire effort = %v", body["reasoning_effort"])
			}
			if test.thinking != "" {
				thinking, _ := body["thinking"].(map[string]any)
				if thinking["type"] != test.thinking {
					t.Fatalf("thinking = %v", thinking)
				}
			} else if _, exists := body["thinking"]; exists {
				t.Fatal("generic endpoint received DeepSeek thinking control")
			}
			if test.provider != "deepseek" && !strings.Contains(string(rendered), "9007199254740993") {
				t.Fatal("request number lost precision")
			}
		})
	}
	model := vllmModel()
	model.ReasoningEffort = "low"
	for _, key := range []string{"reasoning_effort", "thinking", "reasoning"} {
		settings := baseSettings()
		settings.extraBody = []byte(`{"` + key + `":null}`)
		if _, err := renderConfig(model, settings, nil); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatalf("accepted explicit reasoning conflict with %s: %v", key, err)
		}
	}
}
