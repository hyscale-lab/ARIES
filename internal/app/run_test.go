package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

func TestNewRunIDContainsExperimentNameAndIsSafe(t *testing.T) {
	now := time.Date(2026, 7, 21, 2, 3, 4, 5, time.FixedZone("other", 3*60*60))
	id := newRunID(now, "profile")
	if id != "20260720T230304.000000005Z-profile" || strings.ContainsAny(id, `/\\`) {
		t.Fatalf("id=%q", id)
	}
}

func TestCloseOccurrenceClientsRunsReverseConstructionOrderAndJoinsErrors(t *testing.T) {
	first, second := errors.New("sandbox"), errors.New("harness")
	var events []string
	err := closeOccurrenceClients(func() error { events = append(events, "sandbox"); return first }, func() error { events = append(events, "harness"); return second })
	if !reflect.DeepEqual(events, []string{"sandbox", "harness"}) || !errors.Is(err, first) || !errors.Is(err, second) {
		t.Fatalf("events=%v err=%v", events, err)
	}
}

func TestCreateAndPersistRunResultUsesPrivateNoReplaceFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs", "id")
	if err := createRunOutputRoot(root); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(root)
	if info.Mode().Perm() != 0700 {
		t.Fatalf("root mode=%o", info.Mode().Perm())
	}
	path := filepath.Join(root, runResultName)
	if err := persistRunResult(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("file mode=%o", info.Mode().Perm())
	}
	if err := persistRunResult(path, []byte("second\n")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("replace err=%v", err)
	}
}

func TestAttachRunLogWritesPrivateStructuredRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	if err := createRunOutputRoot(root); err != nil {
		t.Fatal(err)
	}
	logger := newLogger()
	logger.SetOutput(io.Discard)
	detach, err := attachRunLog(logger, root)
	if err != nil {
		t.Fatal(err)
	}
	logger.WithField("task_id", "fix-git").Info("test record")
	if err := detach(); err != nil {
		t.Fatal(err)
	}
	logger.Info("after-close")
	content, err := os.ReadFile(filepath.Join(root, "aries.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(content, []byte(`"task_id":"fix-git"`)) || bytes.Contains(content, []byte("after-close")) {
		t.Fatalf("run log=%s", content)
	}
	info, err := os.Stat(filepath.Join(root, "aries.log"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info, err)
	}
}

func TestExecuteAndRecordWritesIdenticalJSON(t *testing.T) {
	root := filepath.Join(t.TempDir(), "run")
	if err := createRunOutputRoot(root); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := executeAndRecord(context.Background(), func(context.Context) (core.RunResult, error) { return core.RunResult{Name: "x", RunID: "r"}, nil }, root, &out)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(filepath.Join(root, runResultName))
	if !bytes.Equal(stored, out.Bytes()) {
		t.Fatalf("stored=%q stdout=%q", stored, out.Bytes())
	}
}

func TestSetupPreparesBackendBeforeSideEffects(t *testing.T) {
	root := t.TempDir()
	profile := filepath.Join(root, "profile.json")
	versions := filepath.Join(root, "versions.json")
	if err := os.WriteFile(versions, []byte(`{"terminalbench2":{"repository_url":"https://example.invalid/repo.git","revision":"0123456789abcdef0123456789abcdef01234567"},"openclaw":{"image":"ghcr.io/openclaw/openclaw:2026.7.1"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	content := `{"name":"test","versions_file":"versions.json","benchmark":{"type":"terminalbench2","root":"root","tasks":["a"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"runtime":{"backend":"deepseek","mode":"external"},"model":{"id":"m","base_url":"https://example.invalid","api_key_env":"KEY"},"output_dir":"runs"}`
	if err := os.WriteFile(profile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var events []string
	wiring := Wiring{PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
		events = append(events, "backend")
		return PreparedBackend{Model: cfg.CoreModel()}, nil
	}, ValidateComponents: func(config.Config) error { return nil }, SetupBenchmark: func(context.Context, config.Config) error {
		events = append(events, "setup")
		return nil
	}, LoadPreparationTasks: func(context.Context, config.Config, []string) ([]core.Task, error) {
		return nil, nil
	}, PullImages: func(context.Context, []string) error { return nil }}
	var out bytes.Buffer
	if err := RunCommand(context.Background(), []string{"setup", profile}, &out, Dependencies{Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"backend", "setup"}) || !strings.Contains(out.String(), "profile ready") {
		t.Fatalf("events=%v out=%q", events, out.String())
	}
}

func TestRunCommandRejectsInvalidGrammarWithoutUseCase(t *testing.T) {
	called := false
	wiring := Wiring{PrepareBackend: func(config.Config, string) (PreparedBackend, error) { called = true; return PreparedBackend{}, nil }}
	if err := RunCommand(context.Background(), nil, &bytes.Buffer{}, Dependencies{Wiring: wiring}); err == nil || called {
		t.Fatalf("err=%v called=%t", err, called)
	}
}

type stubBenchmark struct{}

func (*stubBenchmark) Tasks(context.Context) ([]core.Task, error)                      { return nil, nil }
func (*stubBenchmark) PrepareSandbox(context.Context, core.Task, runner.Sandbox) error { return nil }
func (*stubBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox) (core.Evaluation, error) {
	return core.Evaluation{}, nil
}

type stubHarness struct{}

func (*stubHarness) Start(context.Context, core.HarnessRequest) error { return nil }
func (*stubHarness) Run(context.Context, string) (core.HarnessResult, error) {
	return core.HarnessResult{}, nil
}
func (*stubHarness) Stop(context.Context) error { return nil }

type stubToolSandbox struct{}

func (*stubToolSandbox) Start(context.Context, core.SandboxRequest) (runner.Sandbox, error) {
	return nil, nil
}
func (*stubToolSandbox) Stop(context.Context, runner.Sandbox) error { return nil }

type stubBridge struct{}

func (*stubBridge) Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error) {
	return core.ToolEndpoint{}, nil
}
func (*stubBridge) Stop(context.Context) error { return nil }

type stubResources struct{}

func (*stubResources) Sample(context.Context) ([]core.ResourceReading, error) { return nil, nil }
func (*stubResources) Close() error                                           { return nil }

func TestBuildTaskExperimentCreatesFreshFourRoleGraphs(t *testing.T) {
	wiring := Wiring{
		NewBenchmark: func(config.Config, string, string, string) (runner.Benchmark, error) { return &stubBenchmark{}, nil },
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			h := &stubHarness{}
			return HarnessInstance{Harness: h, Close: func() error { return nil }}, nil
		},
		NewSandbox: func(config.Config, string, string, string, *logrus.Logger) (SandboxInstance, error) {
			s := &stubToolSandbox{}
			return SandboxInstance{Sandbox: s, Resources: &stubResources{}, Close: func() error { return nil }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return &stubBridge{}, nil },
	}
	cfg := config.Config{Name: "x"}
	model := core.ModelConfig{Provider: "deepseek", BaseURL: "https://example.invalid", Model: "m", APIKeyEnv: "KEY"}
	first, err := buildTaskExperiment(cfg, model, "run", t.TempDir(), "a", "a-001", nil, nil, wiring)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildTaskExperiment(cfg, model, "run", t.TempDir(), "a", "a-002", nil, nil, wiring)
	if err != nil {
		t.Fatal(err)
	}
	if first.runner == second.runner || first.recorder == second.recorder {
		t.Fatal("occurrences shared orchestration objects")
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildTaskExperimentUnwindsPartialConstruction(t *testing.T) {
	var events []string
	wiring := Wiring{
		NewBenchmark: func(config.Config, string, string, string) (runner.Benchmark, error) { return &stubBenchmark{}, nil },
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{Harness: &stubHarness{}, Close: func() error { events = append(events, "harness"); return errors.New("harness close") }}, nil
		},
		NewSandbox: func(config.Config, string, string, string, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{Sandbox: &stubToolSandbox{}, Resources: &stubResources{}, Close: func() error { events = append(events, "sandbox"); return errors.New("sandbox close") }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
			return nil, errors.New("bridge construct")
		},
	}
	_, err := buildTaskExperiment(config.Config{Name: "x"}, core.ModelConfig{}, "run", t.TempDir(), "a", "a-001", nil, nil, wiring)
	if err == nil || !strings.Contains(err.Error(), "bridge construct") || !strings.Contains(err.Error(), "sandbox close") || !strings.Contains(err.Error(), "harness close") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(events, []string{"sandbox", "harness"}) {
		t.Fatalf("events=%v", events)
	}
}
