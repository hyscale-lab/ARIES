//go:build integration

package hermes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/model/echo"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const echoIntegrationImage = "docker.io/nousresearch/hermes-agent:v2026.8.31"

// TestEchoModelReceivesTheRealHermesRequest runs the pinned Hermes image
// against the echo model. The one-shot's output is then the request Hermes
// actually sent, including the serving parameters ARIES rendered.
func TestEchoModelReceivesTheRealHermesRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv, client.WithUserAgent("aries-hermes-echo/1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required: %v", err)
	}
	if _, err := api.ImageInspect(ctx, echoIntegrationImage); err != nil {
		pull, pullErr := api.ImagePull(ctx, echoIntegrationImage, client.ImagePullOptions{})
		if pullErr != nil {
			t.Fatalf("pull %s: %v", echoIntegrationImage, pullErr)
		}
		defer pull.Close()
		if err := pull.Wait(ctx); err != nil {
			t.Fatalf("pull %s: %v", echoIntegrationImage, err)
		}
	}

	runID := "hermes-echo-model"
	networkName := "aries-net-" + runID
	labels := map[string]string{"aries.managed": "true", "aries.kind": "echo-model", "aries.run": runID}
	if _, err := api.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{Labels: labels}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = api.NetworkRemove(cleanupCtx, networkName, client.NetworkRemoveOptions{})
	})
	echoBinary := os.Getenv("ARIES_ECHO_SERVER")
	if info, err := os.Stat(echoBinary); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("ARIES_ECHO_SERVER=%q must name a built aries-echo: %v", echoBinary, err)
	}
	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "aries-echo-" + runID,
		Config: &container.Config{
			Image: echoIntegrationImage, Entrypoint: []string{"/aries-echo"},
			Cmd: []string{"-addr", "0.0.0.0:8080"}, Labels: labels,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(networkName),
			Binds:       []string{echoBinary + ":/aries-echo:ro"},
		},
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			networkName: {Aliases: []string{"echo-model"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = api.ContainerRemove(cleanupCtx, created.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := api.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}

	const key = "echo-model-secret-key"
	manager, err := New(Options{
		Image: echoIntegrationImage, OutputDir: t.TempDir(),
		StartTimeout: 90 * time.Second, AgentTimeout: 2 * time.Minute, CleanupTimeout: 60 * time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return []byte(key), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cleanupCancel()
		_ = manager.Stop(cleanupCtx)
		_ = manager.Close()
	})
	identity := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(identity, []byte("integration identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	temperature := 0.7
	if err := manager.Start(ctx, core.HarnessRequest{
		RunID: runID, TaskID: "echo",
		Endpoint: core.ToolEndpoint{Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries", Network: networkName, IdentitySourceFile: identity, Workdir: "/app"},
		Model: core.ModelConfig{
			Provider: "openai", BaseURL: "http://echo-model:8080/v1", Model: echo.DefaultModel, APIKeyEnv: "ARIES_TEST_MODEL_KEY",
			ContextLength: 262144, MaxTokens: 32768, Temperature: &temperature,
		},
	}); err != nil {
		t.Fatal(err)
	}
	const instruction = "ARIES-ECHO-INSTRUCTION: list the files you can see."
	result, err := manager.Run(ctx, instruction)
	if err != nil || result.Status != core.StatusSucceeded {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	for _, want := range []string{
		`"path": "/v1/chat/completions"`, `"model": "` + echo.DefaultModel + `"`, instruction,
		`"temperature": 0.7`, `"max_tokens": 32768`, `"tools"`, "[redacted]",
	} {
		if !strings.Contains(result.FinalResponse, want) {
			t.Errorf("final response lacks %q:\n%s", want, result.FinalResponse)
		}
	}
	if strings.Contains(result.FinalResponse, key) {
		t.Fatal("model key reached the echoed response")
	}
	if err := manager.Stop(context.WithoutCancel(ctx)); err != nil {
		t.Fatal(err)
	}
}
