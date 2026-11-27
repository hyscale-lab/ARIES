package openclaw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Overlay holds the profile's harness.openclaw objects, merged into the
// openclaw.json ARIES renders for each task: Model into the provider's one
// model entry, Provider into the provider entry, and AgentDefaults into
// agents.defaults.
type Overlay struct {
	Model         map[string]any
	Provider      map[string]any
	AgentDefaults map[string]any
}

func (overlay Overlay) empty() bool {
	return len(overlay.Model) == 0 && len(overlay.Provider) == 0 && len(overlay.AgentDefaults) == 0
}

func (overlay Overlay) objects() map[string]map[string]any {
	return map[string]map[string]any{"model": overlay.Model, "provider": overlay.Provider, "agent_defaults": overlay.AgentDefaults}
}

// overlayOwnedKeys set the model id, endpoint, credential, transport, sandbox,
// primary model, or per-model request parameters that the harness configures,
// so an overlay may not set them. New refuses these before any effect;
// applyOverlay also refuses any key the rendered file already holds, which
// covers the model entry's reasoning and compat that renderConfig writes only
// for a model with a reasoning effort. pkg/config accepts a narrower set of
// keys from a profile, none of these.
var overlayOwnedKeys = map[string][]string{
	"model":          {"id", "name", "baseUrl", "api", "headers"},
	"provider":       {"baseUrl", "apiKey", "api", "models", "auth", "headers"},
	"agent_defaults": {"model", "models", "sandbox", "subagents"},
}

func (overlay Overlay) validate() error {
	for name, object := range overlay.objects() {
		for _, key := range overlayOwnedKeys[name] {
			if _, ok := object[key]; ok {
				return fmt.Errorf("OpenClaw overlay %s.%s would replace what the harness configures", name, key)
			}
		}
	}
	return nil
}

// applyOverlay adds an overlay New accepted to a configuration renderConfig
// produced and replaces ${ARIES_RUN_ID} and ${ARIES_TASK_ID} in the overlay's
// string values.
func applyOverlay(rendered []byte, overlay Overlay, runID, taskID string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(rendered))
	decoder.UseNumber()
	var configuration map[string]any
	if err := decoder.Decode(&configuration); err != nil {
		return nil, fmt.Errorf("decode rendered OpenClaw config: %w", err)
	}
	providers, _ := lookupObject(configuration, "models", "providers")
	if len(providers) != 1 {
		return nil, errors.New("rendered OpenClaw config must hold exactly one provider")
	}
	var provider map[string]any
	for _, value := range providers {
		provider, _ = value.(map[string]any)
	}
	models, ok := provider["models"].([]any)
	if !ok || len(models) != 1 {
		return nil, errors.New("rendered OpenClaw provider must hold exactly one model")
	}
	model, ok := models[0].(map[string]any)
	if !ok {
		return nil, errors.New("rendered OpenClaw model entry is not an object")
	}
	defaults, ok := lookupObject(configuration, "agents", "defaults")
	if !ok {
		return nil, errors.New("rendered OpenClaw config has no agents.defaults")
	}
	replacer := strings.NewReplacer("${ARIES_RUN_ID}", runID, "${ARIES_TASK_ID}", taskID)
	targets := map[string]map[string]any{"model": model, "provider": provider, "agent_defaults": defaults}
	objects := overlay.objects()
	for _, name := range []string{"model", "provider", "agent_defaults"} {
		for _, key := range slices.Sorted(maps.Keys(objects[name])) {
			if _, exists := targets[name][key]; exists {
				return nil, fmt.Errorf("OpenClaw overlay %s.%s would replace what the harness configures", name, key)
			}
			targets[name][key] = substitute(objects[name][key], replacer)
		}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(configuration); err != nil {
		return nil, fmt.Errorf("encode OpenClaw config: %w", err)
	}
	return output.Bytes(), nil
}

func lookupObject(root map[string]any, keys ...string) (map[string]any, bool) {
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

// substitute returns a deep copy of value with replacer applied to every
// string value.
func substitute(value any, replacer *strings.Replacer) any {
	switch typed := value.(type) {
	case map[string]any:
		copied := make(map[string]any, len(typed))
		for key, element := range typed {
			copied[key] = substitute(element, replacer)
		}
		return copied
	case []any:
		copied := make([]any, len(typed))
		for index, element := range typed {
			copied[index] = substitute(element, replacer)
		}
		return copied
	case string:
		return replacer.Replace(typed)
	}
	return value
}
