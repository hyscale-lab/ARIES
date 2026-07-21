package openclaw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	configContainerPath = "/run/aries/openclaw.json"
	modelKeyPath        = "/run/aries/model.key"
	gatewayKeyPath      = "/run/aries/gateway.key"
	gatewayTokenEnv     = "OPENCLAW_GATEWAY_TOKEN"
	launcherPath        = "/run/aries/launch"
	agentWrapperPath    = "/run/aries/run-agent"
	stateContainerPath  = "/home/node/.openclaw"
	clientConfigPath    = "/run/aries/ssh/config"
	workspaceRoot       = "/aries/openclaw"
)

type openClawConfig struct {
	Gateway gatewayConfig `json:"gateway"`
	Models  modelsConfig  `json:"models"`
	Agents  agentsConfig  `json:"agents"`
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
	if err := validateEndpoint(endpoint); err != nil {
		return nil, err
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
				"aries": {
					BaseURL: model.BaseURL,
					APIKey:  "${" + model.APIKeyEnv + "}",
					API:     "openai-completions",
					Models:  []modelRecord{{ID: model.Model, Name: model.Model}},
				},
			},
		},
		Agents: agentsConfig{Defaults: agentDefaults{
			Model: primaryModel{Primary: "aries/" + model.Model},
			Sandbox: sandboxConfig{
				Mode: "all", Scope: "shared", Backend: "ssh", WorkspaceAccess: "none",
				SSH: sshConfig{
					Target: endpoint.Username + "@" + endpoint.Address, Command: endpoint.ClientCommand,
					WorkspaceRoot: workspaceRoot, StrictHostKeyChecking: true, UpdateHostKeys: false,
					IdentityFile: endpoint.IdentityFile, KnownHostsFile: endpoint.KnownHostsFile,
				},
			},
		}},
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
	parsed, err := url.Parse(model.BaseURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("OpenClaw model base URL must be absolute HTTP(S)")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("OpenClaw model base URL must not contain credentials, query, or fragment")
	}
	if strings.TrimSpace(model.Model) == "" || strings.ContainsAny(model.Model, "\x00\r\n") {
		return errors.New("OpenClaw model ID is invalid")
	}
	if !validEnvironmentName(model.APIKeyEnv) {
		return errors.New("OpenClaw API-key environment name is invalid")
	}
	return nil
}

func validateEndpoint(endpoint core.ToolEndpoint) error {
	if endpoint.Protocol != "ssh" || endpoint.Address != "task-sandbox:2222" || endpoint.Username != "aries" || strings.TrimSpace(endpoint.Network) == "" {
		return errors.New("OpenClaw requires the exact task-local SSH endpoint")
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
		if r == '_' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

func launcherScript(apiKeyEnv string) []byte {
	return []byte("#!/bin/sh\nset -eu\nmodel_key=$(cat " + modelKeyPath + ")\ngateway_key=$(cat " + gatewayKeyPath + ")\nexport " + apiKeyEnv + "=\"$model_key\"\nexport " + gatewayTokenEnv + "=\"$gateway_key\"\nunset model_key gateway_key\nexec \"$@\"\n")
}

func agentWrapperScript() []byte {
	return []byte(`#!/bin/sh
set -u
result_dir=$1
shift
mkdir -p "$result_dir"
"$@" >"$result_dir/stdout.tmp" 2>"$result_dir/stderr.tmp" &
pid=$!
printf '%s' "$pid" >"$result_dir/pid.tmp"
mv "$result_dir/pid.tmp" "$result_dir/pid"
wait "$pid"
status=$?
mv "$result_dir/stdout.tmp" "$result_dir/stdout"
mv "$result_dir/stderr.tmp" "$result_dir/stderr"
printf '%s' "$status" >"$result_dir/status.tmp"
mv "$result_dir/status.tmp" "$result_dir/status"
exit "$status"
`)
}
