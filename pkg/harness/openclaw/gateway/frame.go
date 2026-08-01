package gateway

import (
	"fmt"
	"strings"
)

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

// ResponseError reports only bounded protocol metadata. It deliberately never
// renders the response because error details can contain credentials or echoed
// request parameters.
func ResponseError(method string, response Frame) error {
	errorPayload := response.Map("error")
	code := boundedDiagnostic(errorPayload["code"])
	message := boundedDiagnostic(errorPayload["message"])
	if code == "" {
		code = "UNKNOWN"
	}
	if message == "" {
		message = "request rejected"
	}
	return fmt.Errorf("gateway %s rejected request (%s): %s", strings.TrimSpace(method), code, message)
}
