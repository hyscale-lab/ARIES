package core

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"unicode"
)

// MCPServerConfig defines the configuration for an external or in-harness MCP server.
type MCPServerConfig struct {
	Name    string   `json:"name"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	URL     string   `json:"url,omitempty"`
	// Transport is how a URL server is reached, "sse" or "streamable-http".
	// Empty leaves the choice to the harness's own default, which differs
	// between harnesses, so a server that speaks only one names it.
	Transport string `json:"transport,omitempty"`
	// TimeoutSeconds bounds one tool call to this server; zero keeps the
	// harness's own default.
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	SecretEnv      map[string]string `json:"secret_env,omitempty"`
	// Cwd is a command server's absolute working directory in the harness
	// runtime; empty keeps the harness's default.
	Cwd string `json:"cwd,omitempty"`
	// Headers are fixed, non-secret HTTP headers for a url server. Credentials
	// never travel here: a header named like one is rejected.
	Headers map[string]string `json:"headers,omitempty"`
}

// credentialHeaders are header names, lowercased, that carry credentials.
var credentialHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "api-key": true, "x-auth-token": true,
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
	switch cfg.Transport {
	case "", "sse", "streamable-http":
	default:
		return fmt.Errorf("MCP server %q transport must be sse or streamable-http", name)
	}
	if cfg.Transport != "" && !hasURL {
		return fmt.Errorf("MCP server %q transport applies only to a url server", name)
	}
	if cfg.TimeoutSeconds < 0 {
		return fmt.Errorf("MCP server %q timeout_seconds must not be negative", name)
	}

	if hasURL && len(cfg.Env) > 0 {
		return fmt.Errorf("MCP server %q env is not supported for url servers", name)
	}
	if hasURL && len(cfg.SecretEnv) > 0 {
		return fmt.Errorf("MCP server %q secret_env is not supported for url servers", name)
	}
	if cfg.Cwd != "" {
		if !hasCommand {
			return fmt.Errorf("MCP server %q cwd applies only to a command server", name)
		}
		if !strings.HasPrefix(cfg.Cwd, "/") || path.Clean(cfg.Cwd) != cfg.Cwd || strings.ContainsFunc(cfg.Cwd, unicode.IsControl) {
			return fmt.Errorf("MCP server %q cwd must be an absolute, clean path", name)
		}
	}
	if len(cfg.Headers) > 0 && !hasURL {
		return fmt.Errorf("MCP server %q headers apply only to a url server", name)
	}
	seenHeaders := make(map[string]bool, len(cfg.Headers))
	for header, value := range cfg.Headers {
		lower := strings.ToLower(header)
		if header == "" || strings.IndexFunc(header, func(r rune) bool { return !headerTokenRune(r) }) >= 0 {
			return fmt.Errorf("MCP server %q header name %q is invalid", name, header)
		}
		if seenHeaders[lower] {
			return fmt.Errorf("MCP server %q header %q appears more than once", name, header)
		}
		seenHeaders[lower] = true
		if credentialHeaders[lower] {
			return fmt.Errorf("MCP server %q header %q carries a credential; headers are not secret", name, header)
		}
		if strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("MCP server %q header %q value contains control characters", name, header)
		}
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

// ValidPluginName reports whether name satisfies the Agent Plugins 1.0.0
// manifest name constraints: 1-64 lowercase letters, digits, hyphens or
// periods, starting and ending alphanumeric, with no "--" or "..".
func ValidPluginName(name string) bool {
	if name == "" || len(name) > 64 || strings.Contains(name, "--") || strings.Contains(name, "..") {
		return false
	}
	for index, character := range name {
		alphanumeric := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if !alphanumeric && (index == 0 || index == len(name)-1 || character != '-' && character != '.') {
			return false
		}
	}
	return true
}

// headerTokenRune reports whether r may appear in an HTTP header name (RFC 9110 token).
func headerTokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)
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
