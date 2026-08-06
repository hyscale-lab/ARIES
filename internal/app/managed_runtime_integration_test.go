package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

func TestConcreteManagedRuntimeWrapsPreflightAndTaskLifecycle(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		record(request.URL.Path)
		switch request.URL.Path {
		case "/health":
			writer.WriteHeader(http.StatusOK)
		case "/v1/models":
			if request.Header.Get("Authorization") != "Bearer integration-key" {
				t.Errorf("authorization=%q", request.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(writer, `{"data":[{"id":"managed-model"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	started := filepath.Join(root, "started")
	t.Setenv("STARTED", started)
	t.Setenv("SGLANG_API_KEY", "integration-key")
	executable := filepath.Join(root, "python helper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf started > \"$STARTED\"\ntrap 'exit 0' TERM\nsleep 60 & wait\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := writeManagedIntegrationProfile(t, root, server.URL, executable)

	var concrete *runtimesglang.Runtime
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, outputDir string) (PreparedBackend, error) {
			if _, err := runtimesglang.LoadNativeConfig(cfg.Runtime.Config.ResolvedFile, cfg.Model.ID, cfg.Model.BaseURL); err != nil {
				return PreparedBackend{}, err
			}
			var err error
			concrete, err = runtimesglang.New(runtimesglang.Options{Executable: cfg.Runtime.Config.Executable, ConfigPath: cfg.Runtime.Config.ResolvedFile, OutputDir: outputDir, BaseURL: cfg.Model.BaseURL})
			return PreparedBackend{Model: cfg.CoreModel(), Runtime: concrete}, err
		},
		SetupBenchmark:       func(context.Context, config.Config) error { record("prepare"); return nil },
		LoadPreparationTasks: func(context.Context, config.Config, []string) ([]core.Task, error) { return nil, nil },
		PullImages:           func(context.Context, []string) error { return nil },
		NewBenchmark: func(_ config.Config, _, _, occurrenceID string) (runner.Benchmark, error) {
			record("compose")
			return &managedIntegrationBenchmark{id: occurrenceID}, nil
		},
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{Harness: &stubHarness{}, Close: func() error { return nil }}, nil
		},
		NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{Sandbox: &managedIntegrationSandbox{}, Resources: &stubResources{}, Close: func() error { return nil }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
			return &stubBridge{}, nil
		},
	}
	var logs strings.Builder
	logger := logrus.New()
	logger.SetOutput(&logs)
	logger.SetFormatter(&logrus.JSONFormatter{})
	if err := Run(context.Background(), profile, io.Discard, Dependencies{Logger: logger, PreflightClient: server.Client(), Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	if concrete == nil {
		t.Fatal("concrete runtime was not constructed")
	}
	select {
	case <-concrete.Done():
	case <-time.After(time.Second):
		t.Fatal("runtime was not stopped")
	}
	if err := concrete.Err(); err != nil {
		t.Fatalf("intentional stop Err=%v", err)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("runtime process did not start: %v", err)
	}
	mu.Lock()
	gotEvents := append([]string(nil), events...)
	mu.Unlock()
	if !reflect.DeepEqual(gotEvents, []string{"prepare", "/health", "/v1/models", "compose"}) {
		t.Fatalf("events=%v", gotEvents)
	}
	content := logs.String()
	for _, state := range []string{"started", "healthy", "stopped"} {
		if !strings.Contains(content, `"runtime_state":"`+state+`"`) {
			t.Fatalf("missing lifecycle state %q: %s", state, content)
		}
	}
	for _, state := range []string{"unexpected_exit", "stop_failed"} {
		if strings.Contains(content, `"runtime_state":"`+state+`"`) {
			t.Fatalf("intentional stop logged lifecycle state %q: %s", state, content)
		}
	}
}

func TestConcreteManagedRuntimeNaturalExitLogsStoppedAfterUnexpectedExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/health":
			writer.WriteHeader(http.StatusOK)
		case "/v1/models":
			_, _ = io.WriteString(writer, `{"data":[{"id":"managed-model"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	exitNow := filepath.Join(root, "exit-now")
	t.Setenv("EXIT_NOW", exitNow)
	t.Setenv("SGLANG_API_KEY", "integration-key")
	executable := filepath.Join(root, "python helper")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nwhile [ ! -f \"$EXIT_NOW\" ]; do sleep 0.01; done\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := writeManagedIntegrationProfile(t, root, server.URL, executable)

	var concrete *runtimesglang.Runtime
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, outputDir string) (PreparedBackend, error) {
			var err error
			concrete, err = runtimesglang.New(runtimesglang.Options{Executable: cfg.Runtime.Config.Executable, ConfigPath: cfg.Runtime.Config.ResolvedFile, OutputDir: outputDir, BaseURL: cfg.Model.BaseURL, CredentialEnv: cfg.Model.APIKeyEnv})
			return PreparedBackend{Model: cfg.CoreModel(), Runtime: concrete}, err
		},
		SetupBenchmark:       func(context.Context, config.Config) error { return nil },
		LoadPreparationTasks: func(context.Context, config.Config, []string) ([]core.Task, error) { return nil, nil },
		PullImages:           func(context.Context, []string) error { return nil },
		NewBenchmark: func(_ config.Config, _, _, occurrenceID string) (runner.Benchmark, error) {
			if err := os.WriteFile(exitNow, []byte("exit"), 0o600); err != nil {
				return nil, err
			}
			select {
			case <-concrete.Done():
			case <-time.After(time.Second):
				return nil, errors.New("managed runtime did not exit")
			}
			return &managedIntegrationBenchmark{id: occurrenceID}, nil
		},
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{Harness: &stubHarness{}, Close: func() error { return nil }}, nil
		},
		NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{Sandbox: &managedIntegrationSandbox{}, Resources: &stubResources{}, Close: func() error { return nil }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
			return &stubBridge{}, nil
		},
	}
	var logs strings.Builder
	logger := logrus.New()
	logger.SetOutput(&logs)
	logger.SetFormatter(&logrus.JSONFormatter{})
	err := Run(context.Background(), profile, io.Discard, Dependencies{Logger: logger, PreflightClient: server.Client(), Wiring: wiring})
	if concrete == nil {
		t.Fatal("concrete runtime was not constructed")
	}
	if err == nil || !errors.Is(err, concrete.Err()) {
		t.Fatalf("Run=%v runtime Err=%v", err, concrete.Err())
	}
	content := logs.String()
	if !strings.Contains(content, `"runtime_state":"unexpected_exit"`) || !strings.Contains(content, `"runtime_state":"stopped"`) {
		t.Fatalf("missing lifecycle states: %s", content)
	}
	if strings.Contains(content, `"runtime_state":"stop_failed"`) {
		t.Fatalf("natural exit was logged as cleanup failure: %s", content)
	}
}

func writeManagedIntegrationProfile(t *testing.T, root, serverURL, executable string) string {
	t.Helper()
	endpoint, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(root, "native.yaml")
	nativeContent := fmt.Sprintf("model-path: managed-model\nserved-model-name: managed-model\nhost: 127.0.0.1\nport: %d\ndevice: cpu\ntensor-parallel-size: 1\ncontext-length: 1024\nmem-fraction-static: 0.5\nreasoning-parser: test\ntool-call-parser: test\n", port)
	if err := os.WriteFile(native, []byte(nativeContent), 0o600); err != nil {
		t.Fatal(err)
	}
	versions := filepath.Join(root, "versions.json")
	if err := os.WriteFile(versions, []byte(`{"terminalbench2":{"repository_url":"https://example.invalid/repo.git","revision":"0123456789abcdef0123456789abcdef01234567"},"openclaw":{"image":"ghcr.io/openclaw/openclaw:2026.7.1"},"hermes":{"image":"docker.io/nousresearch/hermes-agent:v2026.5.29.2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "profile.json")
	content := fmt.Sprintf(`{"name":"managed-integration","versions_file":%q,"benchmark":{"type":"terminalbench2","root":"unused","tasks":["task"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"runtime":{"backend":"sglang","mode":"managed","config":{"file":%q,"executable":%q,"startup_timeout":"2s","stop_timeout":"1s"}},"model":{"id":"managed-model","base_url":%q,"api_key_env":"SGLANG_API_KEY"},"output_dir":%q}`, versions, native, executable, serverURL+"/v1", filepath.Join(root, "runs"))
	if err := os.WriteFile(profile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return profile
}

type managedIntegrationSandbox struct{}

func (*managedIntegrationSandbox) Start(context.Context, core.SandboxRequest) (runner.Sandbox, error) {
	return &stubSandbox{}, nil
}

func (*managedIntegrationSandbox) Stop(context.Context, runner.Sandbox) error { return nil }

type managedIntegrationBenchmark struct{ id string }

func (benchmark *managedIntegrationBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{{ID: benchmark.id, Instruction: "run", Environment: core.Environment{Image: "image:tag", Workdir: "/"}}}, nil
}

func (*managedIntegrationBenchmark) PrepareSandbox(context.Context, core.Task, runner.Sandbox) error {
	return nil
}

func (*managedIntegrationBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox) (core.Evaluation, error) {
	return core.Evaluation{Status: core.StatusSucceeded}, nil
}
