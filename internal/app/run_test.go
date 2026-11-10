package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

func writeReplayCommandProfile(t *testing.T, output string) string {
	t.Helper()
	path := writeCommandProfile(t, output)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := strings.Replace(string(data), `"harness":{"type":"openclaw"}`, `"replay":{"source":"source-run/a-001","deployment":{"backend":"docker"}}`, 1)
	profile = strings.Replace(profile, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"hermes-grpc"}`, 1)
	profile = strings.Replace(profile, `,"runtime":{"backend":"deepseek","mode":"external"}`, "", 1)
	profile = strings.Replace(profile, `,"model":{"id":"deepseek-v4-flash","base_url":"https://api.deepseek.com","api_key_env":"DEEPSEEK_API_KEY"}`, "", 1)
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReplaySetupPreparesTaskWithoutModelBackend(t *testing.T) {
	path := writeReplayCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	var prepared []string
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(config.Config, string) (PreparedBackend, error) {
			t.Fatal("setup prepared model backend")
			return PreparedBackend{}, nil
		},
		SetupBenchmark: func(context.Context, config.Config) error { prepared = append(prepared, "benchmark"); return nil },
		LoadBenchmark: testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
			return []core.Task{{Environment: core.Environment{Image: "task:tag"}}}, nil
		}, nil),
		PullImages: func(_ context.Context, _ config.Config, images []string) error {
			prepared = append(prepared, strings.Join(images, ","))
			return nil
		},
	}
	if err := Setup(context.Background(), path, io.Discard, Dependencies{Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared, []string{"benchmark", "task:tag"}) {
		t.Fatalf("preparation = %v", prepared)
	}
}

func TestReplayRunPreparesTaskWithoutLiveModelOrHarness(t *testing.T) {
	profile := writeReplayCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	stop := errors.New("infrastructure stopped before sandbox")
	var events []string
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(config.Config, string) (PreparedBackend, error) {
			t.Fatal("replay prepared model backend")
			return PreparedBackend{}, nil
		},
		PrepareBridge:  func(context.Context, config.Config) error { events = append(events, "bridge"); return nil },
		SetupBenchmark: func(context.Context, config.Config) error { events = append(events, "benchmark"); return nil },
		LoadBenchmark: testBenchmarkLoader(func(_ context.Context, _ config.Config, taskIDs []string, lookup func(string) ([]byte, bool)) ([]core.Task, error) {
			if !reflect.DeepEqual(taskIDs, []string{"a"}) || lookup != nil {
				t.Fatalf("task IDs = %v, has lookup = %v", taskIDs, lookup != nil)
			}
			events = append(events, "catalog")
			return []core.Task{{ID: "a", Environment: core.Environment{Image: "task:tag"}}}, nil
		}, nil),
		PullImages: func(_ context.Context, _ config.Config, images []string) error {
			events = append(events, "images:"+strings.Join(images, ","))
			return nil
		},
		BuildHarnessImage: func(context.Context, config.Config) error { t.Fatal("replay built harness image"); return nil },
		NewInfrastructure: func(_ config.Config, _, _ string, captureReplay bool, _ *logrus.Logger) (*RunInfrastructure, error) {
			if !captureReplay {
				t.Fatal("replay did not enable evidence capture")
			}
			events = append(events, "infrastructure")
			return nil, stop
		},
	}
	if err := Run(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring}); !errors.Is(err, stop) {
		t.Fatalf("Run error = %v, want infrastructure stop", err)
	}
	want := []string{"bridge", "benchmark", "catalog", "images:task:tag", "infrastructure"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

type replayTaskBenchmark struct {
	stubBenchmark
	task core.Task
}

func (b *replayTaskBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{b.task}, nil
}

type stubReplayCaller struct{ stops int }

func (*stubReplayCaller) Start(context.Context, core.ToolEndpoint, core.HarnessConnectivity, string, string) error {
	return nil
}
func (*stubReplayCaller) Run(context.Context, replay.Plan) error { return nil }
func (caller *stubReplayCaller) Stop(context.Context) error {
	caller.stops++
	return nil
}

func TestReplayTaskBindsLogicalCatalogTaskAndLiveTimeout(t *testing.T) {
	failure := errors.New("source not ready")
	invalidTimeout := errors.New("negative caller timeout")
	for _, tc := range []struct {
		name, taskID, benchmarkType string
		configured, override, want  time.Duration
	}{
		{name: "task timeout", taskID: "logical", configured: time.Minute, want: time.Minute},
		{name: "override", taskID: "logical", configured: time.Minute, override: time.Minute + time.Minute, want: time.Minute + time.Minute},
		{name: "deep research zero timeout", taskID: "logical", benchmarkType: "deepresearchbench"},
		{name: "negative task timeout", taskID: "logical", configured: -time.Minute, want: -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			benchmarkType := tc.benchmarkType
			if benchmarkType == "" {
				benchmarkType = "terminalbench2"
			}
			cfg := config.Config{Name: "replay", Benchmark: config.BenchmarkConfig{Type: benchmarkType}, Replay: &config.ReplayConfig{Source: "source-run/logical-001"}}
			if tc.override > 0 {
				cfg.Overrides.AgentTimeout = &tc.override
			}
			task := core.Task{ID: tc.taskID, Timeout: tc.configured, Environment: core.Environment{Image: "task:tag"}}
			catalog := PreparedBenchmark{Tasks: []core.Task{task}, NewOccurrence: func(logicalID, occurrenceID string) (runner.Benchmark, error) {
				if logicalID != tc.taskID || occurrenceID != "logical-001" {
					t.Fatalf("task binding = %q, %q", logicalID, occurrenceID)
				}
				return &replayTaskBenchmark{task: task}, nil
			}}
			caller := &stubReplayCaller{}
			wiring := Wiring{
				NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
					return SandboxInstance{Sandbox: &stubToolSandbox{}, Resources: &stubResources{}}, nil
				},
				NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return &stubBridge{}, nil },
				NewReplayCaller: func(_ config.Config, _ core.RuntimePlacement, timeout time.Duration, _ *logrus.Logger) (runner.ReplayCaller, error) {
					if timeout != tc.want {
						t.Fatalf("caller timeout = %s, want %s", timeout, tc.want)
					}
					if timeout < 0 {
						return nil, invalidTimeout
					}
					return caller, nil
				},
				LoadReplaySource: func(_ context.Context, loaded config.Config, selected core.Task) (replay.Plan, error) {
					if loaded.Replay.Source != cfg.Replay.Source || !reflect.DeepEqual(selected, task) {
						t.Fatalf("replay source selected wrong logical task: %+v", selected)
					}
					return replay.Plan{}, failure
				},
			}
			experiment, err := buildTaskExperiment(cfg, core.ModelConfig{}, nil, "run", t.TempDir(), tc.taskID, "logical-001", nil, nil, catalog, wiring)
			if tc.want < 0 {
				if !errors.Is(err, invalidTimeout) || experiment != nil {
					t.Fatalf("negative timeout accepted: experiment = %v, error = %v", experiment, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := experiment.Run(context.Background()); !errors.Is(err, failure) {
				t.Fatalf("replay source error = %v, want %v", err, failure)
			}
			if caller.stops != 1 {
				t.Fatalf("replay caller stopped %d times after source failure", caller.stops)
			}
		})
	}
}

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
	if err := os.WriteFile(versions, []byte(`{"terminalbench2":{"repository_url":"https://example.invalid/repo.git","revision":"0123456789abcdef0123456789abcdef01234567"},"deepresearchbench":{"repository_url":"https://example.invalid/deep-research-bench.git","revision":"fedcba9876543210fedcba9876543210fedcba98"},"sweatlasqa":{"repository_url":"https://example.invalid/swe-atlas.git","revision":"1111111111111111111111111111111111111111"},"swebenchpro":{"dataset_repository_url":"https://example.invalid/swe-bench-pro-data.git","dataset_revision":"1111111111111111111111111111111111111111","evaluator_repository_url":"https://example.invalid/swe-bench-pro-evaluator.git","evaluator_revision":"2222222222222222222222222222222222222222"},"openclaw":{"image":"ghcr.io/openclaw/openclaw:2026.7.1"},"hermes":{"image":"docker.io/nousresearch/hermes-agent:v2026.5.29.2"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	content := `{"name":"test","versions_file":"versions.json","benchmark":{"type":"terminalbench2","root":"root","tasks":["a"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"runtime":{"backend":"deepseek","mode":"external"},"model":{"id":"m","base_url":"https://example.invalid","api_key_env":"KEY"},"output_dir":"runs"}`
	if err := os.WriteFile(profile, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var events []string
	runtimeEvents := []string{}
	runtime := &recordingRuntime{events: &runtimeEvents, done: make(chan struct{})}
	wiring := Wiring{PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
		events = append(events, "backend")
		return PreparedBackend{Model: cfg.CoreModel(), Runtime: runtime}, nil
	}, ValidateComponents: func(config.Config) error { return nil }, SetupBenchmark: func(context.Context, config.Config) error {
		events = append(events, "setup")
		return nil
	}, LoadBenchmark: testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
		return nil, nil
	}, nil), BuildHarnessImage: func(context.Context, config.Config) error { return nil }, PullImages: func(context.Context, config.Config, []string) error { return nil }}
	var out bytes.Buffer
	if err := Setup(context.Background(), profile, &out, Dependencies{Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"backend", "setup"}) || !strings.Contains(out.String(), "profile ready") {
		t.Fatalf("events=%v out=%q", events, out.String())
	}
	if len(runtimeEvents) != 0 {
		t.Fatalf("setup took runtime ownership: %v", runtimeEvents)
	}
}

func TestRunEnsuresPreparationBeforeOutputAndRuntime(t *testing.T) {
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	cfg, err := config.Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	runtime := &recordingRuntime{events: &events, done: make(chan struct{})}
	prepareErr := errors.New("image preparation canary")
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
			events = append(events, "backend")
			return PreparedBackend{Model: cfg.CoreModel(), Runtime: runtime}, nil
		},
		SetupBenchmark: func(context.Context, config.Config) error {
			events = append(events, "benchmark")
			return nil
		},
		LoadBenchmark: testBenchmarkLoader(func(_ context.Context, _ config.Config, taskIDs []string, _ func(string) ([]byte, bool)) ([]core.Task, error) {
			events = append(events, "tasks:"+strings.Join(taskIDs, ","))
			return []core.Task{{Environment: core.Environment{Image: "task:tag"}}}, nil
		}, nil),
		BuildHarnessImage: func(context.Context, config.Config) error { return nil },
		PullImages: func(_ context.Context, _ config.Config, images []string) error {
			events = append(events, "images:"+strings.Join(images, ","))
			return prepareErr
		},
	}
	err = runCommandForTest(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring})
	if !errors.Is(err, prepareErr) {
		t.Fatalf("Run error=%v", err)
	}
	want := []string{"backend", "benchmark", "tasks:a", "images:" + cfg.Versions.OpenClaw.Image + ",task:tag"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
	if _, statErr := os.Stat(cfg.OutputDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("run output created before preparation: %v", statErr)
	}
}

// The harness image is built after every image is pulled, since it may be
// derived from the pinned one, and its failure stops the run.
func TestHarnessImageIsBuiltAfterPullAndFailureStopsRun(t *testing.T) {
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	buildErr := errors.New("harness image build failed")
	var events []string
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
			return PreparedBackend{Model: cfg.CoreModel()}, nil
		},
		SetupBenchmark: func(context.Context, config.Config) error { return nil },
		LoadBenchmark: testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
			return nil, nil
		}, nil),
		PullImages: func(context.Context, config.Config, []string) error {
			events = append(events, "pull")
			return nil
		},
		BuildHarnessImage: func(context.Context, config.Config) error {
			events = append(events, "build")
			return buildErr
		},
	}
	if err := runCommandForTest(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring}); !errors.Is(err, buildErr) {
		t.Fatalf("Run error=%v", err)
	}
	if want := []string{"pull", "build"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestSetupRetriesIncompletePreparationWithoutReadinessMarker(t *testing.T) {
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	pullCalls := 0
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
			return PreparedBackend{Model: cfg.CoreModel()}, nil
		},
		SetupBenchmark: func(context.Context, config.Config) error { return nil },
		LoadBenchmark: testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
			return nil, nil
		}, nil),
		BuildHarnessImage: func(context.Context, config.Config) error { return nil },
		PullImages: func(context.Context, config.Config, []string) error {
			pullCalls++
			if pullCalls == 1 {
				return errors.New("partial pull")
			}
			return nil
		},
	}
	if err := Setup(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring}); err == nil {
		t.Fatal("first setup unexpectedly succeeded")
	}
	if err := Setup(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	if pullCalls != 2 {
		t.Fatalf("pull calls=%d", pullCalls)
	}
}

func TestRunCancellationDuringPreparationHasNoDownstreamEffects(t *testing.T) {
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	cfg, err := config.Load(profile)
	if err != nil {
		t.Fatal(err)
	}
	downstream := 0
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
			return PreparedBackend{Model: cfg.CoreModel()}, nil
		},
		SetupBenchmark: func(ctx context.Context, _ config.Config) error { return ctx.Err() },
		LoadBenchmark: testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
			downstream++
			return nil, nil
		}, func(config.Config, string, string, string, func(string) ([]byte, bool)) (runner.Benchmark, error) {
			downstream++
			return nil, nil
		}),
		BuildHarnessImage: func(context.Context, config.Config) error { return nil },
		PullImages:        func(context.Context, config.Config, []string) error { downstream++; return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runCommandForTest(ctx, profile, io.Discard, Dependencies{Wiring: wiring}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v", err)
	}
	if downstream != 0 {
		t.Fatalf("downstream effects=%d", downstream)
	}
	if _, statErr := os.Stat(cfg.OutputDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("run output created during canceled preparation: %v", statErr)
	}
}

type stubBenchmark struct{}

func (*stubBenchmark) Tasks(context.Context) ([]core.Task, error)                      { return nil, nil }
func (*stubBenchmark) PrepareSandbox(context.Context, core.Task, runner.Sandbox) error { return nil }
func (*stubBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox, runner.EvaluationSandboxes) (core.Evaluation, error) {
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

type sourceStubBridge struct {
	stubBridge
	logicalID   string
	image       string
	taskImage   string
	completions int
}

func (b *sourceStubBridge) RecordSource(logicalID, image string) {
	b.logicalID, b.image = logicalID, image
}

func (b *sourceStubBridge) RecordTask(image string) { b.taskImage = image }

func (b *sourceStubBridge) CompleteHarness(context.Context) error {
	b.completions++
	return nil
}

func TestTaskExperimentRequiresRecordingMethodsOnlyForRecording(t *testing.T) {
	type sourceRecorder interface{ RecordSource(string, string) }
	type taskRecorder interface{ RecordTask(string) }
	type harnessCompleter interface{ CompleteHarness(context.Context) error }
	for _, test := range []struct {
		name      string
		recording bool
		missing   string
		wrap      func(*sourceStubBridge) runner.ToolBridge
	}{
		{name: "missing source", recording: true, missing: "RecordSource", wrap: func(b *sourceStubBridge) runner.ToolBridge {
			return struct {
				runner.ToolBridge
				taskRecorder
				harnessCompleter
			}{b, b, b}
		}},
		{name: "missing task", recording: true, missing: "RecordTask", wrap: func(b *sourceStubBridge) runner.ToolBridge {
			return struct {
				runner.ToolBridge
				sourceRecorder
				harnessCompleter
			}{b, b, b}
		}},
		{name: "missing completion", recording: true, missing: "CompleteHarness", wrap: func(b *sourceStubBridge) runner.ToolBridge {
			return struct {
				runner.ToolBridge
				sourceRecorder
				taskRecorder
			}{b, b, b}
		}},
		{name: "supported recording", recording: true, wrap: func(b *sourceStubBridge) runner.ToolBridge { return b }},
		{name: "ordinary run", wrap: func(b *sourceStubBridge) runner.ToolBridge { return &b.stubBridge }},
	} {
		t.Run(test.name, func(t *testing.T) {
			const logicalID = "task-logical"
			const occurrenceID = "task-logical-001"
			const image = "task-image:tag"
			const oneCompletion = 1
			task := core.Task{ID: occurrenceID, Environment: core.Environment{Image: image}}
			bridge := &sourceStubBridge{}
			var events []string
			resources := &infrastructureResources{events: &events}
			closeErr := errors.New("transport close failed")
			closeClient := func(name string) error {
				events = append(events, name)
				if test.missing != "" {
					return closeErr
				}
				return nil
			}
			wiring := Wiring{recordForReplay: test.recording,
				NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
					return HarnessInstance{Harness: &stubHarness{}, Close: func() error { return closeClient("harness-close") }}, nil
				},
				NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
					return SandboxInstance{Sandbox: &cancelSandbox{events: &events}, Resources: resources, Close: func() error { return closeClient("sandbox-close") }}, nil
				},
				NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return test.wrap(bridge), nil },
			}
			catalog := PreparedBenchmark{
				Tasks:         []core.Task{{ID: logicalID, Environment: task.Environment}},
				NewOccurrence: func(_, _ string) (runner.Benchmark, error) { return &replayTaskBenchmark{task: task}, nil },
			}
			experiment, err := buildTaskExperiment(config.Config{Name: "fixture"}, core.ModelConfig{}, nil, "run", t.TempDir(), logicalID, occurrenceID, nil, nil, catalog, wiring)
			if test.missing != "" {
				if experiment != nil || err == nil || !strings.Contains(err.Error(), test.missing) || !errors.Is(err, closeErr) {
					t.Fatalf("missing %s accepted or cleanup error lost: experiment=%v error=%v", test.missing, experiment, err)
				}
				if bridge.logicalID != "" || bridge.taskImage != "" || bridge.completions != 0 || !reflect.DeepEqual(events, []string{"observer-close", "sandbox-close", "harness-close"}) {
					t.Fatalf("invalid recording caused task effects: bridge=%+v events=%v", bridge, events)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, err := experiment.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := result.Tasks[0]
			if got.Isolation.Status != core.StatusConfirmed || got.Evaluation.Status != core.StatusSucceeded || got.Cleanup.Status != core.StatusSucceeded {
				t.Fatalf("task result = %+v", got)
			}
			if test.recording {
				if bridge.logicalID != logicalID || bridge.image != image || bridge.taskImage != image || bridge.completions != oneCompletion {
					t.Fatalf("recording methods did not receive task evidence: %+v", bridge)
				}
			} else if bridge.logicalID != "" || bridge.taskImage != "" || bridge.completions != 0 {
				t.Fatalf("ordinary run used recording methods: %+v", bridge)
			}
		})
	}
}

func TestTaskExperimentBindsCatalogIdentityNotOccurrenceIdentity(t *testing.T) {
	const logicalID = "task-logical"
	const occurrenceID = "task-logical-001"
	const image = "task-image:tag"
	bridge := &sourceStubBridge{}
	wiring := Wiring{recordForReplay: true,
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{Harness: &stubHarness{}}, nil
		},
		NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{Sandbox: &stubToolSandbox{}, Resources: &stubResources{}}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return bridge, nil },
	}
	catalog := PreparedBenchmark{
		Tasks:         []core.Task{{ID: logicalID, Environment: core.Environment{Image: image}}},
		NewOccurrence: func(_, _ string) (runner.Benchmark, error) { return &stubBenchmark{}, nil },
	}
	_, err := buildTaskExperiment(config.Config{Name: "fixture"}, core.ModelConfig{}, nil, "run", t.TempDir(), logicalID, occurrenceID, nil, nil, catalog, wiring)
	if err != nil {
		t.Fatal(err)
	}
	if bridge.logicalID != logicalID || bridge.image != image || bridge.logicalID == occurrenceID {
		t.Fatalf("source binding = %q, %q", bridge.logicalID, bridge.image)
	}
}

func TestBuildTaskExperimentCreatesFreshFourRoleGraphs(t *testing.T) {
	wiring := Wiring{
		LoadBenchmark: testBenchmarkLoader(nil, func(config.Config, string, string, string, func(string) ([]byte, bool)) (runner.Benchmark, error) {
			return &stubBenchmark{}, nil
		}),
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			h := &stubHarness{}
			return HarnessInstance{Harness: h, Close: func() error { return nil }}, nil
		},
		NewSandbox: func(_ config.Config, _, _, _ string, gpuIndices []int, _ *logrus.Logger) (SandboxInstance, error) {
			if !reflect.DeepEqual(gpuIndices, []int{4, 2}) {
				t.Fatalf("GPU indices = %v", gpuIndices)
			}
			gpuIndices[0] = 99
			s := &stubToolSandbox{}
			return SandboxInstance{Sandbox: s, Resources: &stubResources{}, Close: func() error { return nil }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
			return &stubBridge{}, nil
		},
	}
	cfg := config.Config{Name: "x"}
	model := core.ModelConfig{Provider: "deepseek", BaseURL: "https://example.invalid", Model: "m", APIKeyEnv: "KEY"}
	preparedGPUIndices := []int{4, 2}
	catalog := testPreparedBenchmark(t, wiring)
	first, err := buildTaskExperiment(cfg, model, preparedGPUIndices, "run", t.TempDir(), "a", "a-001", nil, nil, catalog, wiring)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildTaskExperiment(cfg, model, preparedGPUIndices, "run", t.TempDir(), "a", "a-002", nil, nil, catalog, wiring)
	if err != nil {
		t.Fatal(err)
	}
	if first.runner == second.runner || first.recorder == second.recorder {
		t.Fatal("occurrences shared orchestration objects")
	}
	if !reflect.DeepEqual(preparedGPUIndices, []int{4, 2}) {
		t.Fatalf("prepared GPU indices mutated: %v", preparedGPUIndices)
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
		LoadBenchmark: testBenchmarkLoader(nil, func(config.Config, string, string, string, func(string) ([]byte, bool)) (runner.Benchmark, error) {
			return &stubBenchmark{}, nil
		}),
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{Harness: &stubHarness{}, Close: func() error { events = append(events, "harness"); return errors.New("harness close") }}, nil
		},
		NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{Sandbox: &stubToolSandbox{}, Resources: &stubResources{}, Close: func() error { events = append(events, "sandbox"); return errors.New("sandbox close") }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
			return nil, errors.New("bridge construct")
		},
	}
	_, err := buildTaskExperiment(config.Config{Name: "x"}, core.ModelConfig{}, nil, "run", t.TempDir(), "a", "a-001", nil, nil, testPreparedBenchmark(t, wiring), wiring)
	if err == nil || !strings.Contains(err.Error(), "bridge construct") || !strings.Contains(err.Error(), "sandbox close") || !strings.Contains(err.Error(), "harness close") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(events, []string{"sandbox", "harness"}) {
		t.Fatalf("events=%v", events)
	}
}

func TestBridgePreparationFailurePrecedesBenchmarkEffects(t *testing.T) {
	sentinel := errors.New("bridge binary unavailable")
	touched := false
	wiring := Wiring{
		PrepareBridge:  func(context.Context, config.Config) error { return sentinel },
		SetupBenchmark: func(context.Context, config.Config) error { touched = true; return nil },
		LoadBenchmark: testBenchmarkLoader(func(context.Context, config.Config, []string, func(string) ([]byte, bool)) ([]core.Task, error) {
			return nil, nil
		}, nil),
		PullImages:        func(context.Context, config.Config, []string) error { return nil },
		BuildHarnessImage: func(context.Context, config.Config) error { return nil },
	}
	if _, err := ensurePrepared(context.Background(), config.Config{}, t.TempDir(), wiring, nil); !errors.Is(err, sentinel) || touched {
		t.Fatalf("preparation order/error lost: %v touched=%v", err, touched)
	}
}

type occurrenceRecordingHarness struct {
	stubHarness
	request core.HarnessRequest
	delay   time.Duration
}

func (h *occurrenceRecordingHarness) Start(_ context.Context, request core.HarnessRequest) error {
	h.request = request
	return nil
}

func (h *occurrenceRecordingHarness) Run(context.Context, string) (core.HarnessResult, error) {
	time.Sleep(h.delay)
	return core.HarnessResult{}, nil
}

func TestRecordingPreservesSelectedOccurrences(t *testing.T) {
	const firstID = "first"
	const secondID = "second"
	const firstImage = "first-image:tag"
	const secondImage = "second-image:tag"
	const loopDuration = time.Second
	const singleCompletion = 1
	const firstLoopDivisor = 2
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	catalogTasks := []core.Task{
		{ID: firstID, Environment: core.Environment{Image: firstImage}},
		{ID: secondID, Environment: core.Environment{Image: secondImage}},
	}
	for _, test := range []struct {
		name      string
		selectors []string
		want      []string
		loop      time.Duration
		recording bool
	}{
		{name: "distinct", selectors: []string{firstID, secondID}, want: []string{"first-001", "second-002"}, recording: true},
		{name: "repeated", selectors: []string{secondID, firstID, secondID}, want: []string{"second-001", "first-002", "second-003"}, recording: true},
		{name: "expanding selector", selectors: []string{"group"}, want: []string{"first-001", "second-002"}, recording: true},
		{name: "finite loop", selectors: []string{firstID}, want: []string{"first-001", "first-002"}, loop: loopDuration, recording: true},
		{name: "ordinary repeated", selectors: []string{secondID, firstID, secondID}, want: []string{"second-001", "first-002", "second-003"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				path := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				selectors, err := json.Marshal(test.selectors)
				if err != nil {
					t.Fatal(err)
				}
				profile := strings.ReplaceAll(string(data), `"tasks":["a"]`, `"tasks":`+string(selectors))
				profile = strings.ReplaceAll(profile, `"harness":{"type":"openclaw"}`, `"harness":{"type":"hermes"}`)
				profile = strings.ReplaceAll(profile, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"hermes-grpc"}`)
				if test.loop != 0 {
					profile = strings.ReplaceAll(profile, `"output_dir":`, `"execution":{"loop_duration":"`+test.loop.String()+`"},"output_dir":`)
				}
				if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
					t.Fatal(err)
				}
				tasks := catalogTasks
				if test.loop != 0 {
					tasks = catalogTasks[:singleCompletion]
				}
				var bridges []*sourceStubBridge
				var harnesses []*occurrenceRecordingHarness
				var benchmarks []*replayTaskBenchmark
				var sandboxes []*cancelSandbox
				wiring := Wiring{
					ValidateComponents: func(config.Config) error { return nil },
					PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
						return PreparedBackend{Model: cfg.CoreModel()}, nil
					},
					SetupBenchmark:     func(context.Context, config.Config) error { return nil },
					BenchmarkRevisions: func(config.Config) map[string]string { return map[string]string{"revision": "pin"} },
					LoadBenchmark: func(_ context.Context, _ config.Config, _ string, _ []string, _ func(string) ([]byte, bool)) (PreparedBenchmark, error) {
						return PreparedBenchmark{Tasks: tasks, NewOccurrence: func(logicalID, occurrenceID string) (runner.Benchmark, error) {
							for _, task := range tasks {
								if task.ID == logicalID {
									task.ID = occurrenceID
									benchmark := &replayTaskBenchmark{task: task}
									benchmarks = append(benchmarks, benchmark)
									return benchmark, nil
								}
							}
							return nil, errors.New("logical task absent")
						}}, nil
					},
					PullImages:        func(context.Context, config.Config, []string) error { return nil },
					BuildHarnessImage: func(context.Context, config.Config) error { return nil },
					NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
						harness := &occurrenceRecordingHarness{}
						if test.loop != 0 {
							harness.delay = test.loop
							if len(harnesses) == 0 {
								harness.delay /= firstLoopDivisor
							}
						}
						harnesses = append(harnesses, harness)
						return HarnessInstance{Harness: harness}, nil
					},
					NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
						sandbox := &cancelSandbox{events: new([]string)}
						sandboxes = append(sandboxes, sandbox)
						return SandboxInstance{Sandbox: sandbox, Resources: &stubResources{}}, nil
					},
					NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
						bridge := &sourceStubBridge{}
						bridges = append(bridges, bridge)
						return bridge, nil
					},
				}
				var output bytes.Buffer
				doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
				if err := runCommandForTest(context.Background(), path, &output, Dependencies{RecordForReplay: test.recording, Wiring: wiring, PreflightClient: doer}); err != nil {
					t.Fatal(err)
				}
				var result core.RunResult
				if err := json.Unmarshal(output.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				var got []string
				for index, task := range result.Tasks {
					got = append(got, task.TaskID)
					harness, bridge := harnesses[index], bridges[index]
					if harness.request.RunID != result.RunID || harness.request.TaskID != task.TaskID || task.Evaluation.Status != core.StatusSucceeded || task.Isolation.Status != core.StatusConfirmed || task.Cleanup.Status != core.StatusSucceeded {
						t.Fatalf("occurrence binding or lifecycle = %+v, request = %+v", task, harness.request)
					}
					if test.recording {
						logicalID := task.TaskID[:strings.LastIndex(task.TaskID, "-")]
						wantImage := firstImage
						if logicalID == secondID {
							wantImage = secondImage
						}
						if bridge.logicalID != logicalID || bridge.image != wantImage || bridge.taskImage != wantImage || bridge.completions != singleCompletion {
							t.Fatalf("source binding for %s = %+v", task.TaskID, bridge)
						}
					} else if bridge.logicalID != "" || bridge.image != "" || bridge.taskImage != "" || bridge.completions != 0 {
						t.Fatalf("ordinary occurrence used recording: %+v", bridge)
					}
					for previous := range index {
						if benchmarks[previous] == benchmarks[index] || harnesses[previous] == harness || sandboxes[previous] == sandboxes[index] || bridges[previous] == bridge {
							t.Fatal("occurrences share mutable owners")
						}
					}
				}
				if !reflect.DeepEqual(got, test.want) {
					t.Fatalf("occurrences = %v, want %v", got, test.want)
				}
				if test.loop != 0 && result.Duration <= test.loop {
					t.Fatalf("admitted work did not drain after loop admission: %s", result.Duration)
				}
			})
		})
	}
}

func TestRecordingRejectsUnsupportedRunBeforePreparation(t *testing.T) {
	for _, test := range []struct {
		name, before, after, want string
		replay, setup             bool
	}{
		{name: "ssh", before: `"bridge":{"type":"openclaw-ssh"}`, after: `"bridge":{"type":"hermes-ssh"}`, want: "hermes-grpc"},
		{name: "arrivals", want: "arrival"},
		{name: "replay", replay: true, want: "replay profile"},
		{name: "setup", setup: true, want: "setup"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
			if test.replay {
				path = writeReplayCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			profile := strings.Replace(string(data), `"harness":{"type":"openclaw"}`, `"harness":{"type":"hermes"}`, 1)
			if test.before != "" {
				profile = strings.Replace(profile, test.before, test.after, 1)
			}
			if test.name != "ssh" {
				profile = strings.Replace(profile, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"hermes-grpc"}`, 1)
			}
			if test.name == "arrivals" {
				profile = strings.Replace(profile, `"output_dir":`, `"execution":{"arrivals_file":"schedule.csv","arrival_rate_per_min":1},"output_dir":`, 1)
			}
			if err := os.WriteFile(path, []byte(profile), 0600); err != nil {
				t.Fatal(err)
			}
			deps := Dependencies{RecordForReplay: true, Wiring: Wiring{ValidateComponents: func(config.Config) error { return nil }, PrepareBackend: func(config.Config, string) (PreparedBackend, error) {
				t.Fatal("recording rejection reached backend")
				return PreparedBackend{}, nil
			}}}
			command := Run
			if test.setup {
				command = Setup
			}
			err = command(context.Background(), path, io.Discard, deps)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestRecordingResolvesCatalogAndRequiredProvenanceBeforeExecution(t *testing.T) {
	const resolvedID = "resolved-task"
	const image = "task:tag"
	task := core.Task{ID: resolvedID, Environment: core.Environment{Image: image}}
	for _, test := range []struct {
		name      string
		tasks     []core.Task
		revisions map[string]string
		want      string
		recording bool
	}{
		{name: "one resolved task", tasks: []core.Task{task}, revisions: map[string]string{"revision": "pin"}, recording: true},
		{name: "selector expands", tasks: []core.Task{task, {ID: "other", Environment: core.Environment{Image: image}}}, revisions: map[string]string{"revision": "pin"}, recording: true},
		{name: "empty selection", revisions: map[string]string{"revision": "pin"}, want: "at least one", recording: true},
		{name: "no provenance", tasks: []core.Task{task}, want: "provenance", recording: true},
		{name: "empty revision", tasks: []core.Task{task}, revisions: map[string]string{"revision": ""}, want: "provenance", recording: true},
		{name: "no image", tasks: []core.Task{{ID: resolvedID}}, revisions: map[string]string{"revision": "pin"}, want: "provenance", recording: true},
		{name: "later task has no image", tasks: []core.Task{task, {ID: "other"}}, revisions: map[string]string{"revision": "pin"}, want: "provenance", recording: true},
		{name: "later task has no ID", tasks: []core.Task{task, {Environment: core.Environment{Image: image}}}, revisions: map[string]string{"revision": "pin"}, want: "provenance", recording: true},
		{name: "ordinary run unrestricted", tasks: []core.Task{task, {ID: "other"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(deepSeekAPIKey, "synthetic-key")
			path := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			profile := strings.Replace(string(data), `"harness":{"type":"openclaw"}`, `"harness":{"type":"hermes"}`, 1)
			profile = strings.Replace(profile, `"bridge":{"type":"openclaw-ssh"}`, `"bridge":{"type":"hermes-grpc"}`, 1)
			if err := os.WriteFile(path, []byte(profile), 0600); err != nil {
				t.Fatal(err)
			}
			stopped := errors.New("occurrence reached")
			var selected []string
			wiring := Wiring{
				ValidateComponents: func(config.Config) error { return nil },
				PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
					return PreparedBackend{Model: cfg.CoreModel()}, nil
				},
				SetupBenchmark:     func(context.Context, config.Config) error { return nil },
				BenchmarkRevisions: func(config.Config) map[string]string { return test.revisions },
				LoadBenchmark: func(_ context.Context, _ config.Config, _ string, selectors []string, _ func(string) ([]byte, bool)) (PreparedBenchmark, error) {
					if !reflect.DeepEqual(selectors, []string{"a"}) {
						t.Fatalf("selectors=%v", selectors)
					}
					return PreparedBenchmark{Tasks: test.tasks, NewOccurrence: func(logicalID, _ string) (runner.Benchmark, error) {
						selected = append(selected, logicalID)
						return nil, stopped
					}}, nil
				},
				PullImages: func(context.Context, config.Config, []string) error {
					if test.want != "" {
						t.Fatal("invalid recording pulled images")
					}
					return nil
				},
				BuildHarnessImage: func(context.Context, config.Config) error { return nil },
				NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
					return HarnessInstance{}, nil
				},
				NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
					return SandboxInstance{}, nil
				},
				NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return nil, nil },
			}
			doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
			err = runCommandForTest(context.Background(), path, io.Discard, Dependencies{RecordForReplay: test.recording, Wiring: wiring, PreflightClient: doer})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("error=%v, want %q", err, test.want)
				}
				return
			}
			wantIDs := []string{"a"}
			if test.recording {
				wantIDs = nil
				for _, task := range test.tasks {
					wantIDs = append(wantIDs, task.ID)
				}
			}
			if !errors.Is(err, stopped) || !reflect.DeepEqual(selected, wantIDs) {
				t.Fatalf("error=%v, selected=%q, want=%q", err, selected, wantIDs)
			}
		})
	}
}
