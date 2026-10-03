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

func TestDeploymentPreflightRejectsKubernetesByComponent(t *testing.T) {
	for _, component := range []string{"harness", "sandbox"} {
		t.Run(component, func(t *testing.T) {
			var cfg config.Config
			if err := json.Unmarshal([]byte(`{"benchmark":{"type":"terminalbench2"},"harness":{"type":"hermes"},"sandbox":{},"bridge":{"type":"hermes-ssh","mode":"embedded"}}`), &cfg); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(`{"`+component+`":{"deployment":{"backend":"kubernetes","kubernetes":{"context":"test","namespace":"aries","runtime_class_name":"kata"}}}}`), &cfg); err != nil {
				t.Fatal(err)
			}
			err := validateComponents(cfg)
			if err == nil || !strings.Contains(err.Error(), component+".deployment") || !strings.Contains(err.Error(), "not implemented") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestKubernetesStopsRunAndSetupBeforeSideEffects(t *testing.T) {
	for _, component := range []string{"harness", "sandbox"} {
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
				profile[component].(map[string]any)["deployment"] = map[string]any{"backend": "kubernetes", "kubernetes": map[string]any{"namespace": "aries"}}
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
				if err == nil || !strings.Contains(err.Error(), component+".deployment") || !strings.Contains(err.Error(), "not implemented") || effects != 0 {
					t.Fatalf("error=%v effects=%d", err, effects)
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("output created: %v", err)
				}
			})
		}
	}
}
