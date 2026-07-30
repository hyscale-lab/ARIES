package openclaw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	configContainerPath = "/run/aries/openclaw.json"
	modelKeyPath        = "/run/aries/model.key"
	realtimeKeyPath     = "/run/aries/realtime.key"
	gatewayKeyPath      = "/run/aries/gateway.key"
	gatewayTokenEnv     = "OPENCLAW_GATEWAY_TOKEN"
	launcherPath        = "/run/aries/launch"
	agentWrapperPath    = "/run/aries/run-agent"
	stateContainerPath  = "/home/node/.openclaw"
	workspaceRoot       = "/aries/openclaw"
)

type openClawConfig struct {
	Gateway gatewayConfig `json:"gateway"`
	Models  modelsConfig  `json:"models"`
	Agents  agentsConfig  `json:"agents"`
	Tools   toolPolicy    `json:"tools"`
}

type toolPolicy struct {
	Deny []string `json:"deny"`
}

type gatewayConfig struct {
	Mode   string        `json:"mode"`
	Auth   gatewayAuth   `json:"auth"`
	Remote gatewayRemote `json:"remote"`
}

type gatewayAuth struct {
	Mode  string `json:"mode"`
	Token string `json:"token"`
}

type gatewayRemote struct {
	Token string `json:"token"`
}

type modelsConfig struct {
	Mode      string                    `json:"mode"`
	Providers map[string]providerConfig `json:"providers"`
}

type providerConfig struct {
	BaseURL string        `json:"baseUrl"`
	APIKey  string        `json:"apiKey"`
	API     string        `json:"api"`
	Models  []modelRecord `json:"models"`
}

type modelRecord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type agentsConfig struct {
	Defaults agentDefaults `json:"defaults"`
}

type agentDefaults struct {
	Model   primaryModel  `json:"model"`
	Sandbox sandboxConfig `json:"sandbox"`
}

type primaryModel struct {
	Primary string `json:"primary"`
}

type sandboxConfig struct {
	Mode            string    `json:"mode"`
	Scope           string    `json:"scope"`
	Backend         string    `json:"backend"`
	WorkspaceAccess string    `json:"workspaceAccess"`
	SSH             sshConfig `json:"ssh"`
}

type sshConfig struct {
	Target                string `json:"target"`
	Command               string `json:"command"`
	WorkspaceRoot         string `json:"workspaceRoot"`
	StrictHostKeyChecking bool   `json:"strictHostKeyChecking"`
	UpdateHostKeys        bool   `json:"updateHostKeys"`
	IdentityFile          string `json:"identityFile"`
	KnownHostsFile        string `json:"knownHostsFile"`
}

func renderConfig(model core.ModelConfig, endpoint core.ToolEndpoint) ([]byte, error) {
	if err := validateModel(model); err != nil {
		return nil, err
	}
	if model.Provider == "sglang" {
		model.BaseURL, _ = normalizeSGLangBaseURL(model.BaseURL)
	}
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
	}
	providerID := model.Provider
	if providerID == "deepseek" {
		providerID = "aries"
	}
	configuration := openClawConfig{
		Gateway: gatewayConfig{
			Mode:   "local",
			Auth:   gatewayAuth{Mode: "token", Token: "${" + gatewayTokenEnv + "}"},
			Remote: gatewayRemote{Token: "${" + gatewayTokenEnv + "}"},
		},
		Models: modelsConfig{
			Mode: "merge",
			Providers: map[string]providerConfig{
				providerID: {
					BaseURL: model.BaseURL,
					APIKey:  "${" + model.APIKeyEnv + "}",
					API:     "openai-completions",
					Models:  []modelRecord{{ID: model.Model, Name: model.Model}},
				},
			},
		},
		Agents: agentsConfig{Defaults: agentDefaults{
			Model: primaryModel{Primary: providerID + "/" + model.Model},
			Sandbox: sandboxConfig{
				Mode: "all", Scope: "shared", Backend: "ssh", WorkspaceAccess: "rw",
				SSH: sshConfig{
					Target: endpoint.Username + "@" + endpoint.Address, Command: endpoint.ClientCommand,
					WorkspaceRoot: workspaceRoot, StrictHostKeyChecking: true, UpdateHostKeys: false,
					IdentityFile: endpoint.IdentityFile, KnownHostsFile: endpoint.KnownHostsFile,
				},
			},
		}},
		// OpenClaw's native SSH filesystem helpers require python3 inside the
		// remote image. Terminal-Bench images do not promise it, while the exec
		// tool has the same sandbox access without changing task images.
		Tools: toolPolicy{Deny: []string{"read", "write", "edit", "apply_patch"}},
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(configuration); err != nil {
		return nil, fmt.Errorf("encode OpenClaw config: %w", err)
	}
	return output.Bytes(), nil
}

func validateModel(model core.ModelConfig) error {
	if model.Provider != "deepseek" && model.Provider != "sglang" {
		return errors.New("OpenClaw model provider must be deepseek or sglang")
	}
	if model.Provider == "sglang" {
		if _, err := normalizeSGLangBaseURL(model.BaseURL); err != nil {
			return fmt.Errorf("OpenClaw SGLang base URL: %w", err)
		}
	} else {
		parsed, err := url.Parse(model.BaseURL)
		if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
			return errors.New("OpenClaw model base URL must be absolute HTTP(S)")
		}
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return errors.New("OpenClaw model base URL must not contain credentials, query, or fragment")
		}
	}
	if strings.TrimSpace(model.Model) == "" || strings.ContainsAny(model.Model, "\x00\r\n") {
		return errors.New("OpenClaw model ID is invalid")
	}
	if !validEnvironmentName(model.APIKeyEnv) {
		return errors.New("OpenClaw API-key environment name is invalid")
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
		return errors.New("OpenClaw requires a task-local SSH endpoint")
	}
	host, port, err := net.SplitHostPort(endpoint.Address)
	if err != nil || net.ParseIP(host) == nil || port == "" {
		return errors.New("OpenClaw SSH address must be an IP host and port")
	}
	paths := map[string]string{
		"client command": endpoint.ClientCommand, "client source": endpoint.ClientSourceFile,
		"identity": endpoint.IdentityFile, "identity source": endpoint.IdentitySourceFile,
		"known-hosts": endpoint.KnownHostsFile, "known-hosts source": endpoint.KnownHostsSourceFile,
	}
	for name, path := range paths {
		if path == "" || strings.ContainsRune(path, 0) || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("OpenClaw %s path must be absolute and clean", name)
		}
	}
	if endpoint.ClientCommand != "/opt/aries/bin/aries-ssh" || endpoint.IdentityFile != "/run/aries/ssh/id_ed25519" || endpoint.KnownHostsFile != "/run/aries/ssh/known_hosts" {
		return errors.New("OpenClaw endpoint paths do not match the pinned bridge contract")
	}
	return nil
}

func validEnvironmentName(value string) bool {
	for index, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

func launcherScript(apiKeyEnv, realtimeAPIKeyEnv string) []byte {
	script := "#!/bin/sh\nset -eu\nmodel_key=$(cat " + modelKeyPath + ")\ngateway_key=$(cat " + gatewayKeyPath + ")\nexport " + apiKeyEnv + "=\"$model_key\"\nexport " + gatewayTokenEnv + "=\"$gateway_key\"\n"
	if realtimeAPIKeyEnv != "" {
		script += "realtime_key=$(cat " + realtimeKeyPath + ")\nexport " + realtimeAPIKeyEnv + "=\"$realtime_key\"\nunset realtime_key\n"
	}
	script += "unset model_key gateway_key\nexec \"$@\"\n"
	return []byte(script)
}
