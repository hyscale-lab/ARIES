package hermes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	extractKeyPath      = stateContainerPath + "/tavily.key"
	identityContainerFS = stagedRoot + "/ssh/id_ed25519"
	agentWrapperPath    = stagedRoot + "/run-agent"
	workspaceRoot       = stagedRoot + "/workspace"

	// tavilyAPIKeyEnv is the in-container environment variable name Hermes's
	// Tavily plugin reads. It is fixed by Hermes itself, unlike the profile's
	// (host-side) HarnessWebSearchConfig.ExtractAPIKeyEnv lookup name.
	tavilyAPIKeyEnv = "TAVILY_API_KEY"

	// searxngBaseURL matches the fixed network alias
	// (pkg/sandbox/docker/docker.go's `networkAlias = "task-sandbox"`) and
	// port (images/deep-research-bench/Dockerfile) that the DRB task
	// sandbox's built-in SearXNG instance is always reachable at from the
	// Hermes harness container, which joins the same per-task Docker
	// network. Mirrors pkg/harness/openclaw/config.go's constant of the same
	// name and value.
	searxngBaseURL = "http://task-sandbox:8888"
)

// hermesProvider maps the profile's runtime backend onto a provider name
// Hermes accepts. "deepseek" is a built-in Hermes provider. Neither pinned
// Hermes version knows "sglang" or a plain "openai" provider
// (hermes_cli/auth.py PROVIDER_REGISTRY), and the one-shot rejects an unknown
// name before any request is made. The generic "custom" provider is the one
// that routes to model.base_url with the configured key, so every
// OpenAI-compatible backend renders as "custom". The same value is passed to
// the one-shot as --provider, so the wrapper and the config never disagree.
func hermesProvider(backend string) string {
	if openAICompatible(backend) {
		return "custom"
	}
	return backend
}

// openAICompatible reports whether the backend is a generic OpenAI-compatible
// server whose base URL must be the versioned /v1 prefix.
func openAICompatible(provider string) bool {
	return provider == "sglang" || provider == "openai"
}

// CompactionSettings is the harness-side copy of the profile's
// harness.compaction block. See pkg/config.HarnessCompactionConfig.
type CompactionSettings struct {
	Enabled         *bool
	ThresholdTokens int
}

// renderSettings are the inputs to renderConfig beyond the model. extraBody
// is the profile's opaque JSON object, or nil.
type renderSettings struct {
	maxTurns               int
	webSearchEnabled       bool
	extractEnabled         bool
	subagentsEnabled       bool
	maxConcurrentSubagents int
	compaction             *CompactionSettings
	extraBody              []byte
}

// renderConfig produces the Hermes `config.yaml`. The credential is written as
// a ${NAME} reference rather than a value: Hermes expands those from the process
// environment (hermes_cli/config.py::_expand_env_vars), and the wrapper script
// exports the name from a private staged key file, so no credential ever reaches
// the rendered config, Docker metadata, or results.
//
// Terminal settings are deliberately absent. Hermes resolves its backend from
// environment variables only (tools/terminal_tool.py::_get_env_config), so the
// SSH target is supplied through containerEnvironment below.
//
// Optional blocks are written only when the profile set them, so a profile
// without them renders the same file as before they existed:
//
//   - model.context_length, model.max_tokens, model.temperature feed Hermes's
//     compressor window arithmetic and its request sampling.
//   - compression.enabled / compression.threshold_tokens set the compaction
//     trigger. threshold_tokens is an absolute cap Hermes applies after its
//     64K minimum and its 75% small-window floor.
//   - custom_providers[0].extra_body is the profile's opaque JSON object.
//     Hermes matches the entry by base_url and merges the object into every
//     chat request; that merge happens only for provider "custom" (see
//     hermesProvider), so the block is refused under DeepSeek.
func renderConfig(model core.ModelConfig, settings renderSettings) ([]byte, error) {
	if err := validateModel(model); err != nil {
		return nil, err
	}
	if err := validateGeneration(model); err != nil {
		return nil, err
	}
	if openAICompatible(model.Provider) {
		normalized, err := normalizeV1BaseURL(model.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Hermes %s base URL: %w", model.Provider, err)
		}
		model.BaseURL = normalized
	}
	if settings.maxTurns <= 0 {
		return nil, errors.New("Hermes max turns must be positive")
	}
	if settings.compaction != nil {
		if settings.compaction.ThresholdTokens < 0 {
			return nil, errors.New("Hermes compaction threshold must be positive")
		}
		if model.ContextLength > 0 && settings.compaction.ThresholdTokens >= model.ContextLength {
			return nil, errors.New("Hermes compaction threshold must be smaller than the context length")
		}
	}
	var extraBody string
	if len(settings.extraBody) != 0 {
		if hermesProvider(model.Provider) != "custom" {
			return nil, errors.New("Hermes merges extra_body only for the custom provider, not " + model.Provider)
		}
		indented, err := indentedJSONObject(settings.extraBody, "      ")
		if err != nil {
			return nil, fmt.Errorf("Hermes extra_body: %w", err)
		}
		extraBody = indented
	}
	var output bytes.Buffer
	output.WriteString("model:\n")
	output.WriteString("  default: " + yamlString(model.Model) + "\n")
	output.WriteString("  provider: " + yamlString(hermesProvider(model.Provider)) + "\n")
	output.WriteString("  base_url: " + yamlString(model.BaseURL) + "\n")
	output.WriteString("  api_key: " + yamlString("${"+model.APIKeyEnv+"}") + "\n")
	output.WriteString("  api_mode: \"chat_completions\"\n")
	if model.ContextLength > 0 {
		output.WriteString("  context_length: " + strconv.Itoa(model.ContextLength) + "\n")
	}
	if model.MaxTokens > 0 {
		output.WriteString("  max_tokens: " + strconv.Itoa(model.MaxTokens) + "\n")
	}
	if model.Temperature != nil {
		output.WriteString("  temperature: " + yamlFloat(*model.Temperature) + "\n")
	}
	output.WriteString("\nagent:\n")
	output.WriteString("  max_turns: " + strconv.Itoa(settings.maxTurns) + "\n")
	if settings.compaction != nil {
		output.WriteString("\ncompression:\n")
		if settings.compaction.Enabled != nil {
			output.WriteString("  enabled: " + strconv.FormatBool(*settings.compaction.Enabled) + "\n")
		}
		if settings.compaction.ThresholdTokens > 0 {
			output.WriteString("  threshold_tokens: " + strconv.Itoa(settings.compaction.ThresholdTokens) + "\n")
		}
	}
	if extraBody != "" {
		// One entry, matched by base_url only (no model key), so it acts as
		// the base_url fallback in Hermes's lookup and never shadows the
		// credential or route already set in the model block above. YAML is
		// a superset of JSON, so the object is written as an indented JSON
		// flow mapping; Hermes then expands the ${ARIES_*} references inside
		// it from the container environment (see containerEnvironment).
		output.WriteString("\ncustom_providers:\n")
		output.WriteString("  - name: \"aries\"\n")
		output.WriteString("    base_url: " + yamlString(model.BaseURL) + "\n")
		output.WriteString("    extra_body: " + extraBody + "\n")
	}
	if !settings.subagentsEnabled {
		// delegation is Hermes's delegate_task toolset. Unlike
		// platform_toolsets (an allowlist of toolset categories), delegation
		// isn't gated by it and is on by default, so disabling it requires
		// this separate top-level key.
		output.WriteString("\ndisabled_toolsets:\n")
		output.WriteString("  - delegation\n")
	} else if settings.maxConcurrentSubagents > 0 {
		output.WriteString("\ndelegation:\n")
		output.WriteString("  max_concurrent_children: " + strconv.Itoa(settings.maxConcurrentSubagents) + "\n")
	}
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
	if settings.webSearchEnabled {
		output.WriteString("    - web\n")
		// search_backend (not backend) is deliberate: the DRB task sandbox's
		// SearXNG instance is search-only. extract_backend is only added when
		// a Tavily key is staged (extractEnabled); otherwise a web_extract
		// call fails with Hermes's own explicit "no extract backend" error
		// rather than an ambiguous one.
		output.WriteString("\nweb:\n")
		output.WriteString("  search_backend: \"searxng\"\n")
		if settings.extractEnabled {
			output.WriteString("  extract_backend: \"tavily\"\n")
		}
	}
	return output.Bytes(), nil
}

// validateGeneration mirrors pkg/config's checks so a caller that bypasses the
// profile loader cannot render an unusable window.
func validateGeneration(model core.ModelConfig) error {
	if model.ContextLength < 0 || model.MaxTokens < 0 {
		return errors.New("Hermes context length and max tokens must be positive")
	}
	if model.ContextLength > 0 && model.MaxTokens > 0 && model.MaxTokens >= model.ContextLength {
		return errors.New("Hermes max tokens must be smaller than the context length")
	}
	if model.Temperature != nil {
		t := *model.Temperature
		if math.IsNaN(t) || math.IsInf(t, 0) || t < 0 || t > 2 {
			return errors.New("Hermes temperature must be between 0 and 2")
		}
	}
	return nil
}

// indentedJSONObject checks that raw is a non-empty JSON object and re-indents
// it so every continuation line sits under the YAML key that owns it. The
// bytes are re-indented, not re-encoded, so key order and number lexemes stay
// exactly as the profile wrote them.
func indentedJSONObject(raw []byte, prefix string) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return "", errors.New("must be a non-empty JSON object")
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, prefix, "  "); err != nil {
		return "", err
	}
	return indented.String(), nil
}

// yamlFloat writes a finite float as a YAML float scalar. A whole number keeps
// a trailing ".0" so YAML does not read it as an integer.
func yamlFloat(value float64) string {
	text := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.ContainsAny(text, ".eE") {
		text += ".0"
	}
	return text
}

// containerEnvironment is the non-secret environment given to the Hermes
// container. Hermes reads its terminal backend entirely from these names.
//
// ARIES_RUN_ID and ARIES_TASK_ID identify the task occurrence. Hermes expands
// ${NAME} references in every configuration string from its process
// environment, so a profile's extra_body can carry a per-task value, such as
// a per-task tag, without ARIES interpreting the block.
//
// TERMINAL_CWD is an ARIES-owned path that deliberately does not exist in any
// task image. The bridge is authoritative for the working directory: it runs
// every command in the sandbox's own workdir. Hermes opens its session with
// `cd <TERMINAL_CWD> 2>/dev/null || true` followed by `pwd -P`, so a path it
// cannot enter makes it adopt the workdir the bridge chose. Naming a real
// sandbox path here is impossible in any case — the harness never learns it.
func containerEnvironment(endpoint core.ToolEndpoint, workdir string, terminalTimeout int, webSearchEnabled bool, runID, taskID string) ([]string, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if err := validateRunID(runID); err != nil {
		return nil, err
	}
	if err := validateTaskID(taskID); err != nil {
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
	environment := []string{
		"HERMES_HOME=" + stateContainerPath,
		"TERMINAL_ENV=ssh",
		"TERMINAL_SSH_HOST=" + host,
		"TERMINAL_SSH_PORT=" + port,
		"TERMINAL_SSH_USER=" + endpoint.Username,
		"TERMINAL_SSH_KEY=" + identityContainerFS,
		"TERMINAL_CWD=" + workdir,
		"TERMINAL_TIMEOUT=" + strconv.Itoa(terminalTimeout),
		"ARIES_RUN_ID=" + runID,
		"ARIES_TASK_ID=" + taskID,
		// The v2026.8 image ships HERMES_WRITE_SAFE_ROOT=/opt/data, which makes
		// write_file and patch refuse every path outside that directory. The
		// tools act on the sandbox over SSH, and the sandbox is the isolation
		// boundary, so the prefix check only denies the agent its own
		// workspace. An empty value turns the check off (agent/file_safety.py).
		"HERMES_WRITE_SAFE_ROOT=",
	}
	if webSearchEnabled {
		environment = append(environment, "SEARXNG_URL="+searxngBaseURL)
	}
	return environment, nil
}

// agentWrapperScript exports the staged credential(s) under their required
// names and replaces itself with the Hermes one-shot. Keeping the exports
// inside the container means no value ever appears in Docker's exec or
// container config. extractEnabled additionally exports the Tavily key
// staged at extractKeyPath, under Hermes's fixed tavilyAPIKeyEnv name.
func agentWrapperScript(apiKeyEnv string, extractEnabled bool) []byte {
	script := `#!/bin/sh
set -eu
if [ ! -f ` + modelKeyPath + ` ]; then
  echo "ARIES: Hermes model key is missing" >&2
  exit 1
fi
` + apiKeyEnv + `="$(cat ` + modelKeyPath + `)"
export ` + apiKeyEnv + `
`
	if extractEnabled {
		script += `if [ ! -f ` + extractKeyPath + ` ]; then
  echo "ARIES: Hermes extract API key is missing" >&2
  exit 1
fi
` + tavilyAPIKeyEnv + `="$(cat ` + extractKeyPath + `)"
export ` + tavilyAPIKeyEnv + `
`
	}
	script += `exec hermes --ignore-rules --yolo --model "$1" --provider "$2" -z "$3"
`
	return []byte(script)
}

func validateModel(model core.ModelConfig) error {
	if model.Provider != "deepseek" && !openAICompatible(model.Provider) {
		return errors.New("Hermes model provider must be deepseek, sglang, or openai")
	}
	if openAICompatible(model.Provider) {
		if _, err := normalizeV1BaseURL(model.BaseURL); err != nil {
			return fmt.Errorf("Hermes %s base URL: %w", model.Provider, err)
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

func normalizeV1BaseURL(baseURL string) (string, error) {
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
