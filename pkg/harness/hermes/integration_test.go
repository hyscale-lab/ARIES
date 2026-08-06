//go:build integration

package hermes

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

const integrationImage = "docker.io/nousresearch/hermes-agent:v2026.5.29.2"

func requireDockerImage(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", integrationImage).CombinedOutput(); err != nil {
		t.Skipf("pinned Hermes image is not present locally (%s): %s", integrationImage, output)
	}
}

func integrationManager(t *testing.T, outputDir string) *Manager {
	t.Helper()
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	manager, err := New(Options{
		Image: integrationImage, OutputDir: outputDir, Logger: logger,
		StartTimeout: 90 * time.Second, AgentTimeout: 90 * time.Second, CleanupTimeout: 60 * time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return []byte("sk-integration-not-a-real-key"), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

// TestHarnessStartsRealHermesContainerAndStopsPositively drives the real
// upstream image: the staged runtime must satisfy the readiness probe (Hermes
// CLI present, ssh present, config and key readable), and Stop must confirm the
// container is gone. No model is contacted.
func TestHarnessStartsRealHermesContainerAndStopsPositively(t *testing.T) {
	requireDockerImage(t)
	outputDir := t.TempDir()
	manager := integrationManager(t, outputDir)

	identityDir := t.TempDir()
	identityPath := filepath.Join(identityDir, "id_ed25519")
	if err := os.WriteFile(identityPath, []byte("-----BEGIN PRIVATE KEY-----\nintegration\n-----END PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(identityPath, 0o600); err != nil {
		t.Fatal(err)
	}
	request := core.HarnessRequest{
		RunID: "integration-run", TaskID: "integration-task",
		Endpoint: core.ToolEndpoint{
			Protocol: "ssh", Address: "127.0.0.1:2222", Username: "aries", Network: "bridge",
			IdentityFile: identityContainerFS, IdentitySourceFile: identityPath,
		},
		Model: core.ModelConfig{Provider: "deepseek", BaseURL: "https://api.deepseek.com", Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := manager.Start(ctx, request); err != nil {
		t.Fatalf("start real Hermes container: %v", err)
	}
	containerName := manager.active.containerName

	// The staged runtime must be exactly what the container sees.
	for _, check := range []struct {
		path string
		want string
	}{
		{configContainerPath, "${DEEPSEEK_API_KEY}"},
		{modelKeyPath, "sk-integration-not-a-real-key"},
		{agentWrapperPath, "exec hermes --ignore-rules --yolo"},
	} {
		output, err := exec.CommandContext(ctx, "docker", "exec", containerName, "cat", check.path).CombinedOutput()
		if err != nil {
			t.Fatalf("read %s: %v (%s)", check.path, err, output)
		}
		if !strings.Contains(string(output), check.want) {
			t.Fatalf("%s does not contain %q:\n%s", check.path, check.want, output)
		}
	}
	// Hermes itself must accept the staged configuration.
	output, err := exec.CommandContext(ctx, "docker", "exec", containerName, "hermes", "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("hermes --version failed: %v (%s)", err, output)
	}
	if !strings.Contains(string(output), "Hermes Agent") {
		t.Fatalf("unexpected hermes version output: %s", output)
	}

	if err := manager.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Positive absence.
	if output, err := exec.CommandContext(ctx, "docker", "inspect", containerName).CombinedOutput(); err == nil {
		t.Fatalf("container %s survived Stop: %s", containerName, output)
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
