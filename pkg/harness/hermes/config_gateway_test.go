package hermes

import (
	"context"
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
	"strings"
	"testing"
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
	if !strings.Contains(string(rendered), "\nagent:\n  max_turns: 10\n  disabled_toolsets:\n    - delegation\n") {
		t.Fatal("missing explicit delegation policy")
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
	if !strings.Contains(text, "    - delegation\n") || !strings.Contains(text, "\ndelegation:\n  max_concurrent_children: 2\n") {
		t.Fatal("delegation not enabled and bounded")
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
	if !strings.Contains(string(rendered), `API_SERVER_KEY: "${SERVICE_TOKEN}"`) {
		t.Fatal("MCP child alias was not preserved")
	}
}
