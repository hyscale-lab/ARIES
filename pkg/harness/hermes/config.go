package hermes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/harness"
	modelconfig "github.com/hyscale-lab/aries/pkg/model"
)

const (
	stagedRoot          = "/run/aries"
	stateContainerPath  = stagedRoot + "/hermes"
	configContainerPath = stateContainerPath + "/config.yaml"
	modelKeyPath        = stateContainerPath + "/model.key"
	extractKeyPath      = stateContainerPath + "/tavily.key"
	voiceKeyPath        = stateContainerPath + "/voice.key"
	voiceWAVPath        = stateContainerPath + "/voice-instruction.wav"
	gatewayLauncherPath = stagedRoot + "/run-gateway"
	workspaceRoot       = stagedRoot + "/workspace"

	// tavilyAPIKeyEnv is the in-container environment variable name Hermes's
	// Tavily plugin reads. It is fixed by Hermes itself, unlike the profile's
	// (host-side) HarnessWebSearchConfig.ExtractAPIKeyEnv lookup name.
	tavilyAPIKeyEnv = "TAVILY_API_KEY"
	// otelPluginName is the hermes-otel plugin that LocalImage adds to the
	// pinned Hermes image. It writes its spans to otelStorePath.
	otelPluginName = "hermes_otel"
	otelStorePath  = stateContainerPath + "/hermes_otel_live.db"
	otelMaxSpans   = 10000
)

// hermesProvider maps the profile backend onto the pinned Hermes provider
// registry. DeepSeek is built in; generic OpenAI-compatible endpoints use
// "custom" so the Gateway resolves the configured base URL and credential.
func hermesProvider(backend string) string {
	if harness.OpenAICompatible(backend) {
		return "custom"
	}
	return backend
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
	mcpServers             []core.MCPServerConfig
}

// renderConfig produces the Hermes `config.yaml`. The credential is written as
// a ${NAME} reference rather than a value: Hermes expands those from the process
// environment (hermes_cli/config.py::_expand_env_vars), and the Gateway launcher
// exports the name from a private staged key file, so no credential ever reaches
// the rendered config, Docker metadata, or results.
//
// The terminal section is rendered separately (renderTerminal) because it names
// the sandbox's workdir, which comes from the bridge's endpoint rather than the
// profile; the SSH target itself is supplied through containerEnvironment.
//
// Optional blocks are written only when the profile set them, so a profile
// without them renders the same file as before they existed:
//
//   - model.context_length and model.max_tokens feed Hermes's compressor
//     window arithmetic and output budget. Temperature is merged into the
//     custom provider extra_body to reach the native request-override path.
//   - compression.enabled / compression.threshold_tokens set the compaction
//     trigger. threshold_tokens is an absolute cap Hermes applies after its
//     64K minimum and its 75% small-window floor.
//   - custom_providers[0].extra_body is the profile's opaque JSON object.
//     Hermes matches the entry by base_url and merges the object into every
//     chat request; that merge happens only for provider "custom" (see
//     hermesProvider). Explicit reasoning also uses this native custom route
//     because the pinned DeepSeek profile does not recognize deepseek-flash.
func renderConfig(model core.ModelConfig, settings renderSettings, voiceSTT *VoiceSTTOptions) ([]byte, error) {
	if err := validateModel(model); err != nil {
		return nil, err
	}
	if err := validateGeneration(model); err != nil {
		return nil, err
	}
	if harness.OpenAICompatible(model.Provider) {
		normalized, err := harness.NormalizeV1BaseURL(model.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Hermes %s base URL: %w", model.Provider, err)
		}
		model.BaseURL = normalized
	}
	// MCP source names are exported into the Gateway process. Child-only
	// target names are safe aliases and do not configure the parent service.
	for _, server := range settings.mcpServers {
		for _, hostVar := range server.SecretEnv {
			if strings.HasPrefix(hostVar, "API_SERVER_") {
				return nil, errors.New("Hermes MCP credential source uses reserved API_SERVER_ namespace")
			}
		}
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
	requestBody := settings.extraBody
	if len(requestBody) != 0 && model.Provider == "deepseek" {
		return nil, errors.New("Hermes merges user extra_body only for the custom provider, not deepseek")
	}
	provider := hermesProvider(model.Provider)
	reasoningBody, err := modelconfig.ReasoningBody(model)
	if err != nil {
		return nil, err
	}
	if reasoningBody != nil {
		object := make(map[string]json.RawMessage)
		if len(requestBody) != 0 {
			if err := json.Unmarshal(requestBody, &object); err != nil || len(object) == 0 {
				return nil, errors.New("Hermes extra_body must be a non-empty JSON object")
			}
		}
		for _, key := range []string{"reasoning_effort", "thinking", "reasoning"} {
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("Hermes model.reasoning_effort conflicts with extra_body.%s", key)
			}
		}
		for key, value := range reasoningBody {
			content, err := json.Marshal(value)
			if err != nil {
				return nil, fmt.Errorf("Hermes reasoning request body: %w", err)
			}
			object[key] = content
		}
		requestBody, err = json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("Hermes reasoning request body: %w", err)
		}
		// The pin's DeepSeek profile omits thinking controls for deepseek-flash.
		// Native custom-provider extras preserve the exact requested wire fields.
		provider = "custom"
	}
	if model.Temperature != nil {
		if !harness.OpenAICompatible(model.Provider) {
			return nil, errors.New("Hermes temperature requires the sglang or openai backend")
		}
		object := make(map[string]json.RawMessage)
		if len(requestBody) != 0 {
			if err := json.Unmarshal(requestBody, &object); err != nil || len(object) == 0 {
				return nil, errors.New("Hermes extra_body must be a non-empty JSON object")
			}
		}
		if _, exists := object["temperature"]; exists {
			return nil, errors.New("Hermes model.temperature conflicts with extra_body.temperature")
		}
		object["temperature"] = json.RawMessage(yamlFloat(*model.Temperature))
		var err error
		requestBody, err = json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("Hermes request body: %w", err)
		}
	}
	var extraBody string
	if len(requestBody) != 0 {
		if provider != "custom" {
			return nil, errors.New("Hermes merges extra_body only for the custom provider, not " + model.Provider)
		}
		indented, err := indentedJSONObject(requestBody, "      ")
		if err != nil {
			return nil, fmt.Errorf("Hermes extra_body: %w", err)
		}
		extraBody = indented
	}
	var output bytes.Buffer
	output.WriteString("model:\n")
	output.WriteString("  default: " + yamlString(model.Model) + "\n")
	output.WriteString("  provider: " + yamlString(provider) + "\n")
	output.WriteString("  base_url: " + yamlString(model.BaseURL) + "\n")
	output.WriteString("  api_key: " + yamlString("${"+model.APIKeyEnv+"}") + "\n")
	output.WriteString("  api_mode: \"chat_completions\"\n")
	if model.ContextLength > 0 {
		output.WriteString("  context_length: " + strconv.Itoa(model.ContextLength) + "\n")
	}
	if model.MaxTokens > 0 {
		output.WriteString("  max_tokens: " + strconv.Itoa(model.MaxTokens) + "\n")
	}
	output.WriteString("\nagent:\n")
	output.WriteString("  max_turns: " + strconv.Itoa(settings.maxTurns) + "\n")
	if !settings.subagentsEnabled {
		// The native API resolves this nested denylist after platform toolsets.
		output.WriteString("  disabled_toolsets:\n    - delegation\n")
	}
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
	if settings.subagentsEnabled && settings.maxConcurrentSubagents > 0 {
		output.WriteString("\ndelegation:\n")
		output.WriteString("  max_concurrent_children: " + strconv.Itoa(settings.maxConcurrentSubagents) + "\n")
	}
	// hermes_otel records each tool call, model call, and API request as a
	// span stamped when it starts and ends. Hermes's own session store stamps
	// messages when it saves the transcript on exit, so these spans are the
	// only per-call timing. Hermes loads only listed plugins; an image
	// without this one skips the name.
	output.WriteString("\nplugins:\n")
	output.WriteString("  enabled:\n")
	output.WriteString("    - " + otelPluginName + "\n")
	if voiceSTT != nil {
		output.WriteString("\nstt:\n")
		output.WriteString("  enabled: true\n")
		output.WriteString("  provider: " + yamlString(voiceSTT.Provider) + "\n")
		output.WriteString("  openai:\n")
		output.WriteString("    model: " + yamlString(voiceSTT.Model) + "\n")
		output.WriteString("  local:\n")
		output.WriteString("    model: " + yamlString(voiceSTT.Model) + "\n")
		output.WriteString("    language: " + yamlString(voiceSTT.Language) + "\n")
	}
	output.WriteString("\ndisplay:\n")
	output.WriteString("  streaming: false\n")
	output.WriteString("  compact: true\n")
	// Native API agent construction resolves this platform-specific toolset list.
	output.WriteString("\nplatform_toolsets:\n")
	output.WriteString("  api_server:\n")
	output.WriteString("    - terminal\n")
	output.WriteString("    - file\n")
	output.WriteString("    - code_execution\n")
	if settings.subagentsEnabled {
		output.WriteString("    - delegation\n")
	}
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
	if len(settings.mcpServers) > 0 {
		output.WriteString("\nmcp_servers:\n")
		for _, server := range settings.mcpServers {
			output.WriteString("  " + server.Name + ":\n")
			if server.URL != "" {
				output.WriteString("    url: " + yamlString(server.URL) + "\n")
			} else if server.Command != "" {
				output.WriteString("    command: " + yamlString(server.Command) + "\n")
				if len(server.Args) > 0 {
					output.WriteString("    args:\n")
					for _, arg := range server.Args {
						output.WriteString("      - " + yamlString(arg) + "\n")
					}
				}
				if len(server.Env) > 0 || len(server.SecretEnv) > 0 {
					output.WriteString("    env:\n")
					keys := make([]string, 0, len(server.Env)+len(server.SecretEnv))
					for k := range server.Env {
						keys = append(keys, k)
					}
					for k := range server.SecretEnv {
						keys = append(keys, k)
					}
					sort.Strings(keys)
					for _, k := range keys {
						if hostVar, ok := server.SecretEnv[k]; ok {
							output.WriteString("      " + k + ": " + yamlString("${"+hostVar+"}") + "\n")
						} else {
							output.WriteString("      " + k + ": " + yamlString(server.Env[k]) + "\n")
						}
					}
				}
			}
		}
	}
	return output.Bytes(), nil
}

// renderTerminal is the `terminal:` section appended to config.yaml. Hermes
// wraps every agent command in `builtin cd -- <cwd> || exit 126`, where <cwd>
// is the call's own workdir or else the configured one (tools/terminal_tool.py),
// so the configured directory must exist in the sandbox: it is the directory
// the bridge runs commands in. The file has to carry it, not only
// TERMINAL_CWD: Gateway runtime refresh treats config.yaml as authoritative,
// so backend, cwd, and timeout are supplied together in the terminal section.
func renderTerminal(workdir string, timeout int) ([]byte, error) {
	if workdir == "" {
		return nil, errors.New("Hermes SSH endpoint does not name the sandbox workdir")
	}
	if !validWorkdir(workdir) {
		return nil, fmt.Errorf("Hermes terminal workdir %q is not shell-neutral", workdir)
	}
	if timeout <= 0 {
		return nil, errors.New("Hermes terminal timeout must be positive")
	}
	var output bytes.Buffer
	output.WriteString("\nterminal:\n")
	output.WriteString("  backend: \"ssh\"\n")
	output.WriteString("  cwd: " + yamlString(workdir) + "\n")
	output.WriteString("  timeout: " + strconv.Itoa(timeout) + "\n")
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
// TERMINAL_CWD is the sandbox directory the bridge runs every agent command in
// (the endpoint's Workdir), the same one renderTerminal writes into
// config.yaml. Hermes opens its session with `cd <TERMINAL_CWD> 2>/dev/null ||
// true`, but it then wraps each command in `builtin cd -- <cwd> || exit 126`
// with the call's workdir or else this directory, so a path the sandbox lacks
// would fail every command that names no workdir before it runs.
func containerEnvironment(endpoint core.ToolEndpoint, workdir string, terminalTimeout int, webSearchEnabled bool, searchURL string, runID, taskID string) ([]string, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if err := harness.ValidateRunID("Hermes", runID); err != nil {
		return nil, err
	}
	if err := harness.ValidateTaskID("Hermes", taskID); err != nil {
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
	if !core.ValidEndpointHost(host) {
		return nil, errors.New("Hermes SSH endpoint host must be a unicast IPv4 address or DNS name")
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return nil, errors.New("Hermes SSH endpoint port is invalid")
	}
	environment := []string{
		"HERMES_HOME=" + stateContainerPath,
		"HERMES_YOLO_MODE=true",
		"API_SERVER_HOST=0.0.0.0",
		"API_SERVER_PORT=8642",
		"TERMINAL_ENV=ssh",
		"TERMINAL_SSH_HOST=" + host,
		"TERMINAL_SSH_PORT=" + port,
		"TERMINAL_SSH_USER=" + endpoint.Username,
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
		// hermes_otel has no backend configured, so it sends nothing over the
		// network and keeps every span in its SQLite store under HERMES_HOME,
		// which collectSpans dumps after the run. The store would otherwise
		// keep only the last 1000 rows of each kind. Content capture is off
		// so prompts, tool arguments, and tool output stay out of the spans.
		"HERMES_OTEL_DASHBOARD_LIVE=true",
		"HERMES_OTEL_DASHBOARD_LIVE_MAX_SPANS=" + strconv.Itoa(otelMaxSpans),
		"HERMES_OTEL_DASHBOARD_LIVE_RETENTION_HOURS=0",
		"HERMES_OTEL_CONTENT_CAPTURE=off",
	}
	if webSearchEnabled {
		if err := harness.ValidateSearch(searchURL, true); err != nil {
			return nil, err
		}
		environment = append(environment, "SEARXNG_URL="+searchURL)
	}
	return environment, nil
}

// gatewayLauncherScript exports the staged credential(s) under their required
// names and replaces itself with the foreground Hermes Gateway. Keeping the exports
// inside the container means no value ever appears in Docker's exec or
// container config. extractEnabled additionally exports the Tavily key
// staged at extractKeyPath, under Hermes's fixed tavilyAPIKeyEnv name.
func gatewayLauncherScript(apiKeyEnv string, extractEnabled bool, mcpHostVars ...string) []byte {
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
	for _, hostVar := range mcpHostVars {
		keyPath := stateContainerPath + "/mcp_" + hostVar + ".key"
		script += `if [ ! -f ` + keyPath + ` ]; then
  echo "ARIES: Hermes MCP secret ` + hostVar + ` is missing" >&2
  exit 1
fi
` + hostVar + `="$(cat ` + keyPath + `)"
export ` + hostVar + `
`
	}
	script += `API_SERVER_KEY="$(cat ` + gatewayKeyPath + `)"
export API_SERVER_KEY
exec hermes gateway run --no-supervise --external-supervisor
`
	return []byte(script)
}

func validateModel(model core.ModelConfig) error {
	if err := harness.ValidateModel("Hermes", model); err != nil {
		return err
	}
	if strings.HasPrefix(model.APIKeyEnv, "API_SERVER_") {
		return errors.New("Hermes model credential uses reserved API_SERVER_ namespace")
	}
	return nil
}

func validateEndpoint(endpoint core.ToolEndpoint) error {
	if endpoint.Protocol != "ssh" || endpoint.Username != "aries" {
		return errors.New("Hermes requires a task-local SSH endpoint")
	}
	if endpoint.ClientCommand != "" || endpoint.ClientSourceFile != "" {
		return errors.New("Hermes uses its own SSH client and accepts no bridge client command")
	}
	return nil
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
