package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hyscale-lab/aries/internal/app"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
)

func TestDeploymentPreflightRejectsUnsupportedBackends(t *testing.T) {
	for _, component := range []string{"harness", "sandbox", "bridge"} {
		t.Run(component, func(t *testing.T) {
			var cfg config.Config
			if err := json.Unmarshal([]byte(`{"benchmark":{"type":"terminalbench2"},"harness":{"type":"hermes"},"sandbox":{},"bridge":{"type":"hermes-ssh","mode":"managed"}}`), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(`{"`+component+`":{"deployment":{"backend":"remote"}}}`), &cfg); err != nil {
				t.Fatal(err)
			}
			err := validateComponents(cfg)
			if err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestUnsupportedBackendsStopRunAndSetupBeforeSideEffects(t *testing.T) {
	for _, component := range []string{"harness", "sandbox", "bridge"} {
		for _, command := range []struct {
			name string
			run  func(context.Context, string, io.Writer, app.Dependencies) error
		}{{"run", app.Run}, {"setup", app.Setup}} {
			t.Run(component+"/"+command.name, func(t *testing.T) {
				source, err := os.ReadFile("../../profiles/hermes-tb2-fix-git-deepseek.json")
				if err != nil {
					t.Fatal(err)
				}
				var profile map[string]any
				if err := json.Unmarshal(source, &profile); err != nil {
					t.Fatal(err)
				}
				versions, err := filepath.Abs("../../configs/versions.json")
				if err != nil {
					t.Fatal(err)
				}
				profile["versions_file"] = versions
				output := filepath.Join(t.TempDir(), "never-created")
				profile["output_dir"] = output
				profile[component].(map[string]any)["deployment"] = map[string]any{"backend": "remote"}
				data, err := json.Marshal(profile)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "profile.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				wiring := commandWiring()
				// Preparation is the first externally effective operation after preflight.
				effects := 0
				wiring.PrepareBackend = func(config.Config, string) (app.PreparedBackend, error) {
					effects++
					return app.PreparedBackend{}, errors.New("unexpected preparation")
				}
				err = command.run(context.Background(), path, io.Discard, app.Dependencies{Wiring: wiring})
				if err == nil || !strings.Contains(err.Error(), "unsupported") || effects != 0 {
					t.Fatalf("error=%v effects=%d", err, effects)
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("output created: %v", err)
				}
			})
		}
	}
}

func TestSupportedBridgePlacements(t *testing.T) {
	for _, backend := range []string{"docker"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.Config{
				Benchmark: config.BenchmarkConfig{Type: "terminalbench2"},
				Harness:   config.HarnessConfig{Type: "hermes"},
				Bridge:    config.BridgeConfig{Type: "hermes-ssh", Deployment: config.DeploymentConfig{Backend: backend}},
				Versions:  config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}},
			}

			if err := validateComponents(cfg); err != nil {
				t.Fatalf("supported placement rejected: %v", err)
			}

		})
	}
}
