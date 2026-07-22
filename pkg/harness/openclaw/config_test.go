package openclaw

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func testEndpoint() core.ToolEndpoint {
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries", Network: "aries-net-test",
		ClientCommand: "/opt/aries/bin/aries-ssh", ClientSourceFile: "/host/aries-ssh",
		IdentityFile: "/run/aries/ssh/id_ed25519", IdentitySourceFile: "/host/id_ed25519",
		KnownHostsFile: "/run/aries/ssh/known_hosts", KnownHostsSourceFile: "/host/known_hosts",
	}
}

func testModel() core.ModelConfig {
	return core.ModelConfig{BaseURL: "http://fake-model:8080/v1", Model: "deterministic-model", APIKeyEnv: "ARIES_FAKE_API_KEY"}
}

func TestRenderConfigLocksProviderSharedSSHAndPlaceholder(t *testing.T) {
	content, err := renderConfig(testModel(), testEndpoint())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("dummy-secret")) || !bytes.Contains(content, []byte(`"apiKey": "${ARIES_FAKE_API_KEY}"`)) {
		t.Fatalf("API key placeholder = %s", content)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(content, &configuration); err != nil {
		t.Fatal(err)
	}
	provider := configuration.Models.Providers["aries"]
	sandbox := configuration.Agents.Defaults.Sandbox
	if configuration.Gateway.Mode != "local" || configuration.Models.Mode != "merge" || provider.API != "openai-completions" || provider.BaseURL != testModel().BaseURL {
		t.Fatalf("provider config = %#v", configuration)
	}
	if configuration.Gateway.Auth.Mode != "token" || configuration.Gateway.Auth.Token != "${OPENCLAW_GATEWAY_TOKEN}" || configuration.Gateway.Remote.Token != "${OPENCLAW_GATEWAY_TOKEN}" {
		t.Fatalf("gateway config = %#v", configuration.Gateway)
	}
	if configuration.Agents.Defaults.Model.Primary != "aries/deterministic-model" || sandbox.Mode != "all" || sandbox.Scope != "shared" || sandbox.Backend != "ssh" || sandbox.WorkspaceAccess != "none" {
		t.Fatalf("agent config = %#v", configuration.Agents.Defaults)
	}
	if sandbox.SSH.Target != "aries@172.22.0.1:39425" || sandbox.SSH.Command != "/opt/aries/bin/aries-ssh" || sandbox.SSH.WorkspaceRoot != workspaceRoot || !sandbox.SSH.StrictHostKeyChecking || sandbox.SSH.UpdateHostKeys {
		t.Fatalf("SSH config = %#v", sandbox.SSH)
	}
}

func TestRenderConfigRejectsInvalidInputs(t *testing.T) {
	for name, mutate := range map[string]func(*core.ModelConfig, *core.ToolEndpoint){
		"base URL": func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.BaseURL = "file:///tmp/model" },
		"model":    func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.Model = "\n" },
		"key env":  func(model *core.ModelConfig, _ *core.ToolEndpoint) { model.APIKeyEnv = "bad-name" },
		"protocol": func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.Protocol = "http" },
		"network":  func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.Network = "" },
		"address":  func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.Address = "task-sandbox:2222" },
		"identity": func(_ *core.ModelConfig, endpoint *core.ToolEndpoint) { endpoint.IdentityFile = "/wrong" },
	} {
		t.Run(name, func(t *testing.T) {
			model, endpoint := testModel(), testEndpoint()
			mutate(&model, &endpoint)
			if _, err := renderConfig(model, endpoint); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}

func TestLauncherUsesFileSecretAndDirectExec(t *testing.T) {
	script := string(launcherScript("ARIES_FAKE_API_KEY"))
	for _, required := range []string{"model_key=$(cat /run/aries/model.key)", "gateway_key=$(cat /run/aries/gateway.key)", "export ARIES_FAKE_API_KEY=\"$model_key\"", "export OPENCLAW_GATEWAY_TOKEN=\"$gateway_key\"", "exec \"$@\""} {
		if !strings.Contains(script, required) {
			t.Fatalf("launcher missing %q: %s", required, script)
		}
	}
}
