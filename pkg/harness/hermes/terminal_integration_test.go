//go:build integration

package hermes

import (
	"context"
	"encoding/json"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise terminal workdir and spans through the native Gateway and real bridge.
func TestTerminalCallWithoutWorkdirRunsInTheEndpointWorkdir(t *testing.T) {
	const base = "docker.io/nousresearch/hermes-agent:v2026.8.31"
	t.Run("base", func(t *testing.T) { runHermesBridgeScenario(t, false, 1, false) })
	t.Run("derived", func(t *testing.T) { runHermesBridgeScenario(t, false, 1, true, preparedImage(t, base)) })
}

// preparedImage builds, or reuses from the local cache, the image ARIES runs
// for base, exactly as preparation does before a run.
func preparedImage(t *testing.T, base string) string {
	t.Helper()
	// The plugin pin is the one the version catalog ships.
	content, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "versions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Hermes struct {
			OTelPlugin struct {
				RepositoryURL string `json:"repository_url"`
				Version       string `json:"version"`
				Revision      string `json:"revision"`
			} `json:"otel_plugin"`
		} `json:"hermes"`
	}
	if err := json.Unmarshal(content, &catalog); err != nil {
		t.Fatal(err)
	}
	pin := catalog.Hermes.OTelPlugin
	plugin := OTelPlugin{RepositoryURL: pin.RepositoryURL, Version: pin.Version, Revision: pin.Revision}
	image, err := LocalImage(base, plugin)
	if err != nil {
		t.Fatal(err)
	}
	args, err := OTelBuildArgs(base, plugin)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := dockerdeployment.BuildImage(ctx, "", image, OTelDockerfile, args); err != nil {
		t.Fatal(err)
	}
	return image
}

// assertToolSpan requires a span named name for the tool call id whose
// wall-clock window is well formed.
func assertToolSpan(t *testing.T, path, name, callID string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		var span struct {
			Name       string         `json:"name"`
			Start      int64          `json:"start_time_unix_nano"`
			End        int64          `json:"end_time_unix_nano"`
			Attributes map[string]any `json:"attributes"`
		}
		if err := json.Unmarshal([]byte(line), &span); err != nil {
			t.Fatalf("decode span: %v\n%s", err, line)
		}
		if span.Name != name || span.Attributes["gen_ai.tool.call.id"] != callID {
			continue
		}
		if span.Start <= 0 || span.End < span.Start {
			t.Fatalf("span window is malformed: %s", line)
		}
		return
	}
	t.Fatalf("no %s span for %s in:\n%s", name, callID, content)
}
