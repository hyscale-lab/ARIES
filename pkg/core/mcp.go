package core

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

// MCPServerConfig defines the configuration for an external or in-harness MCP server.
type MCPServerConfig struct {
	Name      string            `json:"name"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`
}

// ValidateMCPServer verifies that an MCP server configuration specifies a valid name,
// benign plain text env or valid host environment variable names for secret_env, and specifies
// either a validated executable command or an absolute HTTP(S) URL.
func ValidateMCPServer(cfg MCPServerConfig) error {
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		return errors.New("MCP server name is required")
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' {
			continue
		}
		return fmt.Errorf("MCP server name %q contains invalid characters (must be alphanumeric, '-', or '_')", name)
	}

	for envKey, envVal := range cfg.Env {
		if !validEnvName(envKey) {
			return fmt.Errorf("MCP server %q env key %q is not a valid environment variable name", name, envKey)
		}
		if strings.ContainsFunc(envVal, unicode.IsControl) {
			return fmt.Errorf("MCP server %q env value for %q contains invalid control characters", name, envKey)
		}
	}

	for secretKey, hostVar := range cfg.SecretEnv {
		if !validEnvName(secretKey) {
			return fmt.Errorf("MCP server %q secret_env key %q is not a valid environment variable name", name, secretKey)
		}
		if !validEnvName(hostVar) {
			return fmt.Errorf("MCP server %q secret_env value %q for %q is not a valid host environment variable name", name, hostVar, secretKey)
		}
		if _, exists := cfg.Env[secretKey]; exists {
			return fmt.Errorf("MCP server %q key %q cannot be specified in both env and secret_env", name, secretKey)
		}
	}

	hasCommand := strings.TrimSpace(cfg.Command) != ""
	hasURL := strings.TrimSpace(cfg.URL) != ""

	if !hasCommand && !hasURL {
		return fmt.Errorf("MCP server %q must specify either command or url", name)
	}
	if hasCommand && hasURL {
		return fmt.Errorf("MCP server %q cannot specify both command and url", name)
	}

	if hasURL && len(cfg.Env) > 0 {
		return fmt.Errorf("MCP server %q env is not supported for url servers", name)
	}
	if hasURL && len(cfg.SecretEnv) > 0 {
		return fmt.Errorf("MCP server %q secret_env is not supported for url servers", name)
	}

	if hasCommand {
		cmd := strings.TrimSpace(cfg.Command)
		if strings.ContainsFunc(cmd, unicode.IsControl) {
			return fmt.Errorf("MCP server %q command contains invalid control characters", name)
		}
		for _, arg := range cfg.Args {
			if strings.ContainsFunc(arg, unicode.IsControl) {
				return fmt.Errorf("MCP server %q argument contains invalid control characters", name)
			}
		}
	}

	if hasURL {
		parsed, err := url.Parse(cfg.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("MCP server %q url must be absolute HTTP(S)", name)
		}
	}

	return nil
}

func validEnvName(value string) bool {
	for index, character := range value {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return value != ""
}
