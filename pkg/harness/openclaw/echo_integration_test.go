//go:build integration

package openclaw

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/model/echo"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// TestEchoModelReceivesTheRealOpenClawRequest runs the pinned OpenClaw image
// against the echo model. The agent's final response is then the request
// OpenClaw actually sent, so the assertions see its tool definitions and the
// task instruction exactly as a real model would.
func TestEchoModelReceivesTheRealOpenClawRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	api, err := client.New(client.FromEnv, client.WithUserAgent("aries-openclaw-echo/1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	if _, err := api.Ping(ctx, client.PingOptions{}); err != nil {
		t.Fatalf("Docker daemon is required: %v", err)
	}
	versions, err := config.LoadVersions(filepath.Join(repositoryRoot(t), "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	ensureSDKImage(t, ctx, api, versions.OpenClaw.Image)

	runID := "openclaw-echo-model"
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
	created, err := api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "aries-echo-" + runID,
		Config: &container.Config{
			Image: versions.OpenClaw.Image, Entrypoint: []string{"/aries-echo"},
			Cmd: []string{"-addr", "0.0.0.0:8080"}, Labels: labels,
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(networkName),
			Binds:       []string{requiredIntegrationFile(t, "ARIES_ECHO_SERVER") + ":/aries-echo:ro"},
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

	keys := map[string]string{"MODEL_KEY": "echo-model-secret-key"}
	harness, err := New(Options{
		Image: versions.OpenClaw.Image, OutputDir: t.TempDir(),
		CleanupTimeout: 30 * time.Second, StartTimeout: time.Minute, AgentTimeout: 2 * time.Minute,
		APIKeyLookup: func(name string) ([]byte, bool) { value, ok := keys[name]; return []byte(value), ok },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_ = harness.Stop(cleanupCtx)
		_ = harness.Close()
	})
	if err := harness.Start(ctx, core.HarnessRequest{
		RunID: runID, TaskID: "echo", Endpoint: fixtureEndpoint(t, networkName), Timeout: 2 * time.Minute,
		Model: core.ModelConfig{Provider: "openai", BaseURL: "http://echo-model:8080/v1", Model: echo.DefaultModel, APIKeyEnv: "MODEL_KEY"},
	}); err != nil {
		t.Fatal(err)
	}
	const instruction = "ARIES-ECHO-INSTRUCTION: list the files you can see."
	result, err := harness.Run(ctx, instruction)
	if err != nil || result.Status != core.StatusSucceeded {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	for _, want := range []string{
		`"path": "/v1/chat/completions"`, `"model": "` + echo.DefaultModel + `"`, instruction,
		`"tools"`, `"name": "exec"`, `"Authorization": [`, "[redacted]",
	} {
		if !strings.Contains(result.FinalResponse, want) {
			t.Errorf("final response lacks %q:\n%s", want, result.FinalResponse)
		}
	}
	if strings.Contains(result.FinalResponse, keys["MODEL_KEY"]) {
		t.Fatal("model key reached the echoed response")
	}
	if err := harness.Stop(context.WithoutCancel(ctx)); err != nil {
		t.Fatal(err)
	}
}
