package harness

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Options contains configuration inputs shared by native harnesses. Each
// harness applies its own defaults, supported modes, and rendering policy.
type Options struct {
	Mode                   string
	WebSearchEnabled       bool
	ExtractAPIKeyEnv       string
	SubagentsEnabled       bool
	MaxConcurrentSubagents int
	MCPServers             []core.MCPServerConfig
	// RedactEnv names host environment variables whose values the harness is
	// never given but scrubs from what it saves: a benchmark's credentials
	// that reach the sandbox, where the agent can read them and repeat them
	// (Toolathlon's account tokens). They are read through the API-key lookup
	// at Start; an unset variable is skipped.
	RedactEnv []string
}

// TTSOptions are the common speech synthesis inputs. Native transcription and
// realtime session settings remain owned by each harness.
type TTSOptions struct {
	Provider     string
	BaseURL      string
	APIKeyEnv    string
	Model        string
	Voice        string
	Instructions string
	Speed        *float64
	Timeout      time.Duration
}

// ValidateSearch checks a resolved service endpoint when search is requested.
// Disabled search does not require the task to provide a search service.
func ValidateSearch(searchURL string, enabled bool) error {
	if !enabled {
		return nil
	}
	parsed, err := url.Parse(searchURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.IndexFunc(searchURL, unicode.IsControl) >= 0 {
		return errors.New("web search requires a resolved HTTP(S) task search endpoint without credentials")
	}
	return nil
}

// ValidateModel checks model settings shared by native harnesses.
func ValidateModel(label string, model core.ModelConfig) error {
	if model.Provider != "deepseek" && !OpenAICompatible(model.Provider) {
		return fmt.Errorf("%s model provider must be deepseek, sglang, or openai", label)
	}
	if OpenAICompatible(model.Provider) {
		if _, err := NormalizeV1BaseURL(model.BaseURL); err != nil {
			return fmt.Errorf("%s %s base URL: %w", label, model.Provider, err)
		}
	} else {
		parsed, err := url.Parse(model.BaseURL)
		if err != nil || parsed.Hostname() == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("%s model base URL must be absolute HTTP(S)", label)
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s model base URL must not contain credentials, query, or fragment", label)
		}
	}
	if strings.TrimSpace(model.Model) == "" || strings.ContainsFunc(model.Model, unicode.IsControl) {
		return fmt.Errorf("%s model ID is invalid", label)
	}
	if !ValidEnvironmentName(model.APIKeyEnv) {
		return fmt.Errorf("%s API-key environment name is invalid", label)
	}
	return nil
}

// OpenAICompatible identifies supported generic OpenAI-compatible backends.
func OpenAICompatible(provider string) bool {
	return provider == "sglang" || provider == "openai"
}

// NormalizeV1BaseURL requires a versioned endpoint and normalizes its trailing slash.
func NormalizeV1BaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(baseURL, "#") {
		return "", errors.New("must be an absolute HTTP(S) URL without credentials, escaped path, query, or fragment")
	}
	if parsed.Path != "/v1" && parsed.Path != "/v1/" {
		return "", errors.New("path must be exactly /v1")
	}
	parsed.Path = "/v1"
	return parsed.String(), nil
}

// ValidEnvironmentName checks names interpolated into native launcher scripts.
func ValidEnvironmentName(value string) bool {
	for index, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

// ValidateRunID checks identifiers used in runtime labels and artifact paths.
func ValidateRunID(label, value string) error {
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s run ID must contain 1 to 128 safe characters", label)
	}
	if !safeIdentifierChars(value) {
		return fmt.Errorf("%s run ID contains an unsafe character", label)
	}
	return nil
}

// ValidateTaskID checks the occurrence identifier used in runtime ownership.
func ValidateTaskID(label, value string) error {
	if value == "" || len(value) > 149 || !safeIdentifierChars(value) {
		return fmt.Errorf("%s task ID is invalid", label)
	}
	return nil
}

func safeIdentifierChars(value string) bool {
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return false
	}
	return true
}
