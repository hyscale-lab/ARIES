package dsh

import (
	"encoding/hex"
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
	"github.com/hyscale-lab/aries/pkg/harness"
	modelconfig "github.com/hyscale-lab/aries/pkg/model"
)

// renderConfig produces a JSON document accepted as a native DSH YAML patch.
// The launcher exports the staged credential as ARIES_MODEL_API_KEY and keeps
// HOME and DSH_HOME under /run/aries/dsh/home. The task filesystem and processes
// use the version-matched native SSH providers, independently of that home.
func renderConfig(model core.ModelConfig, options harness.Options, endpoint core.ToolEndpoint, helperHash string) ([]byte, error) {
	if err := harness.ValidateModel("DSH", model); err != nil {
		return nil, err
	}
	if err := modelconfig.ValidateGeneration(model); err != nil {
		return nil, err
	}
	if model.Temperature != nil {
		return nil, errors.New("DSH does not support model.temperature through its native provider configuration")
	}
	if options.Mode != "" && options.Mode != "agent" {
		return nil, errors.New("DSH supports only agent mode")
	}
	if options.WebSearchEnabled || options.ExtractAPIKeyEnv != "" {
		return nil, errors.New("DSH does not support the ARIES web search or extract configuration")
	}
	if len(options.MCPServers) != 0 {
		return nil, errors.New("DSH MCP server configuration is not supported")
	}
	if options.MaxConcurrentSubagents != 0 {
		return nil, errors.New("DSH cannot enforce max_concurrent_subagents across all native delegation tools")
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	digest, err := hex.DecodeString(helperHash)
	if err != nil || len(digest) != 32 || strings.ToLower(helperHash) != helperHash {
		return nil, errors.New("DSH helper hash must be a lowercase SHA-256 digest")
	}

	selection := map[string]any{"provider": model.Provider, "model": model.Model}
	patch := []map[string]any{
		// A patch's name asserts identity; changing it skips the row. Disable
		// the local providers and insert their native SSH replacements instead.
		{"id": "subprocess", "disabled": true},
		{"id": "sandbox", "disabled": true},
		{"id": "fs-sandbox", "disabled": true},
		// ARIES supplies the process and filesystem isolation. Avoid nested
		// bwrap, whose private /tmp would hide the installed remote runtime.
		{"id": "sandbox-policy", "config": map[string]any{"mode": "danger-full-access", "workspaceRoot": endpoint.Workdir}},
		{"id": "approval", "config": map[string]any{"policy": "never"}},
		{"id": "credentials", "config": map[string]any{"path": "/run/aries/dsh/home/.credentials.yaml"}},
		{"id": "session-persistence-jsonl", "config": map[string]any{"root": "/run/aries/dsh/sessions", "compression": "none"}},
		// Native headless can otherwise run with failed tool plugins. Require
		// the remote execution services before admitting the benchmark turn.
		{"id": "headless-runner", "inject": []string{"headlessStartup", "fs", "subprocess", "sandbox", "shell", "ptcRuntime"}},
		{"id": "storage-json", "config": map[string]any{"root": "/run/aries/dsh/storages"}},
		{"id": "tool-web", "config": map[string]any{"search": false, "fetch": true}},
		{"id": "ptc-runtime", "config": map[string]any{
			"nodeExecutable": "/tmp/aries-dsh-runtime/node",
			"bootstrapPath":  "/tmp/aries-dsh-runtime/node_modules/@deepseek-ai/dsh-ptc-runtime-node/lib/process.js",
		}},
		{"insert": []map[string]any{
			{
				"id": "ssh", "name": "@deepseek-ai/dsh-ssh",
				"config": map[string]any{
					"host": "aries-sandbox", "node": "/tmp/aries-dsh-runtime/node",
					"helper":     "/tmp/aries-dsh-runtime/node_modules/@deepseek-ai/dsh-ssh/lib/helper.js",
					"helperHash": helperHash, "workspace": endpoint.Workdir,
				},
			},
			{"id": "subprocess-ssh", "name": "@deepseek-ai/dsh-subprocess-ssh"},
			{"id": "sandbox-ssh", "name": "@deepseek-ai/dsh-sandbox-ssh"},
			{"id": "fs-ssh", "name": "@deepseek-ai/dsh-fs-ssh"},
		}},
	}
	if model.Provider == "deepseek" {
		selection["provider"] = "deepseek-official"
		baseURL, err := deepSeekBaseURL(model.BaseURL)
		if err != nil {
			return nil, err
		}
		provider := map[string]any{"baseURL": baseURL, "apiKeyEnv": "ARIES_MODEL_API_KEY"}
		if model.ContextLength > 0 || model.MaxTokens > 0 {
			provider["models"] = []map[string]any{modelEntry(model)}
		}
		if model.ReasoningEffort != "" {
			effort := model.ReasoningEffort
			if effort == "none" {
				effort = "off"
			}
			switch effort {
			case "off", "low", "high", "max":
			default:
				return nil, errors.New("DSH DeepSeek reasoning effort must be off, none, low, high, or max")
			}
			provider["reasoningEffort"] = effort
			selection["reasoningEffort"] = effort
		}
		patch = append(patch, map[string]any{"id": "llm-deepseek", "config": provider})
	} else {
		if model.ReasoningEffort != "" {
			return nil, errors.New("DSH openai and sglang reasoning effort requires a model-specific native pi-ai profile and is not supported")
		}
		baseURL, err := harness.NormalizeV1BaseURL(model.BaseURL)
		if err != nil {
			return nil, err
		}
		patch = append(patch, map[string]any{"id": "llm-pi-ai", "config": map[string]any{
			"providers": map[string]any{model.Provider: map[string]any{
				"api": "openai-completions", "baseURL": baseURL, "apiKeyEnv": "ARIES_MODEL_API_KEY",
				"models": []map[string]any{modelEntry(model)},
				"compat": map[string]any{"maxTokensField": "max_tokens"},
			}},
		}})
	}
	patch = append(patch, map[string]any{"id": "agent-default-model", "config": selection})
	if !options.SubagentsEnabled {
		for _, id := range []string{"tool-subagent", "tool-subagent-fork", "tool-subagent-control", "tool-subagent-list-agents", "tool-workflow"} {
			patch = append(patch, map[string]any{"id": id, "disabled": true})
		}
	}
	return json.MarshalIndent(patch, "", "  ")
}

func modelEntry(model core.ModelConfig) map[string]any {
	entry := map[string]any{"id": model.Model}
	if model.ContextLength > 0 {
		entry["contextWindow"] = model.ContextLength
	}
	if model.MaxTokens > 0 {
		entry["maxTokens"] = model.MaxTokens
	}
	return entry
}

func validateEndpoint(endpoint core.ToolEndpoint) error {
	if endpoint.Protocol != "ssh" || endpoint.Username != "aries" || endpoint.ClientCommand != "" || endpoint.ClientSourceFile != "" {
		return errors.New("DSH requires a native SSH endpoint for user aries without a client override")
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host == "" || strings.ContainsFunc(endpoint.Address, unicode.IsControl) {
		return errors.New("DSH SSH endpoint must contain a host and port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("DSH SSH endpoint port is invalid")
	}
	if !path.IsAbs(endpoint.Workdir) || strings.ContainsFunc(endpoint.Workdir, unicode.IsControl) {
		return errors.New("DSH SSH endpoint requires an absolute sandbox workdir")
	}
	return nil
}

func deepSeekBaseURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("DSH DeepSeek base URL: %w", err)
	}
	// ARIES profiles commonly name DeepSeek's Chat Completions root. Its
	// native Messages API lives under /anthropic; custom deployment paths are
	// already Messages roots and retain the native adapter's URL semantics.
	if parsed.Hostname() == "api.deepseek.com" && (parsed.Path == "" || parsed.Path == "/" || parsed.Path == "/v1" || parsed.Path == "/v1/") {
		parsed.Path = "/anthropic"
	}
	return parsed.String(), nil
}
