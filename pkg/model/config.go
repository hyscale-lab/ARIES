// Package model contains shared model request settings used by harnesses and evaluators.
package model

import (
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
	"math"
)

// ValidateGeneration checks optional numeric model settings, independently of the consumer.
func ValidateGeneration(model core.ModelConfig) error {
	if model.ContextLength < 0 {
		return errors.New("model.context_length must be positive")
	}
	if model.MaxTokens < 0 {
		return errors.New("model.max_tokens must be positive")
	}
	if model.ContextLength > 0 && model.MaxTokens > 0 && model.MaxTokens >= model.ContextLength {
		return errors.New("model.max_tokens must be smaller than model.context_length")
	}
	if model.Temperature != nil {
		t := *model.Temperature
		if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 || t > 2 {
			return errors.New("model.temperature must be between 0 and 2")
		}
	}
	return nil
}

// ReasoningBody translates an explicit effort into Chat Completions fields.
// Endpoint/model support is provider-owned; omitted settings preserve defaults.
func ReasoningBody(model core.ModelConfig) (map[string]any, error) {
	effort := model.ReasoningEffort
	if effort == "" {
		return nil, nil
	}
	if effort == "off" {
		effort = "none"
	}
	if model.Provider == "deepseek" {
		if effort == "none" {
			return map[string]any{"thinking": map[string]any{"type": "disabled"}}, nil
		}
		switch effort {
		case "low", "high", "max":
			return map[string]any{"thinking": map[string]any{"type": "enabled"}, "reasoning_effort": effort}, nil
		default:
			return nil, errors.New("model.reasoning_effort for DeepSeek must be off, none, low, high, or max")
		}
	}
	if model.Provider != "openai" && model.Provider != "sglang" {
		return nil, errors.New("model.reasoning_effort requires deepseek, openai, or sglang")
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return map[string]any{"reasoning_effort": effort}, nil
	default:
		return nil, errors.New("model.reasoning_effort must be off, none, minimal, low, medium, high, xhigh, or max")
	}
}
