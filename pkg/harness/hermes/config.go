package hermes

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	stagedRoot          = "/run/aries"
	stateContainerPath  = stagedRoot + "/hermes"
	configContainerPath = stateContainerPath + "/config.yaml"
	modelKeyPath        = stateContainerPath + "/model.key"
	identityContainerFS = stagedRoot + "/ssh/id_ed25519"
	agentWrapperPath    = stagedRoot + "/run-agent"
	workspaceRoot       = stagedRoot + "/workspace"
)

// renderConfig produces the Hermes `config.yaml`. The credential is written as
// a ${NAME} reference rather than a value: Hermes expands those from the process
// environment (hermes_cli/config.py::_expand_env_vars), and the wrapper script
// exports the name from a private staged key file, so no credential ever reaches
// the rendered config, Docker metadata, or results.
//
// Terminal settings are deliberately absent. Hermes resolves its backend from
// environment variables only (tools/terminal_tool.py::_get_env_config), so the
// SSH target is supplied through containerEnvironment below.
func renderConfig(model core.ModelConfig, maxTurns int) ([]byte, error) {
	if err := validateModel(model); err != nil {
		return nil, err
	}
	if model.Provider == "sglang" {
		normalized, err := normalizeSGLangBaseURL(model.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Hermes SGLang base URL: %w", err)
		}
		model.BaseURL = normalized
	}
	if maxTurns <= 0 {
		return nil, errors.New("Hermes max turns must be positive")
	}
	var output bytes.Buffer
	output.WriteString("model:\n")
	output.WriteString("  default: " + yamlString(model.Model) + "\n")
	output.WriteString("  provider: " + yamlString(model.Provider) + "\n")
	output.WriteString("  base_url: " + yamlString(model.BaseURL) + "\n")
	output.WriteString("  api_key: " + yamlString("${"+model.APIKeyEnv+"}") + "\n")
	output.WriteString("  api_mode: \"chat_completions\"\n")
	output.WriteString("\nagent:\n")
	output.WriteString("  max_turns: " + strconv.Itoa(maxTurns) + "\n")
	output.WriteString("\ndisplay:\n")
	output.WriteString("  streaming: false\n")
	output.WriteString("  compact: true\n")
	// Toolsets are configured here rather than through `--toolsets`, which is
	// unusable on the pinned version: _validate_explicit_toolsets can fall off
	// its final branch and return a bare None, and the caller unpacks it, so
	// the agent exits 1 before doing any work.
	output.WriteString("\nplatform_toolsets:\n")
	output.WriteString("  cli:\n")
	output.WriteString("    - terminal\n")
	output.WriteString("    - file\n")
	output.WriteString("    - code_execution\n")
	return output.Bytes(), nil
}

// containerEnvironment is the non-secret environment given to the Hermes
// container. Hermes reads its terminal backend entirely from these names.
//
// TERMINAL_CWD is an ARIES-owned path that deliberately does not exist in any
// task image. The bridge is authoritative for the working directory: it runs
// every command in the sandbox's own workdir. Hermes opens its session with
// `cd <TERMINAL_CWD> 2>/dev/null || true` followed by `pwd -P`, so a path it
// cannot enter makes it adopt the workdir the bridge chose. Naming a real
// sandbox path here is impossible in any case — the harness never learns it.
func containerEnvironment(endpoint core.ToolEndpoint, workdir string, terminalTimeout int) ([]string, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if !validWorkdir(workdir) {
		return nil, fmt.Errorf("Hermes terminal workdir %q is not shell-neutral", workdir)
	}
	if terminalTimeout <= 0 {
		return nil, errors.New("Hermes terminal timeout must be positive")
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil {
		return nil, fmt.Errorf("parse Hermes SSH endpoint address: %w", err)
	}
	if _, err := strconv.Atoi(port); err != nil {
		return nil, errors.New("Hermes SSH endpoint port is invalid")
	}
	return []string{
		"HERMES_HOME=" + stateContainerPath,
		"TERMINAL_ENV=ssh",
		"TERMINAL_SSH_HOST=" + host,
		"TERMINAL_SSH_PORT=" + port,
		"TERMINAL_SSH_USER=" + endpoint.Username,
		"TERMINAL_SSH_KEY=" + identityContainerFS,
		"TERMINAL_CWD=" + workdir,
		"TERMINAL_TIMEOUT=" + strconv.Itoa(terminalTimeout),
	}, nil
}

// agentWrapperScript exports the staged credential under the configured name
// and replaces itself with the Hermes one-shot. Keeping the export inside the
// container means the value never appears in Docker's exec or container config.
func agentWrapperScript(apiKeyEnv string) []byte {
	return []byte(`#!/bin/sh
set -eu
if [ ! -f ` + modelKeyPath + ` ]; then
  echo "ARIES: Hermes model key is missing" >&2
  exit 1
fi
` + apiKeyEnv + `="$(cat ` + modelKeyPath + `)"
export ` + apiKeyEnv + `
exec hermes --ignore-rules --yolo --model "$1" --provider "$2" -z "$3"
`)
}

func validateModel(model core.ModelConfig) error {
	if model.Provider != "deepseek" && model.Provider != "sglang" {
		return errors.New("Hermes model provider must be deepseek or sglang")
	}
	if model.Provider == "sglang" {
		if _, err := normalizeSGLangBaseURL(model.BaseURL); err != nil {
			return fmt.Errorf("Hermes SGLang base URL: %w", err)
		}
	} else {
		parsed, err := url.Parse(model.BaseURL)
		if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return errors.New("Hermes model base URL must be absolute HTTP(S)")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("Hermes model base URL must not contain credentials, query, or fragment")
		}
	}
	// yamlString would render a control character as a numeric escape rather
	// than break the document, but a model ID containing one is a configuration
	// error. Rejecting it here fails immediately instead of at the readiness
	// timeout, with an error that names the cause.
	if strings.TrimSpace(model.Model) == "" || strings.ContainsFunc(model.Model, unicode.IsControl) {
		return errors.New("Hermes model ID is invalid")
	}
	if !validEnvironmentName(model.APIKeyEnv) {
		return errors.New("Hermes API-key environment name is invalid")
	}
	return nil
}

func normalizeSGLangBaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.Opaque != "" || parsed.User != nil || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(baseURL, "#") {
		return "", errors.New("must be an absolute HTTP(S) URL without credentials, escaped path, query, or fragment")
	}
	if parsed.Path != "/v1" && parsed.Path != "/v1/" {
		return "", errors.New("path must be exactly /v1")
	}
	parsed.Path = "/v1"
	return parsed.String(), nil
}

func validateEndpoint(endpoint core.ToolEndpoint) error {
	if endpoint.Protocol != "ssh" || endpoint.Username != "aries" || strings.TrimSpace(endpoint.Network) == "" {
		return errors.New("Hermes requires a task-local SSH endpoint")
	}
	if strings.TrimSpace(endpoint.IdentitySourceFile) == "" {
		return errors.New("Hermes requires a staged SSH identity file")
	}
	// Hermes builds its own ssh argv and offers no way to preload a known-hosts
	// file, so a bridge-supplied one would be silently ignored. Refuse rather
	// than imply a host-key guarantee the harness cannot honour.
	if endpoint.ClientCommand != "" || endpoint.ClientSourceFile != "" {
		return errors.New("Hermes uses its own SSH client and accepts no bridge client command")
	}
	return nil
}

func validEnvironmentName(name string) bool {
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return name != ""
}

// validWorkdir mirrors the bridge's rule so the value written into
// TERMINAL_CWD cannot change meaning inside a shell.
func validWorkdir(value string) bool {
	if value == "/" {
		return true
	}
	if len(value) < 2 || value[0] != '/' || value[len(value)-1] == '/' {
		return false
	}
	for _, component := range strings.Split(value[1:], "/") {
		if component == "" || component == "." || component == ".." {
			return false
		}
		for _, character := range component {
			if character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
				continue
			}
			return false
		}
	}
	return true
}

// yamlString emits a double-quoted YAML scalar. Every character that could
// terminate the scalar or be read as a line break is escaped, so no rendered
// value can restructure the document: the quote and backslash, the three
// whitespace controls with short forms, and any remaining control character or
// Unicode line/paragraph separator as a numeric escape.
func yamlString(value string) string {
	var output strings.Builder
	output.WriteByte('"')
	for _, character := range value {
		switch {
		case character == '"':
			output.WriteString(`\"`)
		case character == '\\':
			output.WriteString(`\\`)
		case character == '\n':
			output.WriteString(`\n`)
		case character == '\r':
			output.WriteString(`\r`)
		case character == '\t':
			output.WriteString(`\t`)
		case character == '\u2028':
			output.WriteString(`\L`)
		case character == '\u2029':
			output.WriteString(`\P`)
		case unicode.IsControl(character):
			// IsControl covers C0, DEL, and C1; all fit the two-digit form.
			fmt.Fprintf(&output, `\x%02X`, character)
		default:
			output.WriteRune(character)
		}
	}
	output.WriteByte('"')
	return output.String()
}
