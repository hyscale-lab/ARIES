//go:build integration

package hermes

import (
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"

	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const integrationImage = "docker.io/nousresearch/hermes-agent:v2026.8.31"

func requireDockerImage(t *testing.T) {
	t.Helper()
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := api.ImageInspect(ctx, integrationImage); err != nil {
		t.Fatalf("required pinned Hermes image %s: %v", integrationImage, err)
	}

}

func integrationManager(t *testing.T, outputDir string) *Manager {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:     integrationDeployment(t),
		Image:          integrationImage,
		OutputDir:      outputDir,
		Logger:         logger,
		StartTimeout:   90 * time.Second,
		AgentTimeout:   90 * time.Second,
		CleanupTimeout: 60 * time.Second,
		APIKeyLookup:   func(string) ([]byte, bool) { return []byte("sk-integration-not-a-real-key"), true },
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := manager.Stop(cleanup); err != nil {
			t.Errorf("harness cleanup: %v", err)
		}
		_ = manager.Close()
	})
	return manager
}

// TestHarnessStartsRealHermesContainerAndStopsPositively drives the real
// upstream image: the staged runtime must satisfy the readiness probe (Hermes
// Gateway ready, ssh present, config and key readable), and Stop must confirm the
// container is gone. No model is contacted.
func TestHarnessStartsRealHermesContainerAndStopsPositively(t *testing.T) {
	requireDockerImage(t)
	outputDir := t.TempDir()
	manager := integrationManager(t, outputDir)

	request := core.HarnessRequest{Connectivity: core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: "bridge"}},
		RunID: "integration-run", TaskID: "integration-task",
		Endpoint: core.ToolEndpoint{
			Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries",
			Workdir: "/app",
		},
		Model: core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := manager.Start(ctx, request); err != nil {
		t.Fatalf("start real Hermes container: %v", err)
	}
	containerName := manager.active.Name

	// The staged runtime must be exactly what the container sees.
	for _, check := range []struct {
		path string
		want string
	}{
		{configContainerPath, "${DEEPSEEK_API_KEY}"},
		{modelKeyPath, "sk-integration-not-a-real-key"},
		{gatewayLauncherPath, "hermes gateway"},
	} {
		result, err := manager.runtime.Options.Deployment.Exec(ctx, manager.active.ID, core.Command{Path: "cat", Args: []string{check.path}})
		output := result.Stdout + result.Stderr
		if err != nil || result.ExitCode != 0 {
			t.Fatalf("read %s: %v (%s)", check.path, err, output)
		}
		if !strings.Contains(string(output), check.want) {
			t.Fatalf("%s does not contain %q:\n%s", check.path, check.want, output)
		}
	}
	// Hermes itself must accept the staged configuration.
	result, err := manager.runtime.Options.Deployment.Exec(ctx, manager.active.ID, core.Command{Path: "hermes", Args: []string{"--version"}})
	output := result.Stdout + result.Stderr
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("hermes --version failed: %v (%s)", err, output)
	}
	if !strings.Contains(string(output), "Hermes Agent") {
		t.Fatalf("unexpected hermes version output: %s", output)
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Positive absence.
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if _, err := api.ContainerInspect(ctx, containerName, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("container absence not confirmed: %v", err)
	}
	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	// The rendered config artifact must never carry the credential value.
	retained, err := os.ReadFile(filepath.Join(outputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(retained), "sk-integration-not-a-real-key") {
		t.Fatalf("credential leaked into the retained config:\n%s", retained)
	}
}

func integrationDeployment(t *testing.T) deployment.Deployment {
	t.Helper()
	d, err := dockerdeployment.New(dockerdeployment.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// Fail after Docker has started the native service to exercise cleanup of a
// partially successful startup, rather than an error before allocation.
type failAfterStartDeployment struct {
	deployment.Deployment
	id string
}

func (d *failAfterStartDeployment) Create(ctx context.Context, request deployment.Request) (string, error) {
	id, err := d.Deployment.Create(ctx, request)
	d.id = id
	return id, err
}
func (d *failAfterStartDeployment) Start(ctx context.Context, id string) error {
	if err := d.Deployment.Start(ctx, id); err != nil {
		return err
	}
	return errors.New("injected failure after real service startup")
}

func TestGatewayPartialStartupRemovesRuntime(t *testing.T) {
	requireDockerImage(t)
	backend := &failAfterStartDeployment{Deployment: integrationDeployment(t)}
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:     backend,
		Image:          integrationImage,
		OutputDir:      t.TempDir(),
		CleanupTimeout: time.Minute,
		APIKeyLookup:   func(string) ([]byte, bool) { return []byte("sk-integration-not-a-real-key"), true },
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := manager.Stop(ctx); err != nil {
			t.Error(err)
		}
		_ = manager.Close()
	})
	request := testRequest(t)
	request.Connectivity.Placement = core.RuntimePlacement{AttachmentID: "bridge"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := manager.Start(ctx, request); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("startup error = %v", err)
	}
	if backend.id == "" {
		t.Fatal("failure did not exercise a real runtime")
	}
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	if _, err := api.ContainerInspect(ctx, backend.id, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("partial startup runtime remains: %v", err)
	}
}
