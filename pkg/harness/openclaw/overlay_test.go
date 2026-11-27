package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"
)

func openAIModel() core.ModelConfig {
	model := testModel()
	model.Provider, model.BaseURL, model.Model, model.APIKeyEnv = "openai", "http://169.254.0.1:8000/v1", "/mnt/models/local-model", "LOCAL_API_KEY"
	return model
}

func testOverlay() Overlay {
	decode := func(text string) map[string]any {
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			panic(err)
		}
		return object
	}
	return Overlay{
		Model:    decode(`{"contextWindow":2000000,"contextTokens":2000000,"maxTokens":32768,"reasoning":true,"compat":{"maxTokensField":"max_tokens","thinkingFormat":"openrouter"}}`),
		Provider: decode(`{"timeoutSeconds":900}`),
		AgentDefaults: decode(`{"params":{"temperature":1.0,"extra_body":{"metadata":{"session_id":"${ARIES_RUN_ID}-${ARIES_TASK_ID}","enabled":true},` +
			`"chat_template_kwargs":{"enable_thinking":true}}},"heartbeat":{"every":"0m"},"skipBootstrap":true}`),
	}
}

func TestApplyOverlayAddsSectionsAndKeepsOwnedKeys(t *testing.T) {
	model := openAIModel()
	rendered, err := renderConfig(model, testEndpoint(), ModeAgent, "", false, false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	overlay := testOverlay()
	content, err := applyOverlay(rendered, overlay, "20260101T000000Z-oc", "fix-git-001")
	if err != nil {
		t.Fatal(err)
	}
	var configuration struct {
		Models struct {
			Providers map[string]struct {
				BaseURL        string           `json:"baseUrl"`
				APIKey         string           `json:"apiKey"`
				API            string           `json:"api"`
				TimeoutSeconds int              `json:"timeoutSeconds"`
				Models         []map[string]any `json:"models"`
			} `json:"providers"`
		} `json:"models"`
		Agents struct {
			Defaults map[string]any `json:"defaults"`
		} `json:"agents"`
		Gateway map[string]any `json:"gateway"`
	}
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	provider := configuration.Models.Providers["aries"]
	if provider.BaseURL != "http://169.254.0.1:8000/v1" || provider.APIKey != "${LOCAL_API_KEY}" || provider.API != "openai-completions" || provider.TimeoutSeconds != 900 || len(provider.Models) != 1 {
		t.Fatalf("provider = %#v", provider)
	}
	entry := provider.Models[0]
	if entry["id"] != model.Model || entry["name"] != model.Model || entry["reasoning"] != true || entry["contextWindow"] != 2e6 {
		t.Fatalf("model entry = %#v", entry)
	}
	if compat := entry["compat"].(map[string]any); compat["thinkingFormat"] != "openrouter" || compat["maxTokensField"] != "max_tokens" {
		t.Fatalf("compat = %#v", compat)
	}
	defaults := configuration.Agents.Defaults
	if defaults["model"].(map[string]any)["primary"] != "aries/"+model.Model || defaults["sandbox"].(map[string]any)["backend"] != "ssh" || defaults["skipBootstrap"] != true {
		t.Fatalf("defaults = %#v", defaults)
	}
	metadata := defaults["params"].(map[string]any)["extra_body"].(map[string]any)["metadata"].(map[string]any)
	if metadata["session_id"] != "20260101T000000Z-oc-fix-git-001" || metadata["enabled"] != true {
		t.Fatalf("metadata = %#v", metadata)
	}
	if configuration.Gateway["mode"] != "local" {
		t.Fatalf("gateway = %#v", configuration.Gateway)
	}
	// Numbers keep their written form and nothing is HTML-escaped.
	if !bytes.Contains(content, []byte(`"contextWindow": 2000000`)) || bytes.Contains(content, []byte(`2e+06`)) || bytes.Contains(content, []byte(`<`)) {
		t.Fatalf("encoding = %s", content)
	}
	// The overlay itself is not modified by the substitution.
	if overlay.AgentDefaults["params"].(map[string]any)["extra_body"].(map[string]any)["metadata"].(map[string]any)["session_id"] != "${ARIES_RUN_ID}-${ARIES_TASK_ID}" {
		t.Fatal("applyOverlay modified the overlay")
	}
}

// With a reasoning effort the harness writes the model entry's reasoning and
// compat itself, so an overlay of either is refused rather than merged.
func TestApplyOverlayRefusesKeysTheRenderedFileHolds(t *testing.T) {
	model := openAIModel()
	model.ReasoningEffort = "high"
	rendered, err := renderConfig(model, testEndpoint(), ModeAgent, "", false, false, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	for name, overlay := range map[string]Overlay{
		"reasoning": {Model: map[string]any{"reasoning": false}},
		"compat":    {Model: map[string]any{"compat": map[string]any{"supportsReasoningEffort": true}}},
	} {
		if _, err := applyOverlay(rendered, overlay, "run-1", "fix-git"); err == nil || !strings.Contains(err.Error(), "would replace what the harness configures") {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
	if _, err := applyOverlay(rendered, Overlay{Model: map[string]any{"contextWindow": json.Number("262144")}}, "run-1", "fix-git"); err != nil {
		t.Fatalf("context window refused: %v", err)
	}
}

func TestNewRefusesOverlayOfWhatTheHarnessWrites(t *testing.T) {
	construct := func(overlay Overlay, limit int64) error {
		_, err := New(Options{Runtime: harnesscommon.RuntimeOptions{Deployment: newFakeDeployment(), Image: testOpenClawImage, OutputDir: t.TempDir()}, Overlay: overlay, TelemetryLimit: limit})
		return err
	}
	if err := construct(testOverlay(), 0); err != nil {
		t.Fatalf("overlay refused: %v", err)
	}
	for name, overlay := range map[string]Overlay{
		"model id":         {Model: map[string]any{"id": "other"}},
		"model base url":   {Model: map[string]any{"baseUrl": "http://elsewhere/v1"}},
		"model api":        {Model: map[string]any{"api": "openai-responses"}},
		"base url":         {Provider: map[string]any{"baseUrl": "http://elsewhere/v1"}},
		"api key":          {Provider: map[string]any{"apiKey": "x"}},
		"models":           {Provider: map[string]any{"models": []any{}}},
		"primary":          {AgentDefaults: map[string]any{"model": map[string]any{"primary": "x/y"}}},
		"per-model params": {AgentDefaults: map[string]any{"models": map[string]any{}}},
		"sandbox":          {AgentDefaults: map[string]any{"sandbox": nil}},
		"subagents":        {AgentDefaults: map[string]any{"subagents": map[string]any{}}},
	} {
		if err := construct(overlay, 0); err == nil {
			t.Fatalf("%s: expected refusal", name)
		}
	}
	if err := construct(Overlay{}, -1); err == nil {
		t.Fatal("negative telemetry limit accepted")
	}
}

func TestStartWritesTheOverlayOnlyWhenConfigured(t *testing.T) {
	start := func(overlay Overlay) ([]byte, core.HarnessRequest) {
		t.Helper()
		manager := newTestManager(t, newFakeDeployment(), []byte("model-secret"))
		manager.options.Overlay = overlay
		request := core.HarnessRequest{Connectivity: core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: "aries-net-test"}}, RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: openAIModel()}
		if err := manager.Start(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		defer manager.Stop(context.Background())
		retained, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "openclaw.json"))
		if err != nil {
			t.Fatal(err)
		}
		return retained, request
	}
	retained, request := start(Overlay{})
	rendered, err := renderConfig(request.Model, request.Endpoint, ModeAgent, "", false, false, false, 0, MCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retained, rendered) {
		t.Fatalf("config without the block changed:\n%s\nwant:\n%s", retained, rendered)
	}
	retained, _ = start(testOverlay())
	if !bytes.Contains(retained, []byte(`"session_id": "run-1-fix-git"`)) || !bytes.Contains(retained, []byte(`"contextWindow": 2000000`)) || bytes.Contains(retained, []byte("${ARIES_")) {
		t.Fatalf("config with the block = %s", retained)
	}
}
