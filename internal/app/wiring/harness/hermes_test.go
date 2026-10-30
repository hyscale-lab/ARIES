package harness

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	hermesharness "github.com/hyscale-lab/aries/pkg/harness/hermes"
)

// Preparation builds the image NewHermes runs, from the recipe, the context
// files and the pins in the version catalog.
func TestPrepareHermesImageBuildsTheTagHermesRuns(t *testing.T) {
	cfg := config.Config{}
	cfg.Versions.Hermes.Image = "test/hermes:v1"
	cfg.Versions.Hermes.OTelPlugin = config.HermesOTelPluginVersions{RepositoryURL: "https://example.invalid/hermes-otel", Version: "1.0.0", Revision: "0123456789abcdef0123456789abcdef01234567"}
	cfg.Versions.Hermes.GRPCIO = "1.81.1"
	var built string
	err := PrepareHermesImage(context.Background(), cfg, func(_ context.Context, image, dockerfile string, files map[string][]byte, buildArgs map[string]string) error {
		built = image
		if dockerfile != hermesharness.ImageDockerfile {
			t.Errorf("dockerfile = %q", dockerfile)
		}
		if !maps.EqualFunc(files, hermesharness.ImageContext(), func(a, b []byte) bool { return string(a) == string(b) }) {
			t.Errorf("context files = %v", maps.Keys(files))
		}
		if buildArgs["GRPCIO"] != "1.81.1" || buildArgs["BASE"] != "test/hermes:v1" {
			t.Errorf("build args = %#v", buildArgs)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want, err := hermesharness.LocalImage(cfg.Versions.Hermes.Image, hermesOTelPlugin(cfg), cfg.Versions.Hermes.GRPCIO)
	if err != nil {
		t.Fatal(err)
	}
	if built != want {
		t.Fatalf("built %q, Hermes runs %q", built, want)
	}
}

// Older version catalogs can omit grpcio, but Hermes image preparation must
// reject the missing pin before the builder starts.
func TestPrepareHermesImageRequiresGRPCIOBeforeBuild(t *testing.T) {
	cfg := config.Config{}
	cfg.Versions.Hermes.Image = "test/hermes:v1"
	cfg.Versions.Hermes.OTelPlugin = config.HermesOTelPluginVersions{RepositoryURL: "https://example.invalid/hermes-otel", Version: "1.0.0", Revision: "0123456789abcdef0123456789abcdef01234567"}
	called := false
	err := PrepareHermesImage(context.Background(), cfg, func(context.Context, string, string, map[string][]byte, map[string]string) error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "hermes.grpcio") || called {
		t.Fatalf("prepare error = %v, builder called = %t", err, called)
	}
}
