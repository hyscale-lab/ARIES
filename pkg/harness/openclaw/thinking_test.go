package openclaw

import (
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

// A DeepSeek model missing from disablesThinking does not fail: it runs with
// thinking on, which changes latency and token use without any error. Pin the
// served models so a rename on DeepSeek's side has to be handled here too.
func TestDisablesThinkingForServedDeepSeekModels(t *testing.T) {
	const base = "https://api.deepseek.com"
	for _, model := range []string{"deepseek-flash", "deepseek-v4-pro"} {
		if !disablesThinking(core.ModelConfig{BaseURL: base, Model: model}) {
			t.Errorf("%s would run with thinking on", model)
		}
	}
	for _, model := range []core.ModelConfig{
		// Retired by DeepSeek in favour of deepseek-flash.
		{BaseURL: base, Model: "deepseek-v4-flash"},
		{BaseURL: base + "/", Model: "deepseek-flash"},
		{BaseURL: "http://127.0.0.1:30000", Model: "deepseek-flash"},
	} {
		if disablesThinking(model) {
			t.Errorf("near match treated as official: %+v", model)
		}
	}
}
