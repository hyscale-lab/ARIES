package monitor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	taskContainerID = "1111111111111111111111111111111111111111111111111111111111111111"
	harnessID       = "2222222222222222222222222222222222222222222222222222222222222222"
	initializerID   = "3333333333333333333333333333333333333333333333333333333333333333"
	foreignID       = "4444444444444444444444444444444444444444444444444444444444444444"
)

type fakeEngine struct {
	mu            sync.Mutex
	requests      []engineRequest
	listCalls     int
	statsCalls    int
	idleCloses    int
	statsByID     map[string]int
	listResponse  func(int) (int, string)
	statsResponse func(*http.Request, string, int, int) (int, string)
}

type engineRequest struct {
	method string
	path   string
	query  string
}

type fakeRecorderClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeRecorderClock() *fakeRecorderClock {
	return &fakeRecorderClock{now: time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)}
}

func (clock *fakeRecorderClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeRecorderClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	engine := &fakeEngine{statsByID: make(map[string]int)}
	engine.listResponse = func(int) (int, string) { return http.StatusOK, "[]" }
	engine.statsResponse = func(_ *http.Request, _ string, _, _ int) (int, string) {
		return http.StatusOK, validStatsJSON(300, 100, 2000, 1000, 2, 4096, 8192)
	}
	return engine
}

func (engine *fakeEngine) RoundTrip(request *http.Request) (*http.Response, error) {
	engine.mu.Lock()
	engine.requests = append(engine.requests, engineRequest{method: request.Method, path: request.URL.Path, query: request.URL.RawQuery})
	if request.Method != http.MethodGet {
		engine.mu.Unlock()
		return fakeHTTPResponse(request, http.StatusMethodNotAllowed, "method not allowed"), nil
	}
	if request.URL.Path == "/containers/json" {
		engine.listCalls++
		call := engine.listCalls
		callback := engine.listResponse
		engine.mu.Unlock()
		status, body := callback(call)
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return fakeHTTPResponse(request, status, body), nil
	}
	if strings.HasPrefix(request.URL.Path, "/containers/") && strings.HasSuffix(request.URL.Path, "/stats") {
		identifier := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/containers/"), "/stats")
		engine.statsCalls++
		engine.statsByID[identifier]++
		call := engine.statsCalls
		idCall := engine.statsByID[identifier]
		callback := engine.statsResponse
		engine.mu.Unlock()
		status, body := callback(request, identifier, call, idCall)
		if err := request.Context().Err(); err != nil {
			return nil, err
		}
		return fakeHTTPResponse(request, status, body), nil
	}
	engine.mu.Unlock()
	return fakeHTTPResponse(request, http.StatusNotFound, "not found"), nil
}

func fakeHTTPResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}

func (engine *fakeEngine) counts() (int, int) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.listCalls, engine.statsCalls
}

func (engine *fakeEngine) CloseIdleConnections() {
	engine.mu.Lock()
	engine.idleCloses++
	engine.mu.Unlock()
}

func (engine *fakeEngine) idleCloseCount() int {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return engine.idleCloses
}

func (engine *fakeEngine) recordedRequests() []engineRequest {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	return append([]engineRequest(nil), engine.requests...)
}

func validStatsJSON(total, preTotal, system, preSystem uint64, online int64, usage, limit uint64) string {
	return fmt.Sprintf(`{
  "read":"2026-07-21T00:00:00Z",
  "cpu_stats":{"cpu_usage":{"total_usage":%d,"percpu_usage":[1,1]},"system_cpu_usage":%d,"online_cpus":%d},
  "precpu_stats":{"cpu_usage":{"total_usage":%d,"percpu_usage":[1,1]},"system_cpu_usage":%d,"online_cpus":%d},
  "memory_stats":{"usage":%d,"limit":%d,"stats":{"inactive_file":1}},
  "networks":{}
}`, total, system, online, preTotal, preSystem, online, usage, limit)
}

func dockerOneShotStatsJSON() string {
	return `{
  "read":"2026-07-22T05:18:10.000000000Z",
  "preread":"0001-01-01T00:00:00Z",
  "cpu_stats":{
    "cpu_usage":{"total_usage":257638000,"usage_in_kernelmode":10000000,"usage_in_usermode":247638000},
    "system_cpu_usage":99876543210000,
    "online_cpus":16,
    "throttling_data":{"periods":0,"throttled_periods":0,"throttled_time":0}
  },
  "precpu_stats":{
    "cpu_usage":{"total_usage":0,"usage_in_kernelmode":0,"usage_in_usermode":0},
    "throttling_data":{"periods":0,"throttled_periods":0,"throttled_time":0}
  },
  "memory_stats":{"usage":73400320,"limit":1073741824,"stats":{"inactive_file":4096}},
  "networks":{}
}`
}

func invalidCurrentMemoryStatsJSON() string {
	return `{
  "cpu_stats":{"cpu_usage":{"total_usage":10,"percpu_usage":[10]},"system_cpu_usage":20,"online_cpus":1},
  "precpu_stats":{},
  "memory_stats":{"limit":1024}
}`
}

func listedJSON(runID string, includeForeign bool) string {
	containers := []listedContainer{
		listedFixture(taskContainerID, "aries-task", runID, "fix-git", taskContainerKind),
		listedFixture(harnessID, "aries-openclaw", runID, "fix-git", harnessKind),
		listedFixture(initializerID, "aries-init", runID, "fix-git", "openclaw-initializer"),
	}
	if includeForeign {
		containers = append(containers, listedFixture(foreignID, "aries-foreign", runID, "other-task", taskContainerKind))
	}
	content, err := json.Marshal(containers)
	if err != nil {
		panic(err)
	}
	return string(content)
}

func listedFixture(id, name, runID, taskID, kind string) listedContainer {
	return listedContainer{
		ID: id, Names: []string{"/" + name}, State: "running",
		Labels: map[string]string{
			"aries.managed": "true", "aries.run": runID, "aries.task": taskID, "aries.kind": kind,
		},
	}
}

func newTestRecorder(t *testing.T, engine *fakeEngine, outputDir string, interval time.Duration) *Recorder {
	t.Helper()
	recorder, err := New(Options{
		RunID: "run-1", TaskIDs: []string{"fix-git"}, OutputDir: outputDir,
		DockerSocket: filepath.Join(outputDir, "unused-docker.sock"), Interval: interval, RequestTimeout: 200 * time.Millisecond,
		StopTimeout: time.Second, MaxResponseBytes: 64 << 10, MaxSamplesPerTask: 1000, MaxFileBytes: 1 << 20,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder.engine.http = &http.Client{Transport: engine}
	return recorder
}

func TestRecorderSamplesOnlyOwnedComponentsAndWritesStrictPrivateArtifacts(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) { return http.StatusOK, listedJSON("run-1", true) }
	outputDir := filepath.Join(t.TempDir(), "run")
	recorder := newTestRecorder(t, engine, outputDir, 15*time.Millisecond)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		list, _ := engine.counts()
		return list >= 3
	})
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := reports["fix-git"]
	if report.Status != core.StatusSucceeded || report.Error != "" || report.SampleCount < 4 || report.Duration <= 0 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if len(report.LogPaths) != 2 || !filepath.IsAbs(report.LogPaths[0]) || !filepath.IsAbs(report.LogPaths[1]) {
		t.Fatalf("log paths are not canonical absolute paths: %q", report.LogPaths)
	}
	samples := readSamplesStrict(t, report.LogPaths[0])
	if len(samples) != report.SampleCount {
		t.Fatalf("sample count = %d, report = %d", len(samples), report.SampleCount)
	}
	if samples[0].Second != 0 || samples[1].Second != 0 || samples[2].Second != 1 || samples[3].Second != 1 {
		t.Fatalf("first two sampling intervals are not ordered exactly: %+v", samples[:4])
	}
	components := map[string]int{}
	lastSequence := uint64(0)
	lastSecond := uint64(0)
	for index, sample := range samples {
		if sample.Sequence != uint64(index) {
			t.Fatalf("sequence[%d] = %d", index, sample.Sequence)
		}
		if index != 0 && sample.Sequence <= lastSequence {
			t.Fatalf("sequence did not increase: %d then %d", lastSequence, sample.Sequence)
		}
		if sample.Second < lastSecond || sample.TaskID != "fix-git" || sample.CPUPercent != 40 || sample.MemoryBytes != 4096 || sample.MemoryLimitBytes != 8192 {
			t.Fatalf("invalid sample: %+v", sample)
		}
		if _, err := time.Parse(time.RFC3339Nano, sample.Time); err != nil {
			t.Fatalf("sample time %q: %v", sample.Time, err)
		}
		components[sample.Component]++
		lastSequence, lastSecond = sample.Sequence, sample.Second
	}
	if components[taskContainerKind] == 0 || components[harnessKind] == 0 || len(components) != 2 {
		t.Fatalf("component counts = %#v", components)
	}
	index := readIndexStrict(t, report.LogPaths[1])
	if index.SchemaVersion != 1 || index.RunID != "run-1" || index.TaskID != "fix-git" || index.Status != core.StatusSucceeded ||
		index.IntervalMilliseconds != 15 || index.SampleCount != uint64(len(samples)) || index.ResourcesFile != "resources.jsonl" || len(index.Components) != 2 {
		t.Fatalf("invalid index: %+v", index)
	}
	for _, path := range append([]string{filepath.Dir(report.LogPaths[0])}, report.LogPaths...) {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("mode %s = %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	assertEngineRequestWhitelist(t, engine.recordedRequests())
}

func TestRecorderIgnoresCallerCancellationAndContainerTeardown(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) { return http.StatusOK, listedJSON("run-1", false) }
	engine.statsResponse = func(_ *http.Request, id string, _, idCall int) (int, string) {
		if id == harnessID && idCall > 1 {
			return http.StatusNotFound, `{"message":"No such container"}`
		}
		return http.StatusOK, validStatsJSON(200, 100, 1500, 1000, 1, 1024, 2048)
	}
	ctx, cancel := context.WithCancel(context.Background())
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 10*time.Millisecond)
	if err := recorder.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitFor(t, time.Second, func() bool {
		list, _ := engine.counts()
		return list >= 4
	})
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reports["fix-git"].Status != core.StatusSucceeded || reports["fix-git"].SampleCount < 4 {
		t.Fatalf("report = %+v", reports["fix-git"])
	}
}

func TestRecorderDefersTransientValidationWithinBoundedGrace(t *testing.T) {
	listed := func() string {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return string(content)
	}
	valid := validStatsJSON(200, 100, 2000, 1000, 1, 100, 200)
	t.Run("several invalid ticks remain under grace", func(t *testing.T) {
		engine := newFakeEngine(t)
		engine.listResponse = func(int) (int, string) { return http.StatusOK, listed() }
		engine.statsResponse = func(_ *http.Request, _ string, _, idCall int) (int, string) {
			if idCall == 1 {
				return http.StatusOK, valid
			}
			return http.StatusOK, invalidCurrentMemoryStatsJSON()
		}
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
		clock := newFakeRecorderClock()
		recorder.now = clock.Now
		if err := recorder.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		for second := uint64(1); second <= 5; second++ {
			if err := recorder.sample(context.Background(), second, clock.Now()); err != nil {
				t.Fatalf("sample %d: %v", second, err)
			}
			clock.Advance(3 * time.Second)
		}
		engine.listResponse = func(int) (int, string) { return http.StatusOK, "[]" }
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("grace expiry is strict", func(t *testing.T) {
		engine := newFakeEngine(t)
		engine.listResponse = func(int) (int, string) { return http.StatusOK, listed() }
		engine.statsResponse = func(_ *http.Request, _ string, _, idCall int) (int, string) {
			if idCall == 1 {
				return http.StatusOK, valid
			}
			return http.StatusOK, invalidCurrentMemoryStatsJSON()
		}
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
		clock := newFakeRecorderClock()
		recorder.now = clock.Now
		if err := recorder.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := recorder.sample(context.Background(), 1, clock.Now()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(transientValidationGrace)
		if err := recorder.sample(context.Background(), 2, clock.Now()); err == nil || !strings.Contains(err.Error(), "required current memory fields are absent") {
			t.Fatalf("expired validation error = %v", err)
		}
		engine.listResponse = func(int) (int, string) { return http.StatusOK, "[]" }
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("valid and absent samples reset state", func(t *testing.T) {
		engine := newFakeEngine(t)
		var listPhase atomic.Int32
		engine.listResponse = func(int) (int, string) {
			if listPhase.Load() == 1 {
				return http.StatusOK, "[]"
			}
			return http.StatusOK, listed()
		}
		engine.statsResponse = func(_ *http.Request, _ string, _, idCall int) (int, string) {
			if idCall == 1 || idCall == 3 {
				return http.StatusOK, valid
			}
			return http.StatusOK, invalidCurrentMemoryStatsJSON()
		}
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
		clock := newFakeRecorderClock()
		recorder.now = clock.Now
		if err := recorder.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := recorder.sample(context.Background(), 1, clock.Now()); err != nil {
			t.Fatal(err)
		}
		clock.Advance(transientValidationGrace - time.Second)
		if err := recorder.sample(context.Background(), 2, clock.Now()); err != nil {
			t.Fatalf("valid reset: %v", err)
		}
		if err := recorder.sample(context.Background(), 3, clock.Now()); err != nil {
			t.Fatalf("new deferred period after valid: %v", err)
		}
		listPhase.Store(1)
		if err := recorder.sample(context.Background(), 4, clock.Now()); err != nil {
			t.Fatalf("absence reset: %v", err)
		}
		listPhase.Store(0)
		if err := recorder.sample(context.Background(), 5, clock.Now()); err == nil || !strings.Contains(err.Error(), "required current memory fields are absent") {
			t.Fatalf("first invalid after absence = %v", err)
		}
		listPhase.Store(1)
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("first-ever invalid is strict", func(t *testing.T) {
		engine := newFakeEngine(t)
		engine.listResponse = func(int) (int, string) { return http.StatusOK, listed() }
		engine.statsResponse = func(_ *http.Request, _ string, _, _ int) (int, string) {
			return http.StatusOK, invalidCurrentMemoryStatsJSON()
		}
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
		if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "required current memory fields are absent") {
			t.Fatalf("Start error = %v", err)
		}
		listCalls, statsCalls := engine.counts()
		if listCalls != 1 || statsCalls != 1 {
			t.Fatalf("Engine calls = list %d, stats %d", listCalls, statsCalls)
		}
	})
}

func TestRecorderKeepsDeferredValidationIndependentByContainerID(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) {
		containers := []listedContainer{
			listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind),
			listedFixture(harnessID, "harness", "run-1", "fix-git", harnessKind),
		}
		content, _ := json.Marshal(containers)
		return http.StatusOK, string(content)
	}
	var recoverHarness atomic.Bool
	engine.statsResponse = func(_ *http.Request, id string, _, idCall int) (int, string) {
		if idCall == 1 || recoverHarness.Load() && id == harnessID {
			return http.StatusOK, validStatsJSON(200, 100, 2000, 1000, 1, 100, 200)
		}
		return http.StatusOK, invalidCurrentMemoryStatsJSON()
	}
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
	clock := newFakeRecorderClock()
	recorder.now = clock.Now
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recorder.sample(context.Background(), 1, clock.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(transientValidationGrace)
	recoverHarness.Store(true)
	if err := recorder.sample(context.Background(), 2, clock.Now()); err == nil ||
		!strings.Contains(err.Error(), taskContainerID) || !strings.Contains(err.Error(), "required current memory fields are absent") {
		t.Fatalf("expired task-container validation error = %v", err)
	}
	engine.listResponse = func(int) (int, string) { return http.StatusOK, "[]" }
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderReconcilesPendingValidationDuringStop(t *testing.T) {
	listed := func() string {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return string(content)
	}
	valid := validStatsJSON(200, 100, 2000, 1000, 1, 100, 200)
	setup := func(t *testing.T) (*Recorder, *fakeEngine, *atomic.Int32) {
		t.Helper()
		engine := newFakeEngine(t)
		phase := &atomic.Int32{}
		engine.listResponse = func(int) (int, string) {
			switch phase.Load() {
			case 1:
				return http.StatusOK, "[]"
			case 2:
				return http.StatusInternalServerError, `{"message":"lookup failed"}`
			default:
				return http.StatusOK, listed()
			}
		}
		engine.statsResponse = func(_ *http.Request, _ string, _, idCall int) (int, string) {
			if idCall == 1 {
				return http.StatusOK, valid
			}
			return http.StatusOK, invalidCurrentMemoryStatsJSON()
		}
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
		if err := recorder.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := recorder.sample(context.Background(), 1, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		return recorder, engine, phase
	}
	t.Run("absent pending ID succeeds", func(t *testing.T) {
		recorder, engine, phase := setup(t)
		phase.Store(1)
		reports, err := recorder.Stop(context.Background())
		if err != nil || reports["fix-git"].Status != core.StatusSucceeded {
			t.Fatalf("Stop = %+v, %v", reports, err)
		}
		if engine.idleCloseCount() != 2 {
			t.Fatalf("idle cleanup count = %d, want 2", engine.idleCloseCount())
		}
	})
	t.Run("present pending ID is strict and retryable", func(t *testing.T) {
		recorder, engine, phase := setup(t)
		if _, err := recorder.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "required current memory fields are absent") {
			t.Fatalf("present Stop error = %v", err)
		}
		if engine.idleCloseCount() != 2 {
			t.Fatalf("idle cleanup count after failed Stop = %d, want 2", engine.idleCloseCount())
		}
		phase.Store(1)
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatalf("Stop retry after disappearance: %v", err)
		}
		if engine.idleCloseCount() != 4 {
			t.Fatalf("idle cleanup count after retry = %d, want 4", engine.idleCloseCount())
		}
	})
	t.Run("discovery failure joins validation and remains retryable", func(t *testing.T) {
		recorder, engine, phase := setup(t)
		phase.Store(2)
		if _, err := recorder.Stop(context.Background()); err == nil ||
			!strings.Contains(err.Error(), "required current memory fields are absent") ||
			!strings.Contains(err.Error(), "reconcile pending monitor validation") || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("discovery Stop error = %v", err)
		}
		if engine.idleCloseCount() != 2 {
			t.Fatalf("idle cleanup count after discovery failure = %d, want 2", engine.idleCloseCount())
		}
		phase.Store(1)
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatalf("Stop retry after discovery recovery: %v", err)
		}
		if engine.idleCloseCount() != 4 {
			t.Fatalf("idle cleanup count after discovery retry = %d, want 4", engine.idleCloseCount())
		}
	})
}

func TestRecorderAcceptsDockerOneShotStatsWithoutPreCPUBaseline(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return http.StatusOK, string(content)
	}
	engine.statsResponse = func(_ *http.Request, _ string, _, _ int) (int, string) {
		return http.StatusOK, dockerOneShotStatsJSON()
	}
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Second)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := reports["fix-git"]
	if report.Status != core.StatusSucceeded || report.SampleCount != 1 {
		t.Fatalf("report = %+v", report)
	}
	samples := readSamplesStrict(t, report.LogPaths[0])
	if len(samples) != 1 || samples[0].CPUPercent != 0 || samples[0].MemoryBytes != 73400320 || samples[0].MemoryLimitBytes != 1073741824 {
		t.Fatalf("one-shot sample = %+v", samples)
	}
}

func TestValidateStatsKeepsCurrentCPUAndMemoryStrictWithoutBaseline(t *testing.T) {
	tests := []struct {
		name    string
		content string
		message string
	}{
		{
			name:    "missing current CPU total",
			content: `{"cpu_stats":{"cpu_usage":{},"system_cpu_usage":10},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
			message: "required current CPU fields are absent",
		},
		{
			name:    "missing current system CPU",
			content: `{"cpu_stats":{"cpu_usage":{"total_usage":10}},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
			message: "required current CPU fields are absent",
		},
		{
			name:    "missing current memory usage",
			content: `{"cpu_stats":{"cpu_usage":{"total_usage":10},"system_cpu_usage":20},"precpu_stats":{},"memory_stats":{"limit":2}}`,
			message: "required current memory fields are absent",
		},
		{
			name:    "missing current online CPUs and per-CPU fallback",
			content: `{"cpu_stats":{"cpu_usage":{"total_usage":10},"system_cpu_usage":20},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
			message: "online CPU count 0 is outside the bound",
		},
		{
			name:    "zero current online CPUs and empty per-CPU fallback",
			content: `{"cpu_stats":{"cpu_usage":{"total_usage":10,"percpu_usage":[]},"system_cpu_usage":20,"online_cpus":0},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
			message: "online CPU count 0 is outside the bound",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var document statsDocument
			if err := json.Unmarshal([]byte(test.content), &document); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := validateStats(document); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("validateStats error = %v", err)
			}
		})
	}
}

func TestValidateStatsNormalizesAbsentOrZeroPreCPUBaseline(t *testing.T) {
	tests := []string{
		`{"cpu_stats":{"cpu_usage":{"total_usage":10},"system_cpu_usage":20,"online_cpus":2},"memory_stats":{"usage":1,"limit":2}}`,
		`{"cpu_stats":{"cpu_usage":{"total_usage":10},"system_cpu_usage":20,"online_cpus":2},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
		`{"cpu_stats":{"cpu_usage":{"total_usage":10},"system_cpu_usage":20,"online_cpus":2},"precpu_stats":{"cpu_usage":{"total_usage":0},"system_cpu_usage":0},"memory_stats":{"usage":1,"limit":2}}`,
		`{"cpu_stats":{"cpu_usage":{"total_usage":10,"percpu_usage":[10]},"system_cpu_usage":20},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
		`{"cpu_stats":{"cpu_usage":{"total_usage":10,"percpu_usage":[10]},"system_cpu_usage":20,"online_cpus":0},"precpu_stats":{},"memory_stats":{"usage":1,"limit":2}}`,
	}
	for index, content := range tests {
		var document statsDocument
		if err := json.Unmarshal([]byte(content), &document); err != nil {
			t.Fatal(err)
		}
		cpu, memory, limit, err := validateStats(document)
		if err != nil || cpu != 0 || memory != 1 || limit != 2 {
			t.Fatalf("case %d = cpu %v, memory %d/%d, error %v", index, cpu, memory, limit, err)
		}
	}
}

func TestRecorderTurnsBackgroundMalformedStatsIntoObserverFailure(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return http.StatusOK, string(content)
	}
	engine.statsResponse = func(_ *http.Request, _ string, _, idCall int) (int, string) {
		if idCall > 1 {
			return http.StatusOK, `{"cpu_stats":{"cpu_usage":{"total_usage":-1}}}`
		}
		return http.StatusOK, validStatsJSON(200, 100, 2000, 1000, 1, 100, 200)
	}
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 10*time.Millisecond)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		_, stats := engine.counts()
		return stats >= 2
	})
	select {
	case <-recorder.sampleDone:
	case <-time.After(time.Second):
		t.Fatal("background sampler did not stop after malformed stats")
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := reports["fix-git"]
	if report.Status != core.StatusFailed || !strings.Contains(report.Error, "decode Docker Engine response") || report.SampleCount != 1 {
		t.Fatalf("report = %+v", report)
	}
	index := readIndexStrict(t, report.LogPaths[1])
	if index.Status != core.StatusFailed || index.Error != report.Error {
		t.Fatalf("index = %+v", index)
	}
}

func TestRecorderFailedStartRollsBackAndCanRetry(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return http.StatusOK, string(content)
	}
	var fail atomic.Bool
	fail.Store(true)
	engine.statsResponse = func(_ *http.Request, _ string, _, _ int) (int, string) {
		if fail.Load() {
			return http.StatusInternalServerError, `{"message":"broken"}`
		}
		return http.StatusOK, validStatsJSON(200, 100, 2000, 1000, 1, 100, 200)
	}
	outputDir := filepath.Join(t.TempDir(), "run")
	recorder := newTestRecorder(t, engine, outputDir, 20*time.Millisecond)
	if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("Start error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "monitor", "fix-git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial task directory remains: %v", err)
	}
	if engine.idleCloseCount() != 1 {
		t.Fatalf("idle connection cleanup count after failed Start = %d, want 1", engine.idleCloseCount())
	}
	if _, err := recorder.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("Stop after failed Start error = %v", err)
	}
	if engine.idleCloseCount() != 2 {
		t.Fatalf("idle connection cleanup count after not-started Stop = %d, want 2", engine.idleCloseCount())
	}
	fail.Store(false)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareArtifactsJoinsRollbackCloseAndRemoveFailures(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "run")
	monitorRoot := filepath.Join(outputDir, "monitor")
	if err := os.MkdirAll(filepath.Join(monitorRoot, "task-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(monitorRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("injected rollback close failure")
	removeFailure := errors.New("injected rollback remove failure")
	operations := defaultArtifactOperations()
	operations.closeFile = func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return closeFailure
	}
	operations.remove = func(path string) error {
		err := os.Remove(path)
		if strings.HasSuffix(path, filepath.Join("task-a", "resources.jsonl")) && err == nil {
			return removeFailure
		}
		return err
	}
	artifacts, root, created, err := prepareArtifacts(outputDir, []string{"task-a", "task-b"}, operations)
	if err == nil || !strings.Contains(err.Error(), "create monitor task directory for task-b") {
		t.Fatalf("prepareArtifacts error = %v", err)
	}
	if !errors.Is(err, closeFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("rollback errors were not joined: %v", err)
	}
	if artifacts != nil || root != "" || created {
		t.Fatalf("failed preparation returned artifacts=%v root=%q created=%v", artifacts, root, created)
	}
	if _, statErr := os.Stat(filepath.Join(monitorRoot, "task-a")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("successfully removable partial task directory remains: %v", statErr)
	}
}

func TestRecorderConcurrentStopCachesExactReports(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) { return http.StatusOK, listedJSON("run-1", false) }
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 20*time.Millisecond)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const callers = 12
	results := make(chan map[string]core.ObserverResult, callers)
	errorsFound := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			report, err := recorder.Stop(context.Background())
			results <- report
			errorsFound <- err
		}()
	}
	group.Wait()
	close(results)
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first map[string]core.ObserverResult
	for result := range results {
		if first == nil {
			first = result
		} else if !reflect.DeepEqual(first, result) {
			t.Fatalf("Stop results differ:\n%+v\n%+v", first, result)
		}
	}
	again, err := recorder.Stop(context.Background())
	if err != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("cached Stop = %+v, %v", again, err)
	}
}

func TestSuccessfulStopReleasesSampleStateAndKeepsCachedReport(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return http.StatusOK, string(content)
	}
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), time.Hour)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := cloneReports(first)
	mutated := first["fix-git"]
	mutated.LogPaths[0] = "caller mutation"
	first["fix-git"] = mutated
	recorder.mu.Lock()
	states := recorder.sampleStates
	recorder.mu.Unlock()
	if states != nil {
		t.Fatalf("sample state retained after successful Stop: %#v", states)
	}
	again, err := recorder.Stop(context.Background())
	if err != nil || !reflect.DeepEqual(want, again) {
		t.Fatalf("idempotent Stop = %+v, %v; want %+v", again, err, want)
	}
	againReport := again["fix-git"]
	againReport.LogPaths[0] = "second caller mutation"
	again["fix-git"] = againReport
	third, err := recorder.Stop(context.Background())
	if err != nil || !reflect.DeepEqual(want, third) {
		t.Fatalf("second cached Stop = %+v, %v; want %+v", third, err, want)
	}
}

func TestTimedOutStopWaiterDoesNotOrphanSampler(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) {
		content, _ := json.Marshal([]listedContainer{listedFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)})
		return http.StatusOK, string(content)
	}
	entered := make(chan struct{})
	var once sync.Once
	engine.statsResponse = func(request *http.Request, _ string, _, idCall int) (int, string) {
		if idCall > 1 {
			once.Do(func() { close(entered) })
			<-request.Context().Done()
			return http.StatusRequestTimeout, `{}`
		}
		return http.StatusOK, validStatsJSON(200, 100, 2000, 1000, 1, 100, 200)
	}
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 10*time.Millisecond)
	originalWriteIndex := recorder.writeIndex
	finalizeEntered := make(chan struct{})
	releaseFinalize := make(chan struct{})
	recorder.writeIndex = func(path string, index Index) error {
		close(finalizeEntered)
		<-releaseFinalize
		return originalWriteIndex(path, index)
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("background stats request did not start")
	}
	waiter, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := recorder.Stop(waiter); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed-out waiter error = %v", err)
	}
	select {
	case <-finalizeEntered:
	default:
		t.Fatal("Stop did not continue to artifact finalization")
	}
	close(releaseFinalize)
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reports["fix-git"].Status != core.StatusSucceeded {
		t.Fatalf("report = %+v", reports["fix-git"])
	}
	select {
	case <-recorder.sampleDone:
	default:
		t.Fatal("sampler was orphaned")
	}
}

func TestStopRetriesIndexFinalization(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) { return http.StatusOK, "[]" }
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 20*time.Millisecond)
	original := recorder.writeIndex
	var calls atomic.Int32
	recorder.writeIndex = func(path string, index Index) error {
		if calls.Add(1) == 1 {
			return errors.New("injected index failure")
		}
		return original(path, index)
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "injected index failure") {
		t.Fatalf("first Stop error = %v", err)
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatalf("retry Stop: %v", err)
	}
	if reports["fix-git"].Status != core.StatusSucceeded || reports["fix-git"].SampleCount != 0 {
		t.Fatalf("report = %+v", reports["fix-git"])
	}
}

func TestStopPreservesCloseErrorThenContinuesOnRetry(t *testing.T) {
	engine := newFakeEngine(t)
	engine.listResponse = func(int) (int, string) { return http.StatusOK, "[]" }
	recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 20*time.Millisecond)
	closeFailure := errors.New("injected artifact close failure")
	var closeCalls atomic.Int32
	recorder.artifactOps.closeFile = func(file *os.File) error {
		closeCalls.Add(1)
		if err := file.Close(); err != nil {
			return err
		}
		return closeFailure
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Stop(context.Background()); !errors.Is(err, closeFailure) {
		t.Fatalf("first Stop error = %v", err)
	}
	artifact := recorder.artifacts["fix-git"]
	if !artifact.closed || artifact.resources != nil {
		t.Fatalf("close failure state was not preserved: %+v", artifact)
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatalf("retry Stop: %v", err)
	}
	if reports["fix-git"].Status != core.StatusSucceeded || closeCalls.Load() != 1 {
		t.Fatalf("retry report = %+v, close calls = %d", reports["fix-git"], closeCalls.Load())
	}
}

func TestRecorderRejectsWrongLabelsAndOversizedResponses(t *testing.T) {
	t.Run("wrong labels", func(t *testing.T) {
		engine := newFakeEngine(t)
		container := listedFixture(taskContainerID, "task", "wrong-run", "fix-git", taskContainerKind)
		content, _ := json.Marshal([]listedContainer{container})
		engine.listResponse = func(int) (int, string) { return http.StatusOK, string(content) }
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 20*time.Millisecond)
		if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "wrong ARIES ownership labels") {
			t.Fatalf("Start error = %v", err)
		}
	})
	t.Run("oversized", func(t *testing.T) {
		engine := newFakeEngine(t)
		engine.listResponse = func(int) (int, string) { return http.StatusOK, strings.Repeat(" ", 70<<10) + "[]" }
		recorder := newTestRecorder(t, engine, filepath.Join(t.TempDir(), "run"), 20*time.Millisecond)
		if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "exceeds 65536 bytes") {
			t.Fatalf("Start error = %v", err)
		}
	})
}

func TestNewRejectsUnsafeAndUnboundedConfiguration(t *testing.T) {
	base := Options{RunID: "run-1", TaskIDs: []string{"fix-git"}, OutputDir: t.TempDir()}
	cases := []struct {
		name    string
		mutate  func(*Options)
		message string
	}{
		{name: "run path", mutate: func(options *Options) { options.RunID = "../run" }, message: "unsafe character"},
		{name: "task path", mutate: func(options *Options) { options.TaskIDs = []string{"task/one"} }, message: "unsafe character"},
		{name: "duplicate", mutate: func(options *Options) { options.TaskIDs = []string{"fix-git", "fix-git"} }, message: "repeated"},
		{name: "response", mutate: func(options *Options) { options.MaxResponseBytes = 2 << 30 }, message: "response byte bound"},
		{name: "samples", mutate: func(options *Options) { options.MaxSamplesPerTask = -1 }, message: "sample bound"},
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

func assertEngineRequestWhitelist(t *testing.T, requests []engineRequest) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatal("no Docker Engine requests recorded")
	}
	for _, request := range requests {
		if request.method != http.MethodGet {
			t.Fatalf("unexpected method: %+v", request)
		}
		values, err := url.ParseQuery(request.query)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case request.path == "/containers/json":
			if values.Get("all") != "false" || len(values) != 2 {
				t.Fatalf("unexpected list query: %q", request.query)
			}
			var filters map[string][]string
			if err := json.Unmarshal([]byte(values.Get("filters")), &filters); err != nil {
				t.Fatalf("filters: %v", err)
			}
			sort.Strings(filters["label"])
			want := []string{"aries.managed=true", "aries.run=run-1"}
			if !reflect.DeepEqual(filters, map[string][]string{"label": want}) {
				t.Fatalf("filters = %#v", filters)
			}
		case strings.HasPrefix(request.path, "/containers/") && strings.HasSuffix(request.path, "/stats"):
			if values.Get("one-shot") != "true" || values.Get("stream") != "false" || len(values) != 2 {
				t.Fatalf("unexpected stats query: %q", request.query)
			}
			id := strings.TrimSuffix(strings.TrimPrefix(request.path, "/containers/"), "/stats")
			if id != taskContainerID && id != harnessID {
				t.Fatalf("stats requested for ignored container %q", id)
			}
		default:
			t.Fatalf("unexpected Docker Engine endpoint %q", request.path)
		}
	}
}

func readSamplesStrict(t *testing.T, path string) []ResourceSample {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 16<<10)
	var samples []ResourceSample
	for scanner.Scan() {
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		var sample ResourceSample
		if err := decoder.Decode(&sample); err != nil {
			t.Fatalf("decode sample: %v", err)
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
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}
