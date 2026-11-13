package hermes

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/harness"
	modelconfig "github.com/hyscale-lab/aries/pkg/model"
	"go.yaml.in/yaml/v3"
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

// The ARIES Hermes plugin and the seam script that lets it supply file
// operations. ImageContext puts both into the ARIES Hermes image.
var (
	//go:embed plugin/plugin.yaml
	pluginManifest []byte
	//go:embed plugin/__init__.py
	pluginModule []byte
	//go:embed plugin/sandbox_pb2.py
	pluginSandboxPB []byte
	//go:embed plugin/sandbox_pb2_grpc.py
	pluginSandboxGRPC []byte
	//go:embed seam.py
	seamScript []byte
)

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
	noSandboxTools         bool
	// grpcTarget enables the ARIES terminal backend plugin and supplies its target.
	grpcTarget string
}

// sandboxToolsets are Hermes's toolsets that act in the sandbox: the shell,
// file access and code execution (core.Task.NoSandboxTools withholds them).
var sandboxToolsets = []string{"terminal", "file", "code_execution"}

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
// Optional blocks are written only when the profile sets them, preserving
// native omission and default semantics:
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
	if len(settings.extraBody) != 0 && model.Provider == "deepseek" {
		return nil, errors.New("Hermes merges user extra_body only for the custom provider, not deepseek")
	}
	provider := hermesProvider(model.Provider)
	reasoningBody, err := modelconfig.ReasoningBody(model)
	if err != nil {
		return nil, err
	}
	requestBody := make(map[string]any)
	if len(settings.extraBody) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(settings.extraBody))
		decoder.UseNumber()
		if err := decoder.Decode(&requestBody); err != nil || len(requestBody) == 0 {
			return nil, errors.New("Hermes extra_body must be a non-empty JSON object")
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, errors.New("Hermes extra_body must be a non-empty JSON object")
		}
	}
	if reasoningBody != nil {
		for _, key := range []string{"reasoning_effort", "thinking", "reasoning"} {
			if _, exists := requestBody[key]; exists {
				return nil, fmt.Errorf("Hermes model.reasoning_effort conflicts with extra_body.%s", key)
			}
		}
		for key, value := range reasoningBody {
			requestBody[key] = value
		}
		// The pin's DeepSeek profile omits thinking controls for deepseek-flash.
		// Native custom-provider extras preserve the exact requested wire fields.
		provider = "custom"
	}
	if model.Temperature != nil {
		if !harness.OpenAICompatible(model.Provider) {
			return nil, errors.New("Hermes temperature requires the sglang or openai backend")
		}
		if _, exists := requestBody["temperature"]; exists {
			return nil, errors.New("Hermes model.temperature conflicts with extra_body.temperature")
		}
		// The explicit tag keeps a whole temperature, including zero, a float
		// when Hermes reads it with PyYAML.
		requestBody["temperature"] = &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!float", Style: yaml.TaggedStyle,
			Value: strconv.FormatFloat(*model.Temperature, 'g', -1, 64),
		}
	}
	modelConfig := map[string]any{
		"default":  model.Model,
		"provider": provider,
		"base_url": model.BaseURL,
		"api_key":  "${" + model.APIKeyEnv + "}",
		"api_mode": "chat_completions",
	}
	if model.ContextLength > 0 {
		modelConfig["context_length"] = model.ContextLength
	}
	if model.MaxTokens > 0 {
		modelConfig["max_tokens"] = model.MaxTokens
	}
	agentConfig := map[string]any{"max_turns": settings.maxTurns}
	var disabled []string
	if !settings.subagentsEnabled {
		disabled = append(disabled, "delegation")
	}
	if settings.noSandboxTools {
		// Keep these off whatever a toolset's own default is, as well as
		// leaving them out of platform_toolsets below.
		disabled = append(disabled, sandboxToolsets...)
	}
	if len(disabled) > 0 {
		agentConfig["disabled_toolsets"] = disabled
	}
	plugins := map[string]any{"enabled": []string{otelPluginName}}
	if settings.grpcTarget != "" {
		plugins["enabled"] = []string{otelPluginName, "aries"}
		plugins["entries"] = map[string]any{"aries": map[string]any{"settings": map[string]any{"grpc_target": settings.grpcTarget}}}
	}
	config := map[string]any{
		"model": modelConfig,
		"agent": agentConfig,
		// Listed plugins supply the per-call timing absent from the native
		// session store. Images without hermes_otel skip the name.
		"plugins": plugins,
		"display": map[string]any{"streaming": false, "compact": true},
	}
	if settings.compaction != nil {
		compression := make(map[string]any)
		if settings.compaction.Enabled != nil {
			compression["enabled"] = *settings.compaction.Enabled
		}
		if settings.compaction.ThresholdTokens > 0 {
			compression["threshold_tokens"] = settings.compaction.ThresholdTokens
		}
		if len(compression) == 0 {
			config["compression"] = nil
		} else {
			config["compression"] = compression
		}
	}
	if len(requestBody) > 0 {
		if provider != "custom" {
			return nil, errors.New("Hermes merges extra_body only for the custom provider, not " + model.Provider)
		}
		// The base_url fallback adds native request overrides without
		// shadowing the model route or credential. Hermes expands any
		// registered ${NAME} placeholders from its process environment.
		config["custom_providers"] = []map[string]any{{
			"name": "aries", "base_url": model.BaseURL,
			"extra_body": yamlJSONNumbers(requestBody),
		}}
	}
	if settings.subagentsEnabled && settings.maxConcurrentSubagents > 0 {
		config["delegation"] = map[string]any{"max_concurrent_children": settings.maxConcurrentSubagents}
	}
	if voiceSTT != nil {
		config["stt"] = map[string]any{
			"enabled": true, "provider": voiceSTT.Provider,
			"openai": map[string]any{"model": voiceSTT.Model},
			"local":  map[string]any{"model": voiceSTT.Model, "language": voiceSTT.Language},
		}
	}
	// Native API agent construction resolves this platform-specific list.
	toolsets := []string{}
	if !settings.noSandboxTools {
		toolsets = append(toolsets, sandboxToolsets...)
	}
	if settings.subagentsEnabled {
		toolsets = append(toolsets, "delegation")
	}
	if settings.webSearchEnabled {
		toolsets = append(toolsets, "web")
	}
	config["platform_toolsets"] = map[string]any{"api_server": toolsets}
	if settings.webSearchEnabled {
		// The sandbox's SearXNG service is search-only. Only select Tavily
		// extraction when its private credential file is staged.
		web := map[string]any{"search_backend": "searxng"}
		if settings.extractEnabled {
			web["extract_backend"] = "tavily"
		}
		config["web"] = web
	}
	// With no MCP server named in platform_toolsets, Hermes enables all
	// configured servers through its native tool_describe/tool_call pair.
	if len(settings.mcpServers) > 0 {
		servers := make(map[string]any, len(settings.mcpServers))
		for _, server := range settings.mcpServers {
			entry := make(map[string]any)
			if server.URL != "" {
				entry["url"] = server.URL
				// streamable-http is Hermes's default when this key is absent.
				if server.Transport == "sse" {
					entry["transport"] = "sse"
				}
			} else if server.Command != "" {
				entry["command"] = server.Command
				if len(server.Args) > 0 {
					entry["args"] = server.Args
				}
				if len(server.Env) > 0 || len(server.SecretEnv) > 0 {
					environment := make(map[string]string, len(server.Env)+len(server.SecretEnv))
					for key, value := range server.Env {
						environment[key] = value
					}
					for key, hostVar := range server.SecretEnv {
						environment[key] = "${" + hostVar + "}"
					}
					entry["env"] = environment
				}
			}
			if server.TimeoutSeconds > 0 {
				entry["timeout"] = server.TimeoutSeconds
			}
			servers[server.Name] = entry
		}
		config["mcp_servers"] = servers
	}
	return yaml.Marshal(config)
}

// renderTerminal is the `terminal:` section appended to config.yaml. Hermes
// wraps every agent command in `builtin cd -- <cwd> || exit 126`, where <cwd>
// is the call's own workdir or else the configured one (tools/terminal_tool.py),
// so the configured directory must exist in the sandbox: it is the directory
// the bridge runs commands in. The file has to carry it, not only
// TERMINAL_CWD: Gateway runtime refresh treats config.yaml as authoritative,
// so backend, cwd, and timeout are supplied together in the terminal section.
// backend is "ssh" for Hermes's own SSH backend or "aries" for the plugin.
func renderTerminal(backend, workdir string, timeout int) ([]byte, error) {
	if workdir == "" {
		return nil, errors.New("Hermes endpoint does not name the sandbox workdir")
	}
	if !validWorkdir(workdir) {
		return nil, fmt.Errorf("Hermes terminal workdir %q is not shell-neutral", workdir)
	}
	if timeout <= 0 {
		return nil, errors.New("Hermes terminal timeout must be positive")
	}
	rendered, err := yaml.Marshal(map[string]any{
		"terminal": map[string]any{"backend": backend, "cwd": workdir, "timeout": timeout},
	})
	return append([]byte("\n"), rendered...), err
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

// yamlJSONNumbers preserves JSON number precision in the request-body subtree.
// Explicit float tags also make exponent-only literals numeric in Hermes's
// YAML 1.1 reader; decoding through float64 would round large integers first.
func yamlJSONNumbers(value any) any {
	switch value := value.(type) {
	case json.Number:
		node := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: value.String()}
		if strings.ContainsAny(value.String(), ".eE") {
			node.Tag, node.Style = "!!float", yaml.TaggedStyle
		}
		return node
	case map[string]any:
		for key, child := range value {
			value[key] = yamlJSONNumbers(child)
		}
	case []any:
		for index, child := range value {
			value[index] = yamlJSONNumbers(child)
		}
	}
	return value
}

// containerEnvironment is the non-secret environment given to the Hermes
// container. SSH uses Hermes's own backend and needs the TERMINAL_SSH_*
// target. gRPC selects the ARIES plugin; config.yaml supplies its target.
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
		return nil, fmt.Errorf("parse Hermes endpoint address: %w", err)
	}
	if !core.ValidEndpointHost(host) {
		return nil, errors.New("Hermes endpoint host must be a unicast IPv4 address or DNS name")
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return nil, errors.New("Hermes endpoint port is invalid")
	}
	environment := []string{
		"HERMES_HOME=" + stateContainerPath,
		"HERMES_YOLO_MODE=true",
		"API_SERVER_HOST=0.0.0.0",
		"API_SERVER_PORT=8642",
	}
	if endpoint.Protocol == "grpc" {
		environment = append(environment, "TERMINAL_ENV=aries")
	} else {
		environment = append(environment,
			"TERMINAL_ENV=ssh",
			"TERMINAL_SSH_HOST="+host,
			"TERMINAL_SSH_PORT="+port,
			"TERMINAL_SSH_USER="+endpoint.Username,
		)
	}
	environment = append(environment,
		"TERMINAL_CWD="+workdir,
		"TERMINAL_TIMEOUT="+strconv.Itoa(terminalTimeout),
		"ARIES_RUN_ID="+runID,
		"ARIES_TASK_ID="+taskID,
		// The v2026.8 image ships HERMES_WRITE_SAFE_ROOT=/opt/data, which makes
		// write_file and patch refuse every path outside that directory. The
		// tools act on the sandbox through the bridge, and the sandbox is the isolation
		// boundary, so the prefix check only denies the agent its own
		// workspace. An empty value turns the check off (agent/file_safety.py).
		"HERMES_WRITE_SAFE_ROOT=",
		// hermes_otel has no backend configured, so it sends nothing over the
		// network and keeps every span in its SQLite store under HERMES_HOME,
		// which collectSpans dumps after the run. The store would otherwise
		// keep only the last 1000 rows of each kind. Content capture is off
		// so prompts, tool arguments, and tool output stay out of the spans.
		"HERMES_OTEL_DASHBOARD_LIVE=true",
		"HERMES_OTEL_DASHBOARD_LIVE_MAX_SPANS="+strconv.Itoa(otelMaxSpans),
		"HERMES_OTEL_DASHBOARD_LIVE_RETENTION_HOURS=0",
		"HERMES_OTEL_CONTENT_CAPTURE=off",
	)
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
`
	script += `if [ ! -f ` + modelKeyPath + ` ]; then
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
	if endpoint.Protocol == "grpc" {
		if endpoint.Username != "aries" || endpoint.ClientCommand != "" || endpoint.ClientSourceFile != "" {
			return errors.New("Hermes requires a gRPC endpoint for user aries without a staged client")
		}
		return nil
	}
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
