package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	supportedVersion = "0.157.1"
	stagedRoot       = "/run/aries"
	codexHome        = stagedRoot + "/codex/home"
	codexPath        = stagedRoot + "/codex/codex"
	configPath       = codexHome + "/config.toml"
	environmentsPath = codexHome + "/environments.toml"
	modelKeyPath     = stagedRoot + "/codex/model.key"
	agentWrapperPath = stagedRoot + "/codex/run-agent"
	clientPath       = stagedRoot + "/codex/aries-codex-ssh"
)

// proactiveDelegationText is Codex 0.157.1's bundled proactive multi-agent
// mode text (PROACTIVE_MULTI_AGENT_MODE_TEXT in
// codex-rs/prompts/src/model_messages/multi_agent.rs). Codex injects it only
// under reasoning effort "ultra", which on models without catalog metadata
// (self-hosted servers) also rewrites the requested effort to "medium".
// Configuring it as the multi-agent mode hint injects the same text while the
// profile keeps control of the effort.
const proactiveDelegationText = "Proactive multi-agent delegation is active. Any earlier developer instruction requiring an explicit user request before spawning sub-agents no longer applies. This mode remains active until a later multi-agent mode developer message changes it. User requests override this hint.\n\nIf at any point you can parallelize work by delegating tasks to another agent (no matter if you are root or subagent), you should do so using collaboration tools if it could save time or improve quality."

// Subagents selects Codex's native delegation. Codex enables it by default,
// so it is always rendered explicitly. MaxConcurrent counts children.
type Subagents struct {
	Enabled       bool
	MaxConcurrent int
}

// Codex 0.157.1 speaks Responses to custom providers. Chat Completions-only
// providers and unsupported generation knobs must fail before any side effect.
func renderConfig(model core.ModelConfig, subagents Subagents) ([]byte, error) {
	if model.Provider != "openai" && model.Provider != "sglang" {
		return nil, errors.New("Codex requires an openai or sglang backend with the Responses API")
	}
	parsed, err := url.Parse(model.BaseURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.User != nil || parsed.Opaque != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(model.BaseURL, "#") || parsed.Path != "/v1" && parsed.Path != "/v1/" {
		return nil, errors.New("Codex base URL must be absolute HTTP(S) with exactly /v1 and no credentials, query, or fragment")
	}
	parsed.Path = "/v1"
	if strings.TrimSpace(model.Model) == "" || strings.ContainsFunc(model.Model, unicode.IsControl) {
		return nil, errors.New("Codex model ID is invalid")
	}
	if !validAPIKeyName(model.APIKeyEnv) {
		return nil, errors.New("Codex API-key environment name is invalid or reserved")
	}
	if model.ContextLength < 0 {
		return nil, errors.New("Codex context length must be positive")
	}
	if model.MaxTokens != 0 || model.Temperature != nil {
		return nil, errors.New("Codex does not support profile max_tokens or temperature")
	}
	effort := model.ReasoningEffort
	switch effort {
	case "off":
		effort = "none" // ARIES's spelling; Codex sends Responses values as-is.
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
	default:
		return nil, errors.New("Codex reasoning effort must be off, none, minimal, low, medium, high, xhigh, or max")
	}
	if subagents.MaxConcurrent < 0 {
		return nil, errors.New("Codex sub-agent concurrency must not be negative")
	}
	var output bytes.Buffer
	fmt.Fprintf(&output, "model = %s\nmodel_provider = \"aries\"\napproval_policy = \"never\"\nsandbox_mode = \"danger-full-access\"\nweb_search = \"disabled\"\nallow_login_shell = false\n", tomlString(model.Model))
	if model.ContextLength > 0 {
		fmt.Fprintf(&output, "model_context_window = %d\n", model.ContextLength)
	}
	if effort != "" {
		fmt.Fprintf(&output, "model_reasoning_effort = %s\n", tomlString(effort))
	}
	// Native exec-server applies this policy to its own remote environment;
	// Codex removes unchanged local environment values before sending the
	// request. Preserve the task image's toolchain environment (for example,
	// CARGO_HOME and RUSTUP_HOME), while excluding the harness-only model key.
	fmt.Fprintf(&output, "\n[shell_environment_policy]\ninherit = \"all\"\nexclude = [%s]\n", tomlString(model.APIKeyEnv))
	fmt.Fprintf(&output, "\n[model_providers.aries]\nname = \"ARIES\"\nbase_url = %s\nwire_api = \"responses\"\nenv_key = %s\nrequires_openai_auth = false\n", tomlString(parsed.String()), tomlString(model.APIKeyEnv))
	fmt.Fprintf(&output, "\n[agents]\nenabled = %t\n", subagents.Enabled)
	if subagents.Enabled && subagents.MaxConcurrent > 0 {
		fmt.Fprintf(&output, "max_concurrent_threads_per_session = %d\n", subagents.MaxConcurrent)
	}
	if subagents.Enabled {
		// V2 is Codex's current multi-agent backend; without the hint its
		// default mode tells the model not to delegate unless asked.
		fmt.Fprintf(&output, "\n[features.multi_agent_v2]\nenabled = true\nmulti_agent_mode_hint_text = %s\n", tomlString(proactiveDelegationText))
	}
	return output.Bytes(), nil
}

// The shared bridge service accepts unauthenticated clients on the run's
// private network, so the helper takes only the endpoint address.
func renderEnvironments(endpoint core.ToolEndpoint) ([]byte, error) {
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	return []byte("default = \"aries\"\ninclude_local = false\n\n[[environments]]\nid = \"aries\"\nprogram = " + tomlString(endpoint.ClientCommand) + "\nargs = [\"--address\", " + tomlString(endpoint.Address) + "]\ninitialize_timeout_sec = 30\n"), nil
}

func validateEndpoint(endpoint core.ToolEndpoint) error {
	if endpoint.Protocol != "ssh" || endpoint.Username != "aries" {
		return errors.New("Codex requires a task-local SSH endpoint")
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || !core.ValidEndpointHost(host) || portErr != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("Codex SSH address must include a valid host and port")
	}
	if endpoint.ClientCommand != clientPath || endpoint.ClientSourceFile == "" {
		return errors.New("Codex requires the bridge's staged SSH helper")
	}
	if !path.IsAbs(endpoint.Workdir) || path.Clean(endpoint.Workdir) != endpoint.Workdir || strings.ContainsFunc(endpoint.Workdir, unicode.IsControl) || endpoint.Workdir == stagedRoot || strings.HasPrefix(endpoint.Workdir, stagedRoot+"/") {
		return errors.New("Codex task workdir must be a clean absolute path outside its private runtime")
	}
	return nil
}

func validAPIKeyName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	switch name {
	case "HOME", "PATH", "SHELL", "USER", "LOGNAME", "BASH_ENV", "ENV", "SHELLOPTS", "IFS", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "LC_CTYPE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME":
		return false
	}
	if name == "CODEX_API_KEY" {
		return true
	}
	for _, prefix := range []string{"CODEX_", "LD_", "DYLD_", "RUST_"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

func agentWrapperScript(apiKeyEnv string) []byte {
	// Only the validated variable name enters this constant script. Values are
	// read from a private staged file; Docker never sees them in Env or Cmd.
	return []byte("#!/bin/sh\nset -eu\n" + apiKeyEnv + `="$(cat ` + modelKeyPath + `)"` + "\nexport " + apiKeyEnv + "\nexec " + codexPath + " \"$@\"\n")
}

func tomlString(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }

func finalResponse(content []byte) (string, error) {
	var final string
	completed := false
	for _, line := range bytes.Split(content, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return "", errors.New("Codex emitted invalid JSONL")
		}
		switch event.Type {
		case "item.completed":
			if event.Item.Type == "agent_message" {
				final = event.Item.Text
			}
		case "turn.completed":
			completed = true
		case "turn.failed", "error":
			return "", errors.New("Codex reported a failed turn")
		}
	}
	if !completed {
		return "", errors.New("Codex trajectory has no completed turn")
	}
	return final, nil
}
