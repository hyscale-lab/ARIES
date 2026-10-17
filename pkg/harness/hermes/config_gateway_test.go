package hermes

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestGatewayConfigPreservesNativeDefaultsAndExplicitDelegationPolicy(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"\nmemory:", "\nauxiliary:", "\ncurator:"} {
		if strings.Contains(string(rendered), section) {
			t.Errorf("overrode native default %q", section)
		}
	}
	config := decodeConfigYAML(t, rendered)
	if !reflect.DeepEqual(config["agent"], map[string]any{"max_turns": 10, "disabled_toolsets": []any{"delegation"}}) {
		t.Fatalf("agent settings = %#v", config["agent"])
	}
	if strings.Contains(string(rendered), "\ndisabled_toolsets:") {
		t.Fatal("API ignores top-level disabled_toolsets")
	}
}

func TestGatewayConfigExplicitlyEnablesDelegation(t *testing.T) {
	rendered, err := renderConfig(validModel(), renderSettings{maxTurns: 10, subagentsEnabled: true, maxConcurrentSubagents: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rendered)
	config := decodeConfigYAML(t, rendered)
	if !slices.Contains(config["platform_toolsets"].(map[string]any)["api_server"].([]any), any("delegation")) || config["delegation"].(map[string]any)["max_concurrent_children"] != 2 {
		t.Fatalf("delegation settings = %#v", config)
	}
	if strings.Contains(text, "disabled_toolsets:") {
		t.Fatal("enabled delegation has denylist")
	}
}

func TestGatewayReservedCredentialNamesFailBeforeAllocation(t *testing.T) {
	for _, name := range []string{"API_SERVER_KEY", "API_SERVER_HOST", "API_SERVER_PORT"} {
		for _, kind := range []string{"model", "mcp"} {
			t.Run(kind+"/"+name, func(t *testing.T) {
				fake := newFakeDeployment()
				fake.createErr = errors.New("unexpected allocation")
				manager := newTestManager(t, fake, []byte("private-model-key"))
				request := testRequest(t)
				if kind == "model" {
					request.Model.APIKeyEnv = name
				} else {
					manager.options.Common.MCPServers = []core.MCPServerConfig{{Name: "service", Command: "service", SecretEnv: map[string]string{"SERVICE_TOKEN": name}}}
				}
				err := manager.Start(context.Background(), request)
				if err == nil || !strings.Contains(err.Error(), "reserved") {
					t.Fatalf("expected reserved credential rejection: %v", err)
				}
				if fake.createCalls != 0 {
					t.Fatal("runtime allocated before credential validation")
				}
			})
		}
	}
}

func TestGatewayAllowsReservedMCPChildAlias(t *testing.T) {
	settings := renderSettings{maxTurns: 10, mcpServers: []core.MCPServerConfig{{Name: "service", Command: "service", SecretEnv: map[string]string{"API_SERVER_KEY": "SERVICE_TOKEN"}}}}
	rendered, err := renderConfig(validModel(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := decodeConfigYAML(t, rendered)
	service := config["mcp_servers"].(map[string]any)["service"].(map[string]any)
	if service["env"].(map[string]any)["API_SERVER_KEY"] != "${SERVICE_TOKEN}" {
		t.Fatal("MCP child alias was not preserved")
	}
}
