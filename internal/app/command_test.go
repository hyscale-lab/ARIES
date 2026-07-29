package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

type recordingRuntime struct {
	mu             sync.Mutex
	events         *[]string
	health         []error
	done           chan struct{}
	err            error
	startErr       error
	stopErr        error
	stopContextErr error
}

func (r *recordingRuntime) record(event string) {
	r.mu.Lock()
	*r.events = append(*r.events, event)
	r.mu.Unlock()
}
func (r *recordingRuntime) Start(context.Context) error { r.record("start"); return r.startErr }
func (r *recordingRuntime) Health(context.Context) error {
	r.record("health")
	if len(r.health) == 0 {
		return nil
	}
	err := r.health[0]
	r.health = r.health[1:]
	return err
}
func (r *recordingRuntime) Done() <-chan struct{} { return r.done }
func (r *recordingRuntime) Err() error            { return r.err }
func (r *recordingRuntime) Stop(ctx context.Context) error {
	r.record("stop")
	r.stopContextErr = ctx.Err()
	return r.stopErr
}

func TestInjectedModelRuntimeWrapsPreflightAndRunFailure(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	events := []string{}
	runtime := &recordingRuntime{events: &events, done: make(chan struct{})}
	wiring := failingRunWiring(runtime, &events, errors.New("run canary"))
	doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
	err := Run(context.Background(), profile, io.Discard, Dependencies{PreflightClient: doer, PreflightSleep: func(context.Context, time.Duration) error { return nil }, Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "run canary") {
		t.Fatalf("err=%v", err)
	}
	if !reflect.DeepEqual(events, []string{"start", "health", "run", "stop"}) {
		t.Fatalf("events=%v", events)
	}
	if runtime.stopContextErr != nil {
		t.Fatalf("cleanup context=%v", runtime.stopContextErr)
	}
}

func TestInjectedModelRuntimeStopFailureIsReturned(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	events := []string{}
	runtime := &recordingRuntime{events: &events, done: make(chan struct{}), stopErr: errors.New("stop canary")}
	wiring := failingRunWiring(runtime, &events, errors.New("run canary"))
	doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
	err := Run(context.Background(), profile, io.Discard, Dependencies{PreflightClient: doer, Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "run canary") || !strings.Contains(err.Error(), "stop canary") {
		t.Fatalf("joined err=%v", err)
	}
}

func TestManagedRuntimeLifecycleOrder(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	events := []string{}
	runtime := &recordingRuntime{events: &events, done: make(chan struct{}), health: []error{classifiedHealth{retry: true}, nil}}
	wiring := failingRunWiring(runtime, &events, errors.New("run complete canary"))
	doer := httpDoerFunc(func(req *http.Request) (*http.Response, error) {
		events = append(events, "preflight")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"deepseek-v4-flash"}]}`))}, nil
	})
	err := Run(context.Background(), profile, io.Discard, Dependencies{PreflightClient: doer, PreflightSleep: func(context.Context, time.Duration) error { return nil }, Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "run complete canary") {
		t.Fatalf("err=%v", err)
	}
	want := []string{"start", "health", "health", "preflight", "run", "stop"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v want=%v", events, want)
	}
}

func TestRuntimeLifecycleLogsAreStructuredAndSanitized(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	for _, tc := range []struct {
		name       string
		unexpected bool
		stopErr    error
		wantStates []string
	}{
		{name: "unexpected exit then stopped", unexpected: true, stopErr: cleanupClassifiedError{err: errors.New("stop unexpected body canary")}, wantStates: []string{"started", "healthy", "unexpected_exit", "stopped"}},
		{name: "stop failure", stopErr: errors.New("stop secret body canary"), wantStates: []string{"started", "healthy", "stop_failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
			events := []string{}
			runtime := &recordingRuntime{events: &events, done: make(chan struct{}), err: errors.New("runtime secret body canary"), stopErr: tc.stopErr}
			wiring := failingRunWiring(runtime, &events, errors.New("run body canary"))
			if tc.unexpected {
				wiring.NewBenchmark = func(config.Config, string, string, string) (runner.Benchmark, error) {
					close(runtime.done)
					return nil, errors.New("run body canary")
				}
			}
			var logs bytes.Buffer
			logger := logrus.New()
			logger.SetOutput(&logs)
			logger.SetFormatter(&logrus.JSONFormatter{})
			doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
			err := Run(context.Background(), profile, io.Discard, Dependencies{Logger: logger, PreflightClient: doer, Wiring: wiring})
			if err == nil {
				t.Fatal("expected run error")
			}
			content := logs.String()
			for _, state := range tc.wantStates {
				if !strings.Contains(content, `"runtime_state":"`+state+`"`) {
					t.Fatalf("missing state %q in %s", state, content)
				}
			}
			for _, field := range []string{`"run_id":`, `"profile":"test"`, `"backend":"deepseek"`, `"mode":"external"`} {
				if !strings.Contains(content, field) {
					t.Fatalf("missing field %s in %s", field, content)
				}
			}
			for _, secret := range []string{"runtime secret body canary", "stop secret body canary", "stop unexpected body canary", "run body canary", "synthetic-key", "Authorization"} {
				if strings.Contains(content, secret) {
					t.Fatalf("log leaked %q: %s", secret, content)
				}
			}
		})
	}
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func failingRunWiring(runtime ModelRuntime, events *[]string, runErr error) Wiring {
	return Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
			return PreparedBackend{Model: cfg.CoreModel(), Runtime: runtime}, nil
		},
		NewBenchmark: func(config.Config, string, string, string) (runner.Benchmark, error) {
			*events = append(*events, "run")
			return nil, runErr
		},
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{}, nil
		},
		NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return nil, nil },
	}
}

func TestBackendPreparationPrecedesAllEffects(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, "runs")
	profile := writeCommandProfile(t, runs)
	called := false
	wiring := Wiring{ValidateComponents: func(config.Config) error { return nil }, PrepareBackend: func(config.Config, string) (PreparedBackend, error) {
		return PreparedBackend{}, errors.New("prepare canary")
	}, NewBenchmark: func(config.Config, string, string, string) (runner.Benchmark, error) { called = true; return nil, nil }}
	err := Run(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "prepare canary") || called {
		t.Fatalf("err=%v called=%t", err, called)
	}
	if _, err := os.Stat(runs); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output created: %v", err)
	}
}

func TestRunForwardsFreshPreparedGPUIndicesToEveryOccurrence(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "synthetic-key")
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	content, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	content = bytes.Replace(content, []byte(`"tasks":["a"]`), []byte(`"tasks":["a","a"]`), 1)
	if err := os.WriteFile(profile, content, 0o600); err != nil {
		t.Fatal(err)
	}
	preparedGPUIndices := []int{4, 2}
	sandboxCalls := 0
	wiring := Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
			return PreparedBackend{Model: cfg.CoreModel(), EffectiveGPUIndices: preparedGPUIndices}, nil
		},
		NewBenchmark: func(config.Config, string, string, string) (runner.Benchmark, error) {
			return &oneTaskBenchmark{}, nil
		},
		NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
			return HarnessInstance{Harness: &stubHarness{}, Close: func() error { return nil }}, nil
		},
		NewSandbox: func(_ config.Config, _, _, _ string, gpuIndices []int, _ *logrus.Logger) (SandboxInstance, error) {
			sandboxCalls++
			if !reflect.DeepEqual(gpuIndices, []int{4, 2}) {
				t.Fatalf("sandbox call %d GPU indices = %v", sandboxCalls, gpuIndices)
			}
			gpuIndices[0] = 99
			return SandboxInstance{Sandbox: &managedIntegrationSandbox{}, Resources: &stubResources{}, Close: func() error { return nil }}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
			return &stubBridge{}, nil
		},
	}
	doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	if err := Run(context.Background(), profile, io.Discard, Dependencies{Logger: logger, PreflightClient: doer, Wiring: wiring}); err == nil || !strings.Contains(err.Error(), "observer report missing") {
		t.Fatalf("run error = %v", err)
	}
	if sandboxCalls != 2 || !reflect.DeepEqual(preparedGPUIndices, []int{4, 2}) {
		t.Fatalf("sandbox calls = %d, prepared GPU indices = %v", sandboxCalls, preparedGPUIndices)
	}
}

func TestUnsupportedComponentsAreRejectedImmediatelyOnRunAndSetup(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{name: "benchmark", old: `"type":"terminalbench2"`, new: `"type":"unsupported-benchmark"`},
		{name: "harness", old: `"type":"openclaw"`, new: `"type":"unsupported-harness"`},
		{name: "sandbox", old: `"type":"docker"`, new: `"type":"unsupported-sandbox"`},
		{name: "bridge", old: `"type":"openclaw-ssh"`, new: `"type":"unsupported-bridge"`},
	} {
		for _, useCase := range []struct {
			name string
			call func(string, io.Writer, Dependencies) error
		}{
			{name: "run", call: func(profile string, stdout io.Writer, dependencies Dependencies) error {
				return Run(context.Background(), profile, stdout, dependencies)
			}},
			{name: "setup", call: func(profile string, stdout io.Writer, dependencies Dependencies) error {
				return Setup(context.Background(), profile, stdout, dependencies)
			}},
		} {
			t.Run(useCase.name+"/"+tc.name, func(t *testing.T) {
				output := filepath.Join(t.TempDir(), "runs")
				profile := writeCommandProfile(t, output)
				content, err := os.ReadFile(profile)
				if err != nil {
					t.Fatal(err)
				}
				content = bytes.Replace(content, []byte(tc.old), []byte(tc.new), 1)
				if err := os.WriteFile(profile, content, 0600); err != nil {
					t.Fatal(err)
				}
				effects := 0
				wiring := Wiring{
					ValidateComponents: func(cfg config.Config) error {
						if cfg.Benchmark.Type != "terminalbench2" || cfg.Harness.Type != "openclaw" || cfg.Sandbox.Type != "docker" || cfg.Bridge.Type != "openclaw-ssh" {
							return errors.New("unsupported component canary")
						}
						return nil
					},
					PrepareBackend:       func(config.Config, string) (PreparedBackend, error) { effects++; return PreparedBackend{}, nil },
					SetupBenchmark:       func(context.Context, config.Config) error { effects++; return nil },
					LoadPreparationTasks: func(context.Context, config.Config, []string) ([]core.Task, error) { effects++; return nil, nil },
					PullImages:           func(context.Context, []string) error { effects++; return nil },
					NewBenchmark:         func(config.Config, string, string, string) (runner.Benchmark, error) { effects++; return nil, nil },
					NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
						effects++
						return HarnessInstance{}, nil
					},
					NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
						effects++
						return SandboxInstance{}, nil
					},
					NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { effects++; return nil, nil },
				}
				err = useCase.call(profile, io.Discard, Dependencies{Wiring: wiring})
				if err == nil || !strings.Contains(err.Error(), "unsupported component canary") || effects != 0 {
					t.Fatalf("err=%v effects=%d", err, effects)
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("output side effect: %v", err)
				}
			})
		}
	}
}

func TestExecuteAndRecordPreservesRunAndPersistenceErrorsAndWritesResult(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, runResultName)
	if err := os.WriteFile(path, []byte("stale\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runErr := errors.New("runner failed")
	var stdout bytes.Buffer
	err := executeAndRecord(context.Background(), func(context.Context) (core.RunResult, error) { return core.RunResult{Name: "x", RunID: "r"}, runErr }, root, &stdout)
	if !errors.Is(err, runErr) || !errors.Is(err, os.ErrExist) {
		t.Fatalf("err=%v", err)
	}
	var got core.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil || got.RunID != "r" {
		t.Fatalf("stdout=%q err=%v", stdout.Bytes(), err)
	}
	writeErr := errors.New("stdout failed")
	err = executeAndRecord(context.Background(), func(context.Context) (core.RunResult, error) { return core.RunResult{Name: "x", RunID: "r"}, runErr }, root, failingWriter{err: writeErr})
	if !errors.Is(err, runErr) || !errors.Is(err, os.ErrExist) || !errors.Is(err, writeErr) {
		t.Fatalf("fully joined err=%v", err)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunCredentialSourceFallbackAndClearing(t *testing.T) {
	t.Run("anchored", func(t *testing.T) {
		root, exe := createTestAriesRepository(t)
		key := "anchored-key"
		if err := os.WriteFile(filepath.Join(root, localAPIKeyFile), []byte(key), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(deepSeekAPIKey, "environment-key")
		assertCommandAuthorization(t, exe, key)
	})
	t.Run("environment", func(t *testing.T) {
		exe := filepath.Join(t.TempDir(), ariesExecutableName)
		if err := os.WriteFile(exe, []byte("x"), 0700); err != nil {
			t.Fatal(err)
		}
		key := "environment-key"
		t.Setenv(deepSeekAPIKey, key)
		assertCommandAuthorization(t, exe, key)
		if os.Getenv(deepSeekAPIKey) != key {
			t.Fatal("environment mutated")
		}
	})
}

func assertCommandAuthorization(t *testing.T, exe, want string) {
	t.Helper()
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	events := []string{}
	runtime := &recordingRuntime{events: &events, done: make(chan struct{})}
	w := failingRunWiring(runtime, &events, errors.New("done"))
	doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
	_ = Run(context.Background(), profile, io.Discard, Dependencies{ExecutablePath: exe, PreflightClient: doer, Wiring: w})
	if len(doer.authorizations) != 1 || doer.authorizations[0] != "Bearer "+want {
		t.Fatalf("authorization=%v", doer.authorizations)
	}
}

func writeCommandProfile(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	versions := config.Versions{TerminalBench2: config.TerminalBench2Versions{RepositoryURL: "https://example.invalid/repo.git", Revision: "0123456789abcdef0123456789abcdef01234567"}, OpenClaw: config.OpenClawVersions{Image: "ghcr.io/openclaw/openclaw:2026.7.1"}}
	data, _ := json.Marshal(versions)
	if err := os.WriteFile(filepath.Join(dir, "versions.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	profile := fmt.Sprintf(`{"name":"test","versions_file":"versions.json","benchmark":{"type":"terminalbench2","root":"root","tasks":["a"]},"harness":{"type":"openclaw"},"sandbox":{"type":"docker"},"bridge":{"type":"openclaw-ssh"},"runtime":{"backend":"deepseek","mode":"external"},"model":{"id":"deepseek-v4-flash","base_url":"https://api.deepseek.com","api_key_env":"DEEPSEEK_API_KEY"},"output_dir":%q}`, output)
	path := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(path, []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func createTestAriesRepository(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, ariesExecutableName)
	if err := os.WriteFile(exe, []byte("x"), 0700); err != nil {
		t.Fatal(err)
	}
	return root, exe
}

// RuntimeExitCancelsAndDrainsRun proves one admitted occurrence drains cleanup
// and cancellation prevents the next queued occurrence from being constructed.
func TestRuntimeExitCancelsAndDrainsRun(t *testing.T) {
	t.Setenv(deepSeekAPIKey, "key")
	profile := writeCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	content, _ := os.ReadFile(profile)
	content = bytes.Replace(content, []byte(`"tasks":["a"]`), []byte(`"tasks":["a","b"]`), 1)
	_ = os.WriteFile(profile, content, 0600)
	events := []string{}
	runtime := &recordingRuntime{events: &events, done: make(chan struct{}), err: errors.New("runtime exit")}
	started := make(chan struct{})
	h := &cancelHarness{started: started, events: &events}
	constructed := 0
	wiring := Wiring{ValidateComponents: func(config.Config) error { return nil }, PrepareBackend: func(cfg config.Config, _ string) (PreparedBackend, error) {
		return PreparedBackend{Model: cfg.CoreModel(), Runtime: runtime}, nil
	}, NewBenchmark: func(config.Config, string, string, string) (runner.Benchmark, error) {
		constructed++
		return &oneTaskBenchmark{}, nil
	}, NewHarness: func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error) {
		return HarnessInstance{Harness: h, Close: func() error { return nil }}, nil
	}, NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
		return SandboxInstance{Sandbox: &cancelSandbox{events: &events}, Resources: &stubResources{}, Close: func() error { return nil }}, nil
	}, NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
		return &cancelBridge{events: &events}, nil
	}}
	doer := &preflightDoer{t: t, replies: []preflightReply{{status: 200, body: `{"data":[{"id":"deepseek-v4-flash"}]}`}}}
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), profile, io.Discard, Dependencies{PreflightClient: doer, Wiring: wiring})
	}()
	<-started
	close(runtime.done)
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "runtime exit") {
		t.Fatalf("err=%v", err)
	}
	if constructed != 1 {
		t.Fatalf("constructed=%d", constructed)
	}
	for _, want := range []string{"harness-stop", "bridge-stop", "sandbox-stop", "stop"} {
		if !contains(events, want) {
			t.Fatalf("events=%v missing=%s", events, want)
		}
	}
}
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

type oneTaskBenchmark struct{}

func (*oneTaskBenchmark) Tasks(context.Context) ([]core.Task, error) {
	return []core.Task{{ID: "task", Instruction: "do", Environment: core.Environment{Image: "img", Workdir: "/"}}}, nil
}
func (*oneTaskBenchmark) PrepareSandbox(context.Context, core.Task, runner.Sandbox) error { return nil }
func (*oneTaskBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox) (core.Evaluation, error) {
	return core.Evaluation{Status: core.StatusSucceeded}, nil
}

type cancelHarness struct {
	started chan struct{}
	once    sync.Once
	events  *[]string
}

func (*cancelHarness) Start(context.Context, core.HarnessRequest) error { return nil }
func (h *cancelHarness) Run(ctx context.Context, _ string) (core.HarnessResult, error) {
	h.once.Do(func() { close(h.started) })
	<-ctx.Done()
	return core.HarnessResult{}, ctx.Err()
}
func (h *cancelHarness) Stop(context.Context) error {
	*h.events = append(*h.events, "harness-stop")
	return nil
}

type cancelSandbox struct{ events *[]string }

func (*cancelSandbox) Start(context.Context, core.SandboxRequest) (runner.Sandbox, error) {
	return &stubSandbox{}, nil
}
func (s *cancelSandbox) Stop(context.Context, runner.Sandbox) error {
	*s.events = append(*s.events, "sandbox-stop")
	return nil
}

type stubSandbox struct{}

func (*stubSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}
func (*stubSandbox) Upload(context.Context, string, string) error   { return nil }
func (*stubSandbox) Download(context.Context, string, string) error { return nil }

type cancelBridge struct{ events *[]string }

func (*cancelBridge) Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error) {
	return core.ToolEndpoint{}, nil
}
func (b *cancelBridge) Stop(context.Context) error {
	*b.events = append(*b.events, "bridge-stop")
	return nil
}
