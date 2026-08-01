package gateway

import "encoding/json"

// Frame is one decoded OpenClaw Gateway protocol frame. Higher-level packages
// interpret method- and event-specific payloads.
type Frame map[string]any

func (frame Frame) String(key string) string {
	value, _ := frame[key].(string)
	return value
}

func (frame Frame) Bool(key string) bool {
	value, _ := frame[key].(bool)
	return value
}

func (frame Frame) Map(key string) map[string]any {
	return Map(frame[key])
}

// Map returns value as a JSON object, or an empty object otherwise.
func Map(value any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	if typed, ok := value.(Frame); ok {
		return map[string]any(typed)
	}
	return map[string]any{}
}

// StableString renders protocol-safe diagnostics. Callers must not use it for
// authentication frames, whose raw contents are deliberately transient.
func StableString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	content, err := json.Marshal(value)
	if err != nil {
		return "<unencodable>"
	}
	return string(content)
}
