package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// The Kubernetes provider is selected by the same switches as Docker, and only
// as a whole: a component left on another backend could not be reached.
func TestKubernetesBackendSelectsItsProviderForEveryComponent(t *testing.T) {
	source, err := os.ReadFile("../../profiles/kubernetes/openclaw-tb2-fix-git-deepseek-k8s-pods.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg config.Config
	if err := json.Unmarshal(source, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Versions.Bridge.Image = "aries-bridge:v1"
	// A test binary carries no commit; the bridge image tag needs one.
	previous := bridgeRevision
	bridgeRevision = "0123456789ab"
	t.Cleanup(func() { bridgeRevision = previous })
	if err := validateComponents(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.NormalizeDeployment(); err != nil {
		t.Fatal(err)
	}
	for name, construct := range map[string]func() (interface{ Close() error }, error){
		"harness": func() (interface{ Close() error }, error) { return newHarnessDeployment(cfg, nil) },
		"bridge":  func() (interface{ Close() error }, error) { return newDeployment(cfg.Bridge.Deployment, nil) },
	} {
		transport, err := construct()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if backend, ok := transport.(interface{ BridgeBackend() string }); !ok || backend.BridgeBackend() != "kubernetes" {
			t.Fatalf("%s: transport %T is not the Kubernetes provider", name, transport)
		}
		if err := transport.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// Preparation builds nothing: the cluster pulls its own images.
	if err := prepareBridge(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := pullImages(context.Background(), cfg, []string{"example.test/task:1"}); err != nil {
		t.Fatal(err)
	}
	infrastructure, err := newInfrastructure(cfg, "run-1", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if infrastructure.NetworkPolicy != "per-sandbox-policy" || infrastructure.Environment == nil || infrastructure.Resources == nil {
		t.Fatalf("infrastructure = %+v", infrastructure)
	}
	if err := errors.Join(infrastructure.Resources.Close(), infrastructure.Close()); err != nil {
		t.Fatal(err)
	}

	cfg.Bridge.Deployment = config.DeploymentConfig{Backend: "docker"}
	if err := validateComponents(cfg); err == nil || !strings.Contains(err.Error(), "all use the kubernetes deployment backend") {
		t.Fatalf("mixed backends = %v", err)
	}
}

// Naming a Prometheus server on the Docker sandbox moves the run's
// measurements, bridge included, onto the cAdvisor series the Kubernetes
// backend is measured from.
func TestDockerPrometheusURLSelectsTheCAdvisorSource(t *testing.T) {
	for _, test := range []struct {
		prometheus string
		want       string
	}{{"", "*docker.DockerResourceSource"}, {"http://10.96.1.2:9090", "*docker.PrometheusResourceSource"}} {
		cfg := config.Config{Versions: config.Versions{Bridge: config.BridgeVersions{Image: "aries-bridge:v1"}}}
		cfg.Sandbox.Deployment = config.DeploymentConfig{Backend: "docker", Docker: &config.DockerDeploymentConfig{PrometheusURL: test.prometheus}}
		if err := cfg.NormalizeDeployment(); err != nil {
			t.Fatal(err)
		}
		infrastructure, err := newInfrastructure(cfg, "run-1", t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%T", infrastructure.Resources); got != test.want {
			t.Fatalf("prometheus %q: resources %s, want %s", test.prometheus, got, test.want)
		}
		if err := errors.Join(infrastructure.Resources.Close(), infrastructure.Close()); err != nil {
			t.Fatal(err)
		}
	}
}
