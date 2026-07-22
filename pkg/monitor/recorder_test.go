package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/sirupsen/logrus"
)

type fakeSource struct {
	mu       sync.Mutex
	calls    int
	closes   int
	sample   func(context.Context, int) ([]core.ResourceReading, error)
	closeErr error
}

func (source *fakeSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	source.mu.Lock()
	source.calls++
	call := source.calls
	callback := source.sample
	source.mu.Unlock()
	if callback == nil {
		return nil, nil
	}
	return callback(ctx, call)
}

func (source *fakeSource) Close() error {
	source.mu.Lock()
	defer source.mu.Unlock()
	source.closes++
	return source.closeErr
}

func (source *fakeSource) counts() (int, int) {
	source.mu.Lock()
	defer source.mu.Unlock()
	return source.calls, source.closes
}

func testReading(component, id string, observed time.Time, cpu uint64) core.ResourceReading {
	return core.ResourceReading{
		TaskID: "fix-git", Component: component, RuntimeID: id, RuntimeName: "aries-" + component,
		ObservedAt: observed, CPUUsageNanoseconds: cpu,
		MemoryUsageBytes: 4096, MemoryLimitBytes: 8192,
	}
}

func newTestRecorder(t *testing.T, source ResourceSource, outputDir string, interval time.Duration) *Recorder {
	t.Helper()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	recorder, err := New(Options{
		RunID: "run-1", TaskIDs: []string{"fix-git"}, OutputDir: outputDir, Source: source,
		Interval: interval, RequestTimeout: 200 * time.Millisecond, StopTimeout: time.Second,
		MaxSamplesPerTask: 1000, MaxFileBytes: 1 << 20, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func TestRecorderDerivesCPUAndWritesPortablePrivateArtifacts(t *testing.T) {
	started := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{sample: func(_ context.Context, call int) ([]core.ResourceReading, error) {
		observed := started.Add(time.Duration(call-1) * time.Second)
		return []core.ResourceReading{
			testReading("sandbox", "sandbox-id", observed, uint64(call-1)*500_000_000),
			testReading("harness", "harness-id", observed, uint64(call-1)*250_000_000),
		}, nil
	}}
	outputDir := filepath.Join(t.TempDir(), "run")
	recorder := newTestRecorder(t, source, outputDir, 10*time.Millisecond)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { calls, _ := source.counts(); return calls >= 3 })
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := reports["fix-git"]
	if report.Status != core.StatusSucceeded || report.SampleCount < 6 || len(report.LogPaths) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if filepath.Dir(report.LogPaths[0]) != filepath.Join(outputDir, "fix-git", "monitor") {
		t.Fatalf("resource path = %s", report.LogPaths[0])
	}
	samples := readSamplesStrict(t, report.LogPaths[0])
	seenPositive := map[string]bool{}
	for index, sample := range samples {
		if sample.Sequence != uint64(index) || sample.TaskID != "fix-git" || sample.RuntimeID == "" || sample.RuntimeName == "" || sample.MemoryUsageBytes != 4096 || sample.MemoryLimitBytes != 8192 {
			t.Fatalf("sample[%d] = %+v", index, sample)
		}
		if sample.CPUPercent > 0 {
			seenPositive[sample.Component] = true
		}
	}
	if !seenPositive["sandbox"] || !seenPositive["harness"] {
		t.Fatalf("positive CPU components = %#v", seenPositive)
	}
	index := readIndexStrict(t, report.LogPaths[1])
	if index.SchemaVersion != 2 || index.SampleCount != uint64(len(samples)) || len(index.Components) != 2 {
		t.Fatalf("index = %+v", index)
	}
	for _, path := range append([]string{filepath.Dir(report.LogPaths[0])}, report.LogPaths...) {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}

func TestRecorderCPUBaselinesHandleIdleDisappearAndRejectRegression(t *testing.T) {
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{}
	recorder := newTestRecorder(t, source, filepath.Join(t.TempDir(), "run"), time.Hour)
	source.sample = func(context.Context, int) ([]core.ResourceReading, error) {
		return []core.ResourceReading{testReading("sandbox", "runtime", t0, 0)}, nil
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.sample = func(context.Context, int) ([]core.ResourceReading, error) {
		return []core.ResourceReading{testReading("sandbox", "runtime", t0.Add(time.Second), 500_000_000)}, nil
	}
	if err := recorder.sample(context.Background(), 1, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	source.sample = func(context.Context, int) ([]core.ResourceReading, error) { return nil, nil }
	if err := recorder.sample(context.Background(), 2, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	source.sample = func(context.Context, int) ([]core.ResourceReading, error) {
		return []core.ResourceReading{testReading("sandbox", "runtime", t0.Add(3*time.Second), 900_000_000)}, nil
	}
	if err := recorder.sample(context.Background(), 3, t0.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	source.sample = func(context.Context, int) ([]core.ResourceReading, error) {
		return []core.ResourceReading{testReading("sandbox", "runtime", t0.Add(4*time.Second), 800_000_000)}, nil
	}
	if err := recorder.sample(context.Background(), 4, t0.Add(4*time.Second)); err == nil || !strings.Contains(err.Error(), "CPU counter decreased") {
		t.Fatalf("regression error = %v", err)
	}
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	samples := readSamplesStrict(t, filepath.Join(recorder.outputDir, "fix-git", "monitor", "resources.jsonl"))
	if len(samples) != 3 || samples[0].CPUPercent != 0 || samples[1].CPUPercent != 50 || samples[2].CPUPercent != 0 {
		t.Fatalf("CPU samples = %+v", samples)
	}
}

func TestRecorderScopesCPUBaselinesByTask(t *testing.T) {
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{sample: func(_ context.Context, call int) ([]core.ResourceReading, error) {
		observed := t0.Add(time.Duration(call-1) * time.Second)
		first := testReading("sandbox", "shared-runtime", observed, uint64(call-1)*500_000_000)
		first.TaskID = "task-a"
		second := testReading("sandbox", "shared-runtime", observed, uint64(call-1)*250_000_000)
		second.TaskID = "task-b"
		return []core.ResourceReading{first, second}, nil
	}}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	recorder, err := New(Options{
		RunID: "run-1", TaskIDs: []string{"task-a", "task-b"}, OutputDir: filepath.Join(t.TempDir(), "run"),
		Source: source, Interval: time.Hour, RequestTimeout: time.Second, StopTimeout: time.Second,
		MaxSamplesPerTask: 10, MaxFileBytes: 1 << 20, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recorder.sample(context.Background(), 1, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	for task, want := range map[string]float64{"task-a": 50, "task-b": 25} {
		samples := readSamplesStrict(t, filepath.Join(recorder.outputDir, task, "monitor", "resources.jsonl"))
		if len(samples) != 2 || samples[1].CPUPercent != want {
			t.Fatalf("%s CPU samples = %+v, want second percentage %v", task, samples, want)
		}
	}
}

func TestRecorderReportsBackgroundSourceFailure(t *testing.T) {
	t0 := time.Now().UTC()
	source := &fakeSource{sample: func(_ context.Context, call int) ([]core.ResourceReading, error) {
		if call > 1 {
			return nil, errors.New("resource source unavailable")
		}
		return []core.ResourceReading{testReading("sandbox", "runtime", t0, 1)}, nil
	}}
	recorder := newTestRecorder(t, source, filepath.Join(t.TempDir(), "run"), 5*time.Millisecond)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorder.sampleDone:
	case <-time.After(time.Second):
		t.Fatal("background sampler did not stop")
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report := reports["fix-git"]; report.Status != core.StatusFailed || !strings.Contains(report.Error, "resource source unavailable") || report.SampleCount != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestRecorderFailedStartRollsBackAndCanRetry(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	t0 := time.Now().UTC()
	source := &fakeSource{sample: func(context.Context, int) ([]core.ResourceReading, error) {
		if fail.Load() {
			return nil, errors.New("source broken")
		}
		return []core.ResourceReading{testReading("sandbox", "runtime", t0, 1)}, nil
	}}
	outputDir := filepath.Join(t.TempDir(), "run")
	recorder := newTestRecorder(t, source, outputDir, time.Hour)
	if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "source broken") {
		t.Fatalf("Start error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "fix-git", "monitor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial monitor directory remains: %v", err)
	}
	fail.Store(false)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderConcurrentStopCachesIndependentReports(t *testing.T) {
	source := &fakeSource{}
	recorder := newTestRecorder(t, source, filepath.Join(t.TempDir(), "run"), time.Hour)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	results := make(chan map[string]core.ObserverResult, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := recorder.Stop(context.Background())
			if err != nil {
				t.Errorf("Stop: %v", err)
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	var first map[string]core.ObserverResult
	for result := range results {
		if first == nil {
			first = result
		} else if !reflect.DeepEqual(first, result) {
			t.Fatalf("Stop results differ: %+v != %+v", first, result)
		}
	}
	want := cloneReports(first)
	mutated := first["fix-git"]
	mutated.LogPaths[0] = "caller mutation"
	first["fix-git"] = mutated
	again, err := recorder.Stop(context.Background())
	if err != nil || !reflect.DeepEqual(want, again) {
		t.Fatalf("cached Stop = %+v, %v", again, err)
	}
}

func TestPrepareArtifactsJoinsRollbackFailures(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(filepath.Join(outputDir, "task-b", "monitor"), 0o700); err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("close failure")
	removeFailure := errors.New("remove failure")
	operations := defaultArtifactOperations()
	operations.closeFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return closeFailure
	}
	operations.remove = func(path string) error {
		err := os.Remove(path)
		if strings.HasSuffix(path, filepath.Join("task-a", "monitor", "resources.jsonl")) && err == nil {
			return removeFailure
		}
		return err
	}
	_, _, _, err := prepareArtifacts(outputDir, []string{"task-a", "task-b"}, operations)
	if err == nil || !errors.Is(err, closeFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("prepareArtifacts error = %v", err)
	}
}

func TestNewRejectsUnsafeAndIncompleteConfiguration(t *testing.T) {
	base := Options{RunID: "run-1", TaskIDs: []string{"fix-git"}, OutputDir: t.TempDir(), Source: &fakeSource{}}
	cases := []struct {
		name, message string
		mutate        func(*Options)
	}{
		{name: "run path", message: "unsafe character", mutate: func(options *Options) { options.RunID = "../run" }},
		{name: "task path", message: "unsafe character", mutate: func(options *Options) { options.TaskIDs = []string{"task/one"} }},
		{name: "duplicate", message: "repeated", mutate: func(options *Options) { options.TaskIDs = []string{"fix-git", "fix-git"} }},
		{name: "source", message: "resource source", mutate: func(options *Options) { options.Source = nil }},
		{name: "samples", message: "sample bound", mutate: func(options *Options) { options.MaxSamplesPerTask = -1 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			options := base
			options.TaskIDs = append([]string(nil), base.TaskIDs...)
			test.mutate(&options)
			if _, err := New(options); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("New error = %v", err)
			}
		})
	}
}

func readSamplesStrict(t *testing.T, path string) []ResourceSample {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var samples []ResourceSample
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		var sample ResourceSample
		if err := decoder.Decode(&sample); err != nil {
			t.Fatal(err)
		}
		if err := requireJSONEOF(decoder); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, sample)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return samples
}

func readIndexStrict(t *testing.T, path string) Index {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var index Index
	if err := decoder.Decode(&index); err != nil {
		t.Fatal(err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		t.Fatal(err)
	}
	return index
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("JSON contains a trailing value")
		}
		return err
	}
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition was not met before timeout")
		}
		time.Sleep(time.Millisecond)
	}
}
