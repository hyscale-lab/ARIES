package app

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	bridgegrpc "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/managed"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
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
		PreflightReplaySource: func(context.Context, config.Config, []core.Task, *logrus.Entry) (*bridgegrpc.ReplaySelection, error) {
			events = append(events, "preflight")
			return &bridgegrpc.ReplaySelection{}, nil
		},
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
	want := []string{"bridge", "benchmark", "catalog", "images:task:tag", "preflight", "infrastructure"}
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

const (
	finiteSourceRun = "recorded-run"
	finiteImage     = "task:tag"
	finiteRevision  = "source-revision"
	finiteStart     = "2026-11-05T12:00:00Z"
	finiteCallGap   = time.Second
)

func writeFiniteReplaySource(t *testing.T, logicalIDs []string, zeroCalls bool, offsets ...time.Duration) (string, []core.Task, []core.TaskResult) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	start, err := time.Parse(time.RFC3339Nano, finiteStart)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []core.Task
	var results []core.TaskResult
	seen := map[string]bool{}
	writeJSON := func(path string, value any) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for index, logicalID := range logicalIDs {
		if !seen[logicalID] {
			tasks = append(tasks, core.Task{ID: logicalID, Environment: core.Environment{Image: finiteImage}})
			seen[logicalID] = true
		}
		const firstOccurrence = 1
		id := fmt.Sprintf("%s-%03d", logicalID, index+firstOccurrence)
		dir := filepath.Join(root, id, "bridge")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		var log []byte
		at := start.Add(time.Duration(index) * finiteCallGap)
		if !zeroCalls {
			if len(offsets) == 0 {
				offsets = []time.Duration{0}
			}
			for callIndex, offset := range offsets {
				const firstSequence = 1
				// Canonical empty Stat success: version, method, empty message, OK, empty error.
				const response = "\x15aries.grpc.outcome.v1\x1e/aries.sandbox.v1.Sandbox/Stat\x00\x02OK\x00"
				digest := sha256.Sum256([]byte(response))
				called := at.Add(offset)
				record := map[string]any{
					"run_id": finiteSourceRun, "task_id": id, "sandbox_id": id,
					"sequence": callIndex + firstSequence, "started_at": called.Format(time.RFC3339Nano), "timestamp": called.Format(time.RFC3339Nano),
					"operation_class": "stat", "path": "/app/file", "status": "completed",
					"response_sha256": hex.EncodeToString(digest[:]), "response_bytes": len(response), "grpc_code": "OK",
				}
				line, err := json.Marshal(record)
				if err != nil {
					t.Fatal(err)
				}
				log = append(log, line...)
				log = append(log, '\n')
			}
			at = at.Add(offsets[len(offsets)-1])
		}
		if err := os.WriteFile(filepath.Join(dir, "tool-calls.jsonl"), log, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(log)
		const sourceSchemaVersion = 1
		writeJSON(filepath.Join(dir, "runtime.json"), map[string]any{
			"schema_version": sourceSchemaVersion, "run_id": finiteSourceRun, "replay_ready": true, "SandboxID": id,
			"benchmark_type": "terminalbench2", "task_id": logicalID, "image": finiteImage,
			"benchmark_revisions": map[string]string{"revision": finiteRevision}, "harness_completed_at": at.Format(time.RFC3339Nano),
			"tool_calls_bytes": len(log), "tool_calls_sha256": hex.EncodeToString(digest[:]),
		})
		results = append(results, core.TaskResult{TaskID: id, Evaluation: core.Evaluation{Status: core.StatusSucceeded, Score: float64(index), Reward: float64(index), VerifierStatus: id}})
	}
	writeJSON(filepath.Join(root, "run-result.json"), core.RunResult{RunID: finiteSourceRun, Tasks: results})
	return root, tasks, results
}

type finiteReplayCaller struct {
	stubReplayCaller
	start func(context.Context) error
	run   func(context.Context, replay.Plan) error
	stop  func(context.Context) error
}

func (caller *finiteReplayCaller) Start(ctx context.Context, _ core.ToolEndpoint, _ core.HarnessConnectivity, _, _ string) error {
	if caller.start != nil {
		return caller.start(ctx)
	}
	return nil
}
func (caller *finiteReplayCaller) Run(ctx context.Context, plan replay.Plan) error {
	return caller.run(ctx, plan)
}
func (caller *finiteReplayCaller) Stop(ctx context.Context) error {
	if caller.stop != nil {
		return caller.stop(ctx)
	}
	return caller.stubReplayCaller.Stop(ctx)
}

type finiteReplaySandbox struct {
	managedIntegrationSandbox
	start func(context.Context) error
	stop  func(context.Context) error
}

func (sandbox *finiteReplaySandbox) Start(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	if sandbox.start != nil {
		if err := sandbox.start(ctx); err != nil {
			return nil, err
		}
	}
	return sandbox.managedIntegrationSandbox.Start(ctx, request)
}
func (sandbox *finiteReplaySandbox) Stop(ctx context.Context, _ runner.Sandbox) error {
	if sandbox.stop != nil {
		return sandbox.stop(ctx)
	}
	return nil
}

type finiteReplayBenchmark struct{ replayTaskBenchmark }

func (*finiteReplayBenchmark) Evaluate(context.Context, core.Task, runner.Sandbox, runner.EvaluationSandboxes) (core.Evaluation, error) {
	return core.Evaluation{}, errors.New("replay must not run a verifier or judge")
}

func finiteReplayProfile(t *testing.T, source string, concurrency int) string {
	t.Helper()
	profile := writeReplayCommandProfile(t, filepath.Join(t.TempDir(), "runs"))
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"source-run/a-001"`), []byte(fmt.Sprintf("%q", source)), 1)
	data = bytes.Replace(data, []byte(`"benchmark":`), []byte(fmt.Sprintf(`"execution":{"concurrency":%d},"benchmark":`, concurrency)), 1)
	if err := os.WriteFile(profile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return profile
}

func finiteReplayWiring(tasks []core.Task) Wiring {
	return Wiring{
		ValidateComponents: func(config.Config) error { return nil },
		SetupBenchmark:     func(context.Context, config.Config) error { return nil },
		PullImages:         func(context.Context, config.Config, []string) error { return nil },
		LoadBenchmark: func(context.Context, config.Config, string, []string, func(string) ([]byte, bool)) (PreparedBenchmark, error) {
			return PreparedBenchmark{Tasks: tasks, NewOccurrence: func(logicalID, occurrenceID string) (runner.Benchmark, error) {
				return &finiteReplayBenchmark{replayTaskBenchmark{task: core.Task{ID: occurrenceID, Environment: core.Environment{Image: finiteImage}}}}, nil
			}}, nil
		},
		PreflightReplaySource: func(ctx context.Context, cfg config.Config, tasks []core.Task, logger *logrus.Entry) (*bridgegrpc.ReplaySelection, error) {
			return bridgegrpc.PreflightReplaySource(ctx, cfg.Replay.Source, tasks, cfg.Benchmark.Type, map[string]string{"revision": finiteRevision}, logger)
		},
		NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{Sandbox: &managedIntegrationSandbox{}, Resources: &stubResources{}}, nil
		},
		NewBridge: func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) { return &stubBridge{}, nil },
	}
}

func snapshotFiniteReplaySource(t *testing.T, source string) map[string][sha256.Size]byte {
	t.Helper()
	files := map[string][sha256.Size]byte{}
	if err := filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = sha256.Sum256(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

type finiteAuditBridge struct{ path string }

func (bridge *finiteAuditBridge) Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error) {
	return core.ToolEndpoint{LogPaths: []string{bridge.path}}, nil
}
func (*finiteAuditBridge) Stop(context.Context) error { return nil }

func TestReplayRunReportsSharedTimingAndEqualStartWarnings(t *testing.T) {
	const concurrency = 1
	const firstLateness = 3 * time.Millisecond
	const laterLateness = 40 * time.Millisecond
	source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "a"}, false, 0, finiteCallGap, finiteCallGap)
	wiring := finiteReplayWiring(tasks)
	var runRoot, auditPath string
	created := 0
	wiring.NewBridge = func(_ config.Config, outputRoot string, _ *logrus.Logger) (runner.ToolBridge, error) {
		runRoot = outputRoot
		auditPath = filepath.Join(outputRoot, fmt.Sprintf("audit-%d.jsonl", created))
		created++
		return &finiteAuditBridge{path: auditPath}, nil
	}
	wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
		path := auditPath
		return &finiteReplayCaller{run: func(_ context.Context, plan replay.Plan) error {
			if plan.ReplayAt == nil {
				return errors.New("whole workload has no shared replay origin")
			}
			lateness := firstLateness
			if plan.SourceTaskID == expected[len(expected)-1].TaskID {
				lateness = laterLateness
			}
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(file)
			for _, call := range plan.Calls {
				if err := encoder.Encode(map[string]any{
					"replay_source_sequence": call.Sequence,
					"started_at":             plan.ReplayAt.Add(call.StartOffset + lateness).Format(time.RFC3339Nano),
					"operation_class":        "stat", "response_sha256": call.ResponseSHA256,
				}); err != nil {
					return errors.Join(err, file.Close())
				}
			}
			return file.Close()
		}}, nil
	}
	var out bytes.Buffer
	if err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	var result core.RunResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(filepath.Join(runRoot, "run-result.json"))
	if err != nil || !bytes.Equal(persisted, out.Bytes()) {
		t.Fatalf("persisted result differs from stdout: %v", err)
	}
	if len(result.Tasks) != len(expected) {
		t.Fatalf("missing occurrence results: %+v", result)
	}
	for index, task := range result.Tasks {
		wantDrift := firstLateness
		if index == len(expected)-1 {
			wantDrift = laterLateness
		}
		if task.Replay.Status != core.StatusSucceeded || task.Replay.Timing.Status != "available" || task.Replay.Timing.Basis != "shared_replay_origin" || len(task.Replay.Timing.Calls) == 0 || task.Replay.Responses.Status != "available" || len(task.Replay.Responses.Calls) != len(task.Replay.Timing.Calls) || task.Evaluation.SourceTaskID != expected[index].TaskID {
			t.Fatalf("shared timing or source binding lost: %+v", task)
		}
		for _, call := range task.Replay.Timing.Calls {
			if call.DriftNS != wantDrift.Nanoseconds() {
				t.Fatalf("wrong occurrence displacement: %+v, want %v", task.Replay.Timing, wantDrift)
			}
		}
	}
	logs, err := os.ReadFile(filepath.Join(runRoot, "aries.log"))
	if err != nil || !bytes.Contains(logs, []byte("replay calls have equal start timestamps")) || !bytes.Contains(logs, []byte("tied_source_task_id")) || !bytes.Contains(logs, []byte("tied_source_sequence")) {
		t.Fatalf("equal starts missing from run log: %s, %v", logs, err)
	}
}

func TestReplayRunEmptyDiagnosticsDoNotArmSharedClock(t *testing.T) {
	const concurrency = 1
	source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "a"}, true)
	wiring := finiteReplayWiring(tasks)
	var auditPath string
	wiring.NewBridge = func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
		auditPath = filepath.Join(t.TempDir(), "tool-calls.jsonl")
		return &finiteAuditBridge{path: auditPath}, nil
	}
	wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
		path := auditPath
		return &finiteReplayCaller{run: func(_ context.Context, plan replay.Plan) error {
			if plan.ReplayAt != nil || len(plan.Calls) != 0 {
				return errors.New("empty source armed a clock or invented a call")
			}
			return os.WriteFile(path, nil, 0o600)
		}}, nil
	}
	var out bytes.Buffer
	if err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring}); err != nil {
		t.Fatal(err)
	}
	var result core.RunResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != len(expected) {
		t.Fatalf("missing empty occurrences: %+v", result)
	}
	for _, task := range result.Tasks {
		if task.Replay.Timing.Status != "available" || task.Replay.Timing.Basis != "occurrence_first_start" || len(task.Replay.Timing.Calls) != 0 {
			t.Fatalf("diagnostics armed an unused shared clock: %+v", task.Replay.Timing)
		}
	}
}

func TestReplayRunExecutesFiniteRecordedOccurrences(t *testing.T) {
	for _, test := range []struct {
		name                      string
		ids                       []string
		zeroCalls, single, legacy bool
	}{
		{name: "distinct", ids: []string{"a", "b"}},
		{name: "repeated", ids: []string{"a", "a"}},
		{name: "loop-produced", ids: []string{"a", "b", "a", "b"}},
		{name: "zero calls", ids: []string{"a", "a"}, zeroCalls: true},
		{name: "single occurrence", ids: []string{"a", "b"}, single: true},
		{name: "legacy occurrence", ids: []string{"a"}, single: true, legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, tasks, expected := writeFiniteReplaySource(t, test.ids, test.zeroCalls)
			if test.legacy {
				path := filepath.Join(source, expected[0].TaskID, "bridge", "runtime.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.Replace(data, []byte(`"run_id":"recorded-run",`), nil, 1)
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sourceRoot := source
			original := snapshotFiniteReplaySource(t, sourceRoot)
			if test.single {
				source = filepath.Join(source, expected[0].TaskID)
				expected = expected[:1]
			}
			const concurrency = 1
			profile := finiteReplayProfile(t, source, concurrency)
			var plans []replay.Plan
			wiring := finiteReplayWiring(tasks)
			wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
				return &finiteReplayCaller{run: func(_ context.Context, plan replay.Plan) error { plans = append(plans, plan); return nil }}, nil
			}
			var out bytes.Buffer
			if err := runCommandForTest(context.Background(), profile, &out, Dependencies{Wiring: wiring}); err != nil {
				t.Fatal(err)
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, snapshotFiniteReplaySource(t, sourceRoot)) {
				t.Fatal("replay changed source artifacts")
			}
			if result.RunID == finiteSourceRun {
				t.Fatal("replay reused source run identity")
			}
			if len(result.Tasks) != len(expected) || len(plans) != len(expected) {
				t.Fatalf("results=%+v plans=%+v, want %d occurrences", result, plans, len(expected))
			}
			for index, task := range result.Tasks {
				grade := expected[index].Evaluation
				grade.Reused = true
				grade.SourceRunID = finiteSourceRun
				grade.SourceTaskID = expected[index].TaskID
				if !reflect.DeepEqual(task.Evaluation, grade) {
					t.Fatalf("wrong source binding: %+v", task)
				}
				if len(expected) == 1 || test.zeroCalls {
					if plans[index].ReplayAt != nil {
						t.Fatalf("single occurrence lost legacy timing: %+v", plans[index])
					}
				} else if plans[index].ReplayAt == nil || !plans[index].ReplayAt.Equal(*plans[0].ReplayAt) {
					t.Fatalf("no shared replay origin: %+v", plans)
				}
				if test.zeroCalls && len(plans[index].Calls) != 0 {
					t.Fatalf("invented calls: %+v", plans[index])
				}
			}
		})
	}
}

func TestReplayRunUsesTerminalBenchOccurrenceIDs(t *testing.T) {
	for _, test := range []struct {
		ids    []string
		single bool
	}{
		{ids: []string{"a"}},
		{ids: []string{"a", "b", "a"}},
		{ids: []string{"a", "b", "a"}, single: true},
	} {
		name := strings.Join(test.ids, "+")
		if test.single {
			name += "/occurrence"
		}
		t.Run(name, func(t *testing.T) {
			source, tasks, expected := writeFiniteReplaySource(t, test.ids, false)
			ids := test.ids
			if test.single {
				expected = expected[len(expected)-1:]
				ids = ids[len(ids)-1:]
				source = filepath.Join(source, expected[0].TaskID)
			}
			root := t.TempDir()
			var logicalIDs []string
			for _, task := range tasks {
				logicalIDs = append(logicalIDs, task.ID)
				const positiveLimit = 1
				manifest := fmt.Sprintf("schema_version = \"1.1\"\n[task]\nname = %[1]q\n[agent]\ntimeout_sec = %[3]d\n[verifier]\ntimeout_sec = %[3]d\n[environment]\nbuild_timeout_sec = %[3]d\ndocker_image = %[2]q\ncpus = %[3]d\nmemory_mb = %[3]d\nstorage_mb = %[3]d\n", "terminal-bench/"+task.ID, finiteImage, positiveLimit)
				for name, data := range map[string]string{"task.toml": manifest, "instruction.md": "Complete the task.\n", "tests/test.sh": "#!/bin/sh\n"} {
					path := filepath.Join(root, task.ID, name)
					if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			git := func(args ...string) string {
				out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("fixture git %v: %v: %s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "--quiet")
			git("add", ".")
			git("-c", "user.name=ARIES Test", "-c", "user.email=aries@example.invalid", "-c", "commit.gpgsign=false", "commit", "--quiet", "-m", "fixture")
			catalog, err := terminalbench.New(terminalbench.Options{Root: root, TaskIDs: logicalIDs, OutputDir: t.TempDir(), Revision: git("rev-parse", "HEAD")})
			if err != nil {
				t.Fatal(err)
			}
			selected, err := catalog.Tasks(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			wiring := finiteReplayWiring(tasks)
			wiring.LoadBenchmark = func(context.Context, config.Config, string, []string, func(string) ([]byte, bool)) (PreparedBenchmark, error) {
				return PreparedBenchmark{Tasks: selected, NewOccurrence: catalog.ForTask}, nil
			}
			wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
				return &stubReplayCaller{}, nil
			}
			const concurrency = 1
			var out bytes.Buffer
			if err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring}); err != nil {
				t.Fatal(err)
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != len(expected) {
				t.Fatalf("missing real benchmark occurrences: %+v", result)
			}
			for index, task := range result.Tasks {
				const firstOccurrence = 1
				if want := fmt.Sprintf("%s-%03d", ids[index], index+firstOccurrence); task.TaskID != want || task.Evaluation.SourceTaskID != expected[index].TaskID {
					t.Fatalf("task and source identities changed: %+v, want execution %s", task, want)
				}
			}
		})
	}
}

type finiteStatClient struct {
	sandboxv1.SandboxClient
	started func()
}

func (client finiteStatClient) Stat(context.Context, *sandboxv1.StatRequest, ...grpc.CallOption) (*sandboxv1.StatResponse, error) {
	client.started()
	return &sandboxv1.StatResponse{}, nil
}

func TestReplayRunPreservesCallSpacingAfterInitialPreparation(t *testing.T) {
	for _, ids := range [][]string{{"a"}, {"a", "b"}} {
		t.Run(strings.Join(ids, "+"), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const preparationDelay = finiteCallGap + finiteCallGap
				const callsPerOccurrence = 2
				source, tasks, expected := writeFiniteReplaySource(t, ids, false, 0, finiteCallGap)
				type callStart struct {
					source string
					at     time.Time
				}
				starts := make(chan callStart, len(expected)*callsPerOccurrence)
				wiring := finiteReplayWiring(tasks)
				wiring.NewSandbox = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
					return SandboxInstance{Sandbox: &finiteReplaySandbox{start: func(context.Context) error { time.Sleep(preparationDelay); return nil }}, Resources: &stubResources{}}, nil
				}
				wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
					return &finiteReplayCaller{
						start: func(context.Context) error { time.Sleep(preparationDelay); return nil },
						run: func(ctx context.Context, plan replay.Plan) error {
							client := finiteStatClient{started: func() { starts <- callStart{plan.SourceTaskID, time.Now()} }}
							callCtx, cancel := context.WithTimeout(ctx, preparationDelay+preparationDelay)
							defer cancel()
							return bridgegrpc.ReplayCalls(callCtx, finiteCallGap, client, plan)
						},
					}, nil
				}
				if err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, len(ids)), io.Discard, Dependencies{Wiring: wiring}); err != nil {
					t.Fatal(err)
				}
				close(starts)
				bySource := map[string][]time.Time{}
				for start := range starts {
					bySource[start.source] = append(bySource[start.source], start.at)
				}
				for _, source := range expected {
					calls := bySource[source.TaskID]
					if len(calls) != callsPerOccurrence {
						t.Fatalf("source %s started %d calls", source.TaskID, len(calls))
					}
					if gap := calls[len(calls)-1].Sub(calls[0]); gap != finiteCallGap {
						t.Fatalf("initial preparation consumed source spacing: source=%s gap=%s want=%s", source.TaskID, gap, finiteCallGap)
					}
				}
			})
		})
	}
}

func TestReplayRunArmsClockOnlyForFirstReadyCalls(t *testing.T) {
	for _, stage := range []string{"first empty", "first start fails", "later readiness", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const concurrency = 1
				const preparationDelay = finiteCallGap + finiteCallGap
				source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false, 0, finiteCallGap)
				if stage == "first empty" {
					empty, _, _ := writeFiniteReplaySource(t, []string{"a"}, true)
					for _, name := range []string{"runtime.json", "tool-calls.jsonl"} {
						from := filepath.Join(empty, expected[0].TaskID, "bridge", name)
						data, err := os.ReadFile(from)
						if err != nil {
							t.Fatal(err)
						}
						to := filepath.Join(source, expected[0].TaskID, "bridge", name)
						if err := os.WriteFile(to, data, 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("first caller start failed")
				type execution struct {
					plan            replay.Plan
					ready, finished time.Time
					calls           []time.Time
				}
				var executions []execution
				constructed := 0
				wiring := finiteReplayWiring(tasks)
				wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
					index := constructed
					constructed++
					var ready time.Time
					return &finiteReplayCaller{
						start: func(context.Context) error {
							if index == 0 && stage == "first start fails" {
								return failure
							}
							if index == 0 && stage == "canceled" {
								cancel()
								return nil
							}
							time.Sleep(preparationDelay)
							ready = time.Now()
							return nil
						},
						run: func(ctx context.Context, plan replay.Plan) error {
							starts := make(chan time.Time, len(plan.Calls))
							client := finiteStatClient{started: func() { starts <- time.Now() }}
							callCtx, done := context.WithTimeout(ctx, preparationDelay+preparationDelay)
							defer done()
							err := bridgegrpc.ReplayCalls(callCtx, finiteCallGap, client, plan)
							close(starts)
							record := execution{plan: plan, ready: ready, finished: time.Now()}
							for at := range starts {
								record.calls = append(record.calls, at)
							}
							executions = append(executions, record)
							return err
						},
					}, nil
				}
				err := runCommandForTest(ctx, finiteReplayProfile(t, source, concurrency), io.Discard, Dependencies{Wiring: wiring})
				switch stage {
				case "canceled":
					if !errors.Is(err, context.Canceled) || len(executions) != 0 || constructed != concurrency {
						t.Fatalf("cancellation admitted or ran more work: constructed=%d executions=%d err=%v", constructed, len(executions), err)
					}
					return
				case "first start fails":
					if !errors.Is(err, failure) || len(executions) != len(expected)-concurrency {
						t.Fatalf("failed startup blocked healthy occurrence: executions=%d err=%v", len(executions), err)
					}
				default:
					if err != nil || len(executions) != len(expected) {
						t.Fatalf("execution count=%d err=%v", len(executions), err)
					}
				}
				var origin time.Time
				for _, record := range executions {
					if len(record.plan.Calls) == 0 {
						if record.plan.ReplayAt != nil || !record.finished.Equal(record.ready) {
							t.Fatalf("empty plan armed a clock: %+v", record)
						}
						continue
					}
					if record.plan.ReplayAt == nil {
						t.Fatal("nonempty plan has no shared clock")
					}
					if origin.IsZero() {
						origin = *record.plan.ReplayAt
						if !origin.Equal(record.ready) {
							t.Fatalf("clock armed before first ready caller: origin=%s ready=%s", origin, record.ready)
						}
					} else {
						if !record.plan.ReplayAt.Equal(origin) {
							t.Fatalf("later readiness reset origin: %+v", record)
						}
						if target := origin.Add(record.plan.Calls[0].StartOffset); !record.calls[0].After(target) {
							t.Fatalf("late admission was not exercised: start=%s target=%s", record.calls[0], target)
						}
					}
				}
				if origin.IsZero() {
					t.Fatal("healthy caller never armed the clock")
				}
			})
		})
	}
}

func TestReplayRunBoundsAllocationAndKeepsOriginAfterReadinessDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const concurrency = 2
		const preparationDelay = finiteCallGap + finiteCallGap
		source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b", "a", "b"}, false)
		profile := finiteReplayProfile(t, source, concurrency)
		plans := make(chan replay.Plan, len(expected))
		release := make(chan struct{})
		allocated, active, maximum := 0, 0, 0
		var mu sync.Mutex
		wiring := finiteReplayWiring(tasks)
		wiring.NewSandbox = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			mu.Lock()
			allocated++
			active++
			maximum = max(maximum, active)
			mu.Unlock()
			return SandboxInstance{Sandbox: &finiteReplaySandbox{start: func(context.Context) error {
				time.Sleep(preparationDelay)
				return nil
			}}, Resources: &stubResources{}, Close: func() error { mu.Lock(); active--; mu.Unlock(); return nil }}, nil
		}
		wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
			return &finiteReplayCaller{run: func(ctx context.Context, plan replay.Plan) error {
				plans <- plan
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}}, nil
		}
		done := make(chan error, 1)
		go func() {
			done <- runCommandForTest(context.Background(), profile, io.Discard, Dependencies{Wiring: wiring})
		}()
		synctest.Wait()
		if allocated != concurrency || active != concurrency {
			t.Fatalf("preallocated queued sandboxes: allocated=%d active=%d", allocated, active)
		}
		var origin time.Time
		var admitted []replay.Plan
		for range concurrency {
			plan := <-plans
			if plan.ReplayAt == nil {
				t.Fatal("missing shared origin")
			}
			origin = *plan.ReplayAt
			admitted = append(admitted, plan)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		close(plans)
		for plan := range plans {
			admitted = append(admitted, plan)
		}
		if maximum != concurrency || active != 0 || allocated != len(expected) || len(admitted) != len(expected) {
			t.Fatalf("allocation exceeded bound or leaked: maximum=%d active=%d allocated=%d plans=%d", maximum, active, allocated, len(admitted))
		}
		for _, plan := range admitted {
			if !plan.ReplayAt.Equal(origin) || plan.ReplayAt.Location() != time.UTC {
				t.Fatalf("readiness reset origin: %+v", plan)
			}
			for index, source := range expected {
				if source.TaskID == plan.SourceTaskID && plan.Calls[0].StartOffset != time.Duration(index)*finiteCallGap {
					t.Fatalf("source timeline changed: %+v", plan)
				}
			}
		}
	})
}

type finiteCleanupRuntime struct {
	deployment.Deployment
	present      bool
	stops        int
	closes       int
	stopFailures int
	stopErr      error
	createErr    error
	emptyID      bool
	resultError  string
	closeErr     error
}

func (runtime *finiteCleanupRuntime) Create(context.Context, deployment.Request) (string, error) {
	runtime.present = true
	if runtime.emptyID {
		return "", runtime.createErr
	}
	return "owned-runtime", runtime.createErr
}
func (*finiteCleanupRuntime) Validate(context.Context, string, deployment.Request, [][]byte) error {
	return nil
}
func (*finiteCleanupRuntime) Start(context.Context, string) error           { return nil }
func (*finiteCleanupRuntime) Running(context.Context, string) (bool, error) { return true, nil }
func (*finiteCleanupRuntime) Exec(context.Context, string, core.Command) (core.CommandResult, error) {
	return core.CommandResult{Stdout: "/app\n"}, nil
}
func (*finiteCleanupRuntime) LogsStream(context.Context, string, io.Writer, io.Writer) error {
	return nil
}
func (runtime *finiteCleanupRuntime) Stop(ctx context.Context, _ string) error {
	runtime.stops++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, bounded := ctx.Deadline(); !bounded {
		return errors.New("runtime cleanup has no deadline")
	}
	if runtime.stopErr != nil && (runtime.stopFailures == 0 || runtime.stops <= runtime.stopFailures) {
		return runtime.stopErr
	}
	runtime.present = false
	return nil
}
func (runtime *finiteCleanupRuntime) Close() error { runtime.closes++; return runtime.closeErr }
func (*finiteCleanupRuntime) UploadArchive(context.Context, string, string, io.Reader) error {
	return nil
}
func (runtime *finiteCleanupRuntime) DownloadArchive(_ context.Context, _, name string) (io.ReadCloser, deployment.FileInfo, error) {
	const privateMode = 0o600
	data, err := json.Marshal(map[string]any{"done": true, "error": runtime.resultError})
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	if err := archive.WriteHeader(&tar.Header{Name: filepath.Base(name), Typeflag: tar.TypeReg, Mode: privateMode, Size: int64(len(data))}); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if _, err := archive.Write(data); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if err := archive.Close(); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	return io.NopCloser(&buffer), deployment.FileInfo{Mode: privateMode, Size: int64(len(data))}, nil
}

type finiteCleanupEnvironment struct{}

func (*finiteCleanupEnvironment) Start(context.Context, deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: "network"}}, nil
}
func (*finiteCleanupEnvironment) Validate(context.Context) error { return nil }
func (*finiteCleanupEnvironment) Stop(context.Context) error     { return nil }

func TestReplayRunStopsAdmissionAndRetainsFailedSandboxOwner(t *testing.T) {
	const concurrency = 1
	source, tasks, _ := writeFiniteReplaySource(t, []string{"a", "b"}, false)
	failure := errors.New("runtime removal remains unconfirmed")
	var managers []*sandbox.Manager
	var runtimes []*finiteCleanupRuntime
	wiring := finiteReplayWiring(tasks)
	wiring.NewSandbox = func(_ config.Config, root, _, _ string, _ []int, logger *logrus.Logger) (SandboxInstance, error) {
		runtime := &finiteCleanupRuntime{stopErr: failure}
		manager, err := sandbox.New(sandbox.Options{OutputDir: root, Deployment: runtime,
			NewEnvironment: func() deployment.TaskEnvironment { return &finiteCleanupEnvironment{} }, Logger: logger})
		if err != nil {
			return SandboxInstance{}, err
		}
		managers = append(managers, manager)
		runtimes = append(runtimes, runtime)
		return SandboxInstance{Sandbox: manager, Resources: &stubResources{}, Close: manager.Close}, nil
	}
	wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
		return &stubReplayCaller{}, nil
	}
	t.Cleanup(func() {
		for index, manager := range managers {
			runtimes[index].stopErr = nil
			if err := manager.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	var out bytes.Buffer
	err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
	if !errors.Is(err, failure) {
		t.Fatalf("missing removal error: %v", err)
	}
	if len(runtimes) != concurrency {
		t.Errorf("admitted replacement before confirmed removal: allocated=%d limit=%d", len(runtimes), concurrency)
	}
	var result core.RunResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != concurrency || result.Summary.CleanupFailed != concurrency || result.Tasks[0].Cleanup.Status != core.StatusFailed {
		t.Errorf("missing final cleanup uncertainty: %+v", result)
	}
	for index, runtime := range runtimes {
		if !runtime.present || runtime.closes != 0 {
			t.Errorf("lost live cleanup owner: present=%v transport closes=%d", runtime.present, runtime.closes)
		}
		before := runtime.stops
		if err := managers[index].Close(); !errors.Is(err, failure) || runtime.stops <= before {
			t.Errorf("failed owner was not retained for retry: stops=%d previous=%d error=%v", runtime.stops, before, err)
		}
	}
}

func TestReplayRunReleasesCapacityAfterConfirmedSandboxRetry(t *testing.T) {
	for _, partialStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial-start=%t", partialStart), func(t *testing.T) {
			const concurrency = 1
			const failedAttempts = 1
			const closedTransport = 1
			source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false)
			failure := errors.New("first removal failed")
			var runtimes []*finiteCleanupRuntime
			wiring := finiteReplayWiring(tasks)
			wiring.NewSandbox = func(_ config.Config, root, _, _ string, _ []int, logger *logrus.Logger) (SandboxInstance, error) {
				for _, runtime := range runtimes {
					if runtime.present {
						t.Error("replacement allocated before confirmed removal")
					}
				}
				runtime := &finiteCleanupRuntime{}
				if len(runtimes) == 0 {
					runtime.stopErr, runtime.stopFailures = failure, failedAttempts
					if partialStart {
						runtime.createErr = errors.New("partial runtime creation failed")
					}
				}
				manager, err := sandbox.New(sandbox.Options{OutputDir: root, Deployment: runtime,
					NewEnvironment: func() deployment.TaskEnvironment { return &finiteCleanupEnvironment{} }, Logger: logger})
				if err != nil {
					return SandboxInstance{}, err
				}
				runtimes = append(runtimes, runtime)
				return SandboxInstance{Sandbox: manager, Resources: &stubResources{}, Close: manager.Close}, nil
			}
			wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
				return &stubReplayCaller{}, nil
			}
			var out bytes.Buffer
			err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
			if !errors.Is(err, failure) || len(runtimes) != len(expected) {
				t.Fatalf("confirmed retry blocked admission or lost prior error: allocated=%d error=%v", len(runtimes), err)
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != len(expected) || result.Summary.CleanupFailed != 0 || result.Tasks[0].Error == "" || result.Tasks[0].Cleanup.Status == core.StatusFailed {
				t.Fatalf("confirmed retry reported live ownership or lost error evidence: %+v", result)
			}
			for _, runtime := range runtimes {
				if runtime.present || runtime.closes != closedTransport {
					t.Fatalf("runtime not positively released: %+v", runtime)
				}
			}
		})
	}
}

func TestReplayRunStopsAdmissionAfterUnconfirmedPartialAllocation(t *testing.T) {
	for _, emptyID := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown-identity=%t", emptyID), func(t *testing.T) {
			const concurrency = 1
			source, tasks, _ := writeFiniteReplaySource(t, []string{"a", "b"}, false)
			var runtimes []*finiteCleanupRuntime
			wiring := finiteReplayWiring(tasks)
			wiring.NewSandbox = func(_ config.Config, root, _, _ string, _ []int, logger *logrus.Logger) (SandboxInstance, error) {
				runtime := &finiteCleanupRuntime{createErr: deployment.ErrAllocationUnconfirmed, emptyID: emptyID,
					stopErr: errors.New("partial runtime removal failed")}
				manager, err := sandbox.New(sandbox.Options{OutputDir: root, Deployment: runtime,
					NewEnvironment: func() deployment.TaskEnvironment { return &finiteCleanupEnvironment{} }, Logger: logger})
				if err != nil {
					return SandboxInstance{}, err
				}
				runtimes = append(runtimes, runtime)
				return SandboxInstance{Sandbox: manager, Resources: &stubResources{}, Close: manager.Close}, nil
			}
			wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
				return &stubReplayCaller{}, nil
			}
			var out bytes.Buffer
			err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
			if !errors.Is(err, deployment.ErrAllocationUnconfirmed) || len(runtimes) != concurrency {
				t.Errorf("partial allocation uncertainty allowed replacement: allocated=%d error=%v", len(runtimes), err)
			}
			for _, runtime := range runtimes {
				if !runtime.present || runtime.closes != 0 {
					t.Errorf("partial runtime lost its retry transport: %+v", runtime)
				}
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != concurrency || result.Summary.CleanupFailed != concurrency || result.Tasks[0].Cleanup.Status != core.StatusFailed {
				t.Errorf("partial allocation uncertainty missing from result: %+v", result)
			}
		})
	}
}

func TestReplayRunHandlesCallerCleanupUncertainty(t *testing.T) {
	const concurrency = 1
	const failedStopPhases = 2
	const closedTransport = 1
	for _, stage := range []string{"persistent", "transient", "partial start", "unknown identity", "mixed stop error", "response mismatch"} {
		t.Run(stage, func(t *testing.T) {
			source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false)
			failure := errors.New("caller removal remains unconfirmed")
			const responseError = "strict replay response mismatch"
			var runtimes []*finiteCleanupRuntime
			var callers []*managed.Caller
			wiring := finiteReplayWiring(tasks)
			wiring.NewSandbox = func(_ config.Config, root, _, _ string, _ []int, logger *logrus.Logger) (SandboxInstance, error) {
				manager, err := sandbox.New(sandbox.Options{OutputDir: root, Deployment: &finiteCleanupRuntime{},
					NewEnvironment: func() deployment.TaskEnvironment { return &finiteCleanupEnvironment{} }, Logger: logger})
				if err != nil {
					return SandboxInstance{}, err
				}
				return SandboxInstance{Sandbox: manager, Resources: &stubResources{}, Close: manager.Close}, nil
			}
			wiring.NewBridge = func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
				return &finiteReplayBridge{endpoint: core.ToolEndpoint{Protocol: "grpc", Address: "bridge:8443"}, stop: func(context.Context) error { return nil }}, nil
			}
			wiring.NewReplayCaller = func(_ config.Config, placement core.RuntimePlacement, timeout time.Duration, _ *logrus.Logger) (runner.ReplayCaller, error) {
				for _, runtime := range runtimes {
					if runtime.present {
						t.Error("replacement allocated before confirmed caller removal")
					}
				}
				runtime := &finiteCleanupRuntime{}
				if len(runtimes) == 0 {
					if stage == "response mismatch" {
						runtime.resultError = responseError
					} else {
						runtime.stopErr = failure
					}
					switch stage {
					case "transient":
						runtime.stopFailures = failedStopPhases
					case "partial start":
						runtime.createErr = errors.New("caller partial creation failed")
					case "unknown identity":
						runtime.createErr, runtime.emptyID = deployment.ErrAllocationUnconfirmed, true
					case "mixed stop error":
						runtime.stopErr = errors.Join(&runner.TransportCloseError{Err: errors.New("other transport close failed")}, failure)
					}
				}
				caller, err := managed.New(managed.Options{Runtime: runtime, Image: finiteImage, Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: timeout})
				if err != nil {
					return nil, err
				}
				runtimes, callers = append(runtimes, runtime), append(callers, caller)
				return caller, nil
			}
			t.Cleanup(func() {
				for index, caller := range callers {
					if runtimes[index].emptyID {
						continue
					}
					runtimes[index].stopErr = nil
					if err := caller.Stop(context.Background()); err != nil {
						t.Error(err)
					}
				}
			})
			var out bytes.Buffer
			err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if stage == "transient" || stage == "response mismatch" {
				if len(runtimes) != len(expected) || len(result.Tasks) != len(expected) || result.Summary.CleanupFailed != 0 || result.Tasks[0].Cleanup.Status != core.StatusSucceeded || result.Tasks[0].Error == "" {
					t.Fatalf("confirmed caller release blocked admission or lost error evidence: allocated=%d result=%+v error=%v", len(runtimes), result, err)
				}
				for _, runtime := range runtimes {
					if runtime.present || runtime.closes != closedTransport {
						t.Errorf("confirmed caller release lost: %+v", runtime)
					}
				}
			} else {
				if len(runtimes) != concurrency || len(result.Tasks) != concurrency || result.Summary.CleanupFailed != concurrency || result.Tasks[0].Cleanup.Status != core.StatusFailed {
					t.Fatalf("unconfirmed caller release allowed admission: allocated=%d result=%+v error=%v", len(runtimes), result, err)
				}
				if !runtimes[0].present || runtimes[0].closes != 0 {
					t.Errorf("caller lost ownership or retry transport: %+v", runtimes[0])
				}
			}
			switch stage {
			case "response mismatch":
				if err == nil || !strings.Contains(err.Error(), responseError) {
					t.Errorf("missing strict response failure: %v", err)
				}
			case "unknown identity":
				if !errors.Is(err, deployment.ErrAllocationUnconfirmed) {
					t.Errorf("missing unknown allocation error: %v", err)
				}
			default:
				if !errors.Is(err, failure) {
					t.Errorf("missing original caller removal failure: %v", err)
				}
			}
		})
	}
}

func TestReplayRunContinuesAfterTransportCloseFailure(t *testing.T) {
	for _, owner := range []string{"sandbox", "caller"} {
		t.Run(owner, func(t *testing.T) {
			const concurrency = 1
			const oneRemoval = 1
			source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false)
			original := snapshotFiniteReplaySource(t, source)
			failure := errors.New("runtime transport close failed after confirmed removal")
			var sandboxes, callers []*finiteCleanupRuntime
			wiring := finiteReplayWiring(tasks)
			wiring.NewSandbox = func(_ config.Config, root, _, _ string, _ []int, logger *logrus.Logger) (SandboxInstance, error) {
				runtime := &finiteCleanupRuntime{}
				if owner == "sandbox" && len(sandboxes) == 0 {
					runtime.closeErr = failure
				}
				manager, err := sandbox.New(sandbox.Options{OutputDir: root, Deployment: runtime,
					NewEnvironment: func() deployment.TaskEnvironment { return &finiteCleanupEnvironment{} }, Logger: logger})
				if err != nil {
					return SandboxInstance{}, err
				}
				sandboxes = append(sandboxes, runtime)
				return SandboxInstance{Sandbox: manager, Resources: &stubResources{}, Close: manager.Close}, nil
			}
			wiring.NewBridge = func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
				return &finiteReplayBridge{endpoint: core.ToolEndpoint{Protocol: "grpc", Address: "bridge:8443"}, stop: func(context.Context) error { return nil }}, nil
			}
			wiring.NewReplayCaller = func(_ config.Config, placement core.RuntimePlacement, timeout time.Duration, _ *logrus.Logger) (runner.ReplayCaller, error) {
				runtime := &finiteCleanupRuntime{}
				if owner == "caller" && len(callers) == 0 {
					runtime.closeErr = failure
				}
				callers = append(callers, runtime)
				return managed.New(managed.Options{Runtime: runtime, Image: finiteImage, Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: timeout})
			}
			var out bytes.Buffer
			err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
			if !errors.Is(err, failure) || errors.Is(err, errReplayCleanupUnconfirmed) {
				t.Errorf("transport close failure lost or classified as unsafe admission: %v", err)
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != len(expected) || len(sandboxes) != len(expected) || len(callers) != len(expected) {
				t.Errorf("confirmed removal blocked replacement: tasks=%d sandboxes=%d callers=%d", len(result.Tasks), len(sandboxes), len(callers))
			}
			for _, runtimes := range [][]*finiteCleanupRuntime{sandboxes, callers} {
				for _, runtime := range runtimes {
					if runtime.present || runtime.stops != oneRemoval {
						t.Errorf("runtime removal was not confirmed or was repeated: %+v", runtime)
					}
				}
			}
			for index, task := range result.Tasks {
				grade := expected[index].Evaluation
				grade.Reused, grade.SourceRunID, grade.SourceTaskID = true, finiteSourceRun, expected[index].TaskID
				if task.Isolation.Status != core.StatusConfirmed || !task.Isolation.CallerStopped || !task.Isolation.BridgeRevoked || task.Replay.Status != core.StatusSucceeded || !reflect.DeepEqual(task.Evaluation, grade) {
					t.Errorf("transport error changed isolation, replay, or inherited grade: %+v", task)
				}
			}
			if result.Tasks[0].Cleanup.Status != core.StatusFailed || !strings.Contains(result.Tasks[0].Cleanup.Error, failure.Error()) || !strings.Contains(result.Tasks[0].Error, failure.Error()) {
				t.Errorf("transport close error missing from cleanup evidence: %+v", result.Tasks[0])
			}
			if !reflect.DeepEqual(original, snapshotFiniteReplaySource(t, source)) {
				t.Error("transport cleanup changed the source artifacts")
			}
		})
	}
}

func TestReplayRunRetriesSandboxStopWithoutRelyingOnClientClose(t *testing.T) {
	for _, transient := range []bool{false, true} {
		t.Run(fmt.Sprintf("transient=%t", transient), func(t *testing.T) {
			const concurrency = 1
			const failedAttempt = 1
			source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false)
			failure := errors.New("sandbox Stop did not confirm removal")
			allocated, live := 0, 0
			wiring := finiteReplayWiring(tasks)
			wiring.NewSandbox = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
				if live != 0 {
					t.Error("client close permitted replacement without a positive Stop")
				}
				first := allocated == 0
				allocated++
				stops := 0
				active := false
				var closeClient func() error
				if !transient {
					closeClient = func() error { return nil }
				}
				return SandboxInstance{Sandbox: &finiteReplaySandbox{
					start: func(context.Context) error { active = true; live++; return nil },
					stop: func(ctx context.Context) error {
						stops++
						if ctx.Err() != nil {
							t.Errorf("cleanup inherited cancellation: %v", ctx.Err())
						}
						if _, bounded := ctx.Deadline(); !bounded {
							t.Error("cleanup has no deadline")
						}
						if first && (!transient || stops == failedAttempt) {
							return failure
						}
						if active {
							active = false
							live--
						}
						return nil
					},
				}, Resources: &stubResources{}, Close: closeClient}, nil
			}
			wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
				return &stubReplayCaller{}, nil
			}
			var out bytes.Buffer
			err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
			if !errors.Is(err, failure) {
				t.Fatalf("missing first cleanup failure: %v", err)
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if transient {
				if allocated != len(expected) || live != 0 || result.Tasks[0].Cleanup.Status != core.StatusSucceeded {
					t.Fatalf("positive Stop retry did not release capacity: allocated=%d live=%d result=%+v", allocated, live, result)
				}
			} else if allocated != concurrency || live != concurrency || result.Tasks[0].Cleanup.Status != core.StatusFailed {
				t.Fatalf("unconfirmed Stop released capacity: allocated=%d live=%d result=%+v", allocated, live, result)
			}
		})
	}
}

func TestReplayRunDrainsAdmittedPeersAfterCleanupUncertainty(t *testing.T) {
	for _, stage := range []string{"persistent", "recovered", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const concurrency = 2
				source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b", "a", "b"}, false)
				firstID := expected[0].TaskID
				failure := errors.New("first sandbox removal remains unconfirmed")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				started := make(chan string, len(expected))
				finishFirst, finishPeer := make(chan struct{}), make(chan struct{})
				var mu sync.Mutex
				managers := map[string]*sandbox.Manager{}
				runtimes := map[string]*finiteCleanupRuntime{}
				wiring := finiteReplayWiring(tasks)
				wiring.NewSandbox = func(_ config.Config, root, _, id string, _ []int, logger *logrus.Logger) (SandboxInstance, error) {
					runtime := &finiteCleanupRuntime{}
					if id == firstID {
						runtime.stopErr = failure
					}
					manager, err := sandbox.New(sandbox.Options{OutputDir: root, Deployment: runtime,
						NewEnvironment: func() deployment.TaskEnvironment { return &finiteCleanupEnvironment{} }, Logger: logger})
					if err != nil {
						return SandboxInstance{}, err
					}
					mu.Lock()
					managers[id], runtimes[id] = manager, runtime
					mu.Unlock()
					return SandboxInstance{Sandbox: manager, Resources: &stubResources{}, Close: manager.Close}, nil
				}
				wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
					return &finiteReplayCaller{run: func(ctx context.Context, plan replay.Plan) error {
						started <- plan.SourceTaskID
						release := finishPeer
						if plan.SourceTaskID == firstID {
							release = finishFirst
						}
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}}, nil
				}
				var out bytes.Buffer
				done := make(chan error, concurrency)
				go func() {
					done <- runCommandForTest(ctx, finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
				}()
				for range concurrency {
					<-started
				}
				close(finishFirst)
				synctest.Wait()
				if len(runtimes) != concurrency || !runtimes[firstID].present || runtimes[firstID].closes != 0 {
					t.Errorf("failed owner was replaced or discarded while peer ran: %+v", runtimes)
				}
				select {
				case <-done:
					t.Fatal("run returned before admitted peer drained")
				default:
				}
				if stage == "recovered" {
					runtimes[firstID].stopErr = nil
				}
				if stage == "canceled" {
					cancel()
				} else {
					close(finishPeer)
				}
				err := <-done
				if !errors.Is(err, failure) || stage == "canceled" && !errors.Is(err, context.Canceled) {
					t.Errorf("lost cleanup or cancellation error: %v", err)
				}
				var result core.RunResult
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				wantFirstCleanup := core.StatusFailed
				if stage == "recovered" {
					wantFirstCleanup = core.StatusSucceeded
					if runtimes[firstID].present || result.Summary.CleanupFailed != 0 || result.Tasks[0].Error == "" {
						t.Errorf("drain-time retry did not confirm release or retain prior error: runtime=%+v result=%+v", runtimes[firstID], result)
					}
				}
				if len(runtimes) != concurrency || len(result.Tasks) != concurrency || result.Tasks[0].Cleanup.Status != wantFirstCleanup || result.Tasks[len(result.Tasks)-1].Cleanup.Status != core.StatusSucceeded {
					t.Errorf("peer cleanup or admission failed: runtimes=%+v result=%+v", runtimes, result)
				}
				for id, manager := range managers {
					if id != firstID && runtimes[id].present {
						t.Error("admitted peer runtime remains present")
					}
					runtimes[id].stopErr = nil
					if err := manager.Close(); err != nil {
						t.Error(err)
					}
				}
			})
		})
	}
}

func TestReplayRunRejectsInvalidWorkloadBeforeSandboxStart(t *testing.T) {
	const concurrency = 1
	source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false)
	path := filepath.Join(source, expected[len(expected)-1].TaskID, "bridge", "runtime.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	wiring := finiteReplayWiring(tasks)
	wiring.NewSandbox = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
		t.Error("invalid final source allocated a task sandbox")
		return SandboxInstance{}, errors.New("unexpected sandbox")
	}
	wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
		t.Error("invalid final source allocated a caller")
		return nil, errors.New("unexpected caller")
	}
	if err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), io.Discard, Dependencies{Wiring: wiring}); err == nil || !strings.Contains(err.Error(), "no matching logical task") {
		t.Fatalf("invalid whole-run preflight = %v", err)
	}
}

func TestReplayRunRevalidatesQueuedSourceBeforeSandboxStart(t *testing.T) {
	const concurrency = 1
	const firstOccurrenceCount = 1
	source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "b"}, false)
	path := filepath.Join(source, expected[len(expected)-1].TaskID, "bridge", "runtime.json")
	wiring := finiteReplayWiring(tasks)
	preflight := wiring.PreflightReplaySource
	wiring.PreflightReplaySource = func(ctx context.Context, cfg config.Config, tasks []core.Task, logger *logrus.Entry) (*bridgegrpc.ReplaySelection, error) {
		selection, err := preflight(ctx, cfg, tasks, logger)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return selection, nil
	}
	starts := 0
	wiring.NewSandbox = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
		return SandboxInstance{Sandbox: &finiteReplaySandbox{start: func(context.Context) error { starts++; return nil }}, Resources: &stubResources{}}, nil
	}
	wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
		return &finiteReplayCaller{run: func(context.Context, replay.Plan) error { return nil }}, nil
	}
	var out bytes.Buffer
	err := runCommandForTest(context.Background(), finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
	if err == nil || !strings.Contains(err.Error(), "changed after preflight") || starts != firstOccurrenceCount {
		t.Fatalf("queued source revalidation: starts=%d err=%v", starts, err)
	}
	var result core.RunResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Tasks) != len(expected) || result.Tasks[len(result.Tasks)-1].Replay.Status != core.StatusFailed {
		t.Fatalf("missing queued failure: %+v", result)
	}
}

type finiteReplayBridge struct {
	stubBridge
	startErr error
	stop     func(context.Context) error
	endpoint core.ToolEndpoint
}

func (bridge *finiteReplayBridge) Start(context.Context, runner.Sandbox) (core.ToolEndpoint, error) {
	return bridge.endpoint, bridge.startErr
}
func (bridge *finiteReplayBridge) Stop(ctx context.Context) error { return bridge.stop(ctx) }

func TestReplayRunCleansFailuresAndCancellation(t *testing.T) {
	for _, stage := range []string{"caller start", "caller run", "bridge start", "canceled"} {
		t.Run(stage, func(t *testing.T) {
			const concurrency = 1
			source, tasks, expected := writeFiniteReplaySource(t, []string{"a", "a"}, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New(stage)
			var events []string
			allocated, closed := 0, 0
			cleanup := func(ctx context.Context, event string) error {
				if ctx.Err() != nil {
					t.Errorf("%s used canceled cleanup context: %v", event, ctx.Err())
				}
				if _, bounded := ctx.Deadline(); !bounded {
					t.Errorf("%s has no cleanup deadline", event)
				}
				events = append(events, event)
				return nil
			}
			wiring := finiteReplayWiring(tasks)
			wiring.NewSandbox = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
				allocated++
				return SandboxInstance{Sandbox: &finiteReplaySandbox{stop: func(ctx context.Context) error { return cleanup(ctx, "sandbox-stop") }}, Resources: &stubResources{}, Close: func() error { closed++; return nil }}, nil
			}
			wiring.NewBridge = func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error) {
				bridge := &finiteReplayBridge{stop: func(ctx context.Context) error { return cleanup(ctx, "bridge-stop") }}
				if stage == "bridge start" {
					bridge.startErr = failure
				}
				return bridge, nil
			}
			wiring.NewReplayCaller = func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error) {
				started, stopped := false, false
				return &finiteReplayCaller{
					start: func(context.Context) error {
						started = true
						if stage == "caller start" {
							return failure
						}
						return nil
					},
					run: func(context.Context, replay.Plan) error {
						if stage == "canceled" {
							cancel()
							return ctx.Err()
						}
						return failure
					},
					stop: func(ctx context.Context) error {
						if !started || stopped {
							return nil
						}
						stopped = true
						return cleanup(ctx, "caller-stop")
					},
				}, nil
			}
			var out bytes.Buffer
			err := runCommandForTest(ctx, finiteReplayProfile(t, source, concurrency), &out, Dependencies{Wiring: wiring})
			if stage == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel error = %v", err)
				}
				expected = expected[:concurrency]
			} else if !errors.Is(err, failure) {
				t.Fatalf("failure = %v", err)
			}
			if allocated != len(expected) || closed != allocated {
				t.Fatalf("runtime owners leaked: allocated=%d closed=%d", allocated, closed)
			}
			want := []string{}
			for range expected {
				if stage != "bridge start" {
					want = append(want, "caller-stop")
				}
				want = append(want, "bridge-stop", "sandbox-stop")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("cleanup order=%v want=%v", events, want)
			}
			var result core.RunResult
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Tasks) != len(expected) || result.Infrastructure.Cleanup.Status != core.StatusSucceeded {
				t.Fatalf("missing results or shared cleanup: %+v", result)
			}
			for _, task := range result.Tasks {
				if task.Cleanup.Status != core.StatusSucceeded || !task.Evaluation.Reused || task.Error == "" {
					t.Fatalf("lost failed occurrence: %+v", task)
				}
			}
		})
	}
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
