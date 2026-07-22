package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/hyscale-lab/aries/pkg/core"
)

const (
	taskContainerID = "1111111111111111111111111111111111111111111111111111111111111111"
	harnessID       = "2222222222222222222222222222222222222222222222222222222222222222"
	initializerID   = "3333333333333333333333333333333333333333333333333333333333333333"
	foreignID       = "4444444444444444444444444444444444444444444444444444444444444444"
)

type fakeDocker struct {
	mu sync.Mutex

	listCalls    int
	inspectCalls int
	statsCalls   int
	closeCalls   int
	statsByID    map[string]int
	lastListed   map[string]containertypes.Summary

	listOptions  []client.ContainerListOptions
	statsOptions []client.ContainerStatsOptions

	listFn    func(context.Context, int) ([]containertypes.Summary, error)
	inspectFn func(context.Context, string, int) (containertypes.InspectResponse, error)
	statsFn   func(context.Context, string, int, int) (containertypes.StatsResponse, error)
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		statsByID:  make(map[string]int),
		lastListed: make(map[string]containertypes.Summary),
		listFn: func(context.Context, int) ([]containertypes.Summary, error) {
			return nil, nil
		},
		statsFn: func(context.Context, string, int, int) (containertypes.StatsResponse, error) {
			return statsFixture(300, 100, 2000, 1000, 2, 4096, 8192), nil
		},
	}
}

func (fake *fakeDocker) ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	fake.mu.Lock()
	fake.listCalls++
	call := fake.listCalls
	fake.listOptions = append(fake.listOptions, options)
	callback := fake.listFn
	fake.mu.Unlock()

	items, err := callback(ctx, call)
	if err == nil {
		fake.mu.Lock()
		for _, item := range items {
			fake.lastListed[item.ID] = item
		}
		fake.mu.Unlock()
	}
	return client.ContainerListResult{Items: items}, err
}

func (fake *fakeDocker) ContainerInspect(ctx context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	fake.mu.Lock()
	fake.inspectCalls++
	call := fake.inspectCalls
	callback := fake.inspectFn
	summary, ok := fake.lastListed[id]
	fake.mu.Unlock()

	var inspection containertypes.InspectResponse
	var err error
	if callback != nil {
		inspection, err = callback(ctx, id, call)
	} else if !ok {
		err = errdefs.ErrNotFound
	} else {
		inspection = inspectionFixture(summary)
	}
	return client.ContainerInspectResult{Container: inspection}, err
}

func (fake *fakeDocker) ContainerStats(ctx context.Context, id string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	fake.mu.Lock()
	fake.statsCalls++
	fake.statsByID[id]++
	call := fake.statsCalls
	idCall := fake.statsByID[id]
	fake.statsOptions = append(fake.statsOptions, options)
	callback := fake.statsFn
	fake.mu.Unlock()

	document, err := callback(ctx, id, call, idCall)
	if err != nil {
		return client.ContainerStatsResult{}, err
	}
	content, err := json.Marshal(document)
	if err != nil {
		return client.ContainerStatsResult{}, err
	}
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(content))}, nil
}

func (fake *fakeDocker) Close() error {
	fake.mu.Lock()
	fake.closeCalls++
	fake.mu.Unlock()
	return nil
}

func (fake *fakeDocker) counts() (lists, inspections, stats, closes int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.listCalls, fake.inspectCalls, fake.statsCalls, fake.closeCalls
}

func (fake *fakeDocker) options() ([]client.ContainerListOptions, []client.ContainerStatsOptions) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]client.ContainerListOptions(nil), fake.listOptions...), append([]client.ContainerStatsOptions(nil), fake.statsOptions...)
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

func summaryFixture(id, name, runID, taskID, kind string) containertypes.Summary {
	return containertypes.Summary{
		ID: id, Names: []string{"/" + name}, State: containertypes.StateRunning,
		Labels: map[string]string{
			"aries.managed": managedLabelValue,
			"aries.run":     runID,
			"aries.task":    taskID,
			"aries.kind":    kind,
		},
	}
}

func inspectionFixture(summary containertypes.Summary) containertypes.InspectResponse {
	return containertypes.InspectResponse{
		ID: summary.ID, Name: summary.Names[0],
		State:  &containertypes.State{Status: containertypes.StateRunning, Running: true},
		Config: &containertypes.Config{Labels: cloneLabels(summary.Labels)},
	}
}

func cloneLabels(labels map[string]string) map[string]string {
	clone := make(map[string]string, len(labels))
	for key, value := range labels {
		clone[key] = value
	}
	return clone
}

func listedFixtures(runID string, includeForeign bool) []containertypes.Summary {
	items := []containertypes.Summary{
		summaryFixture(taskContainerID, "aries-task", runID, "fix-git", taskContainerKind),
		summaryFixture(harnessID, "aries-openclaw", runID, "fix-git", harnessKind),
		summaryFixture(initializerID, "aries-init", runID, "fix-git", "openclaw-initializer"),
	}
	if includeForeign {
		items = append(items, summaryFixture(foreignID, "aries-foreign", runID, "other-task", taskContainerKind))
	}
	return items
}

func statsFixture(total, preTotal, system, preSystem uint64, online uint32, usage, limit uint64) containertypes.StatsResponse {
	return containertypes.StatsResponse{
		CPUStats: containertypes.CPUStats{
			CPUUsage:    containertypes.CPUUsage{TotalUsage: total, PercpuUsage: []uint64{1, 1}},
			SystemUsage: system,
			OnlineCPUs:  online,
		},
		PreCPUStats: containertypes.CPUStats{
			CPUUsage:    containertypes.CPUUsage{TotalUsage: preTotal, PercpuUsage: []uint64{1, 1}},
			SystemUsage: preSystem,
			OnlineCPUs:  online,
		},
		MemoryStats: containertypes.MemoryStats{Usage: usage, Limit: limit},
	}
}

func newTestRecorder(t *testing.T, api *fakeDocker, outputDir string, interval time.Duration) *Recorder {
	t.Helper()
	recorder, err := New(Options{
		RunID: "run-1", TaskIDs: []string{"fix-git"}, OutputDir: outputDir,
		DockerSocket: filepath.Join(outputDir, "unused-docker.sock"), Interval: interval,
		RequestTimeout: 200 * time.Millisecond, StopTimeout: time.Second,
		MaxSamplesPerTask: 1000, MaxFileBytes: 1 << 20,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder.engine = &engineClient{api: api}
	return recorder
}

func TestEngineDiscoversOnlyValidatedOwnedContainers(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		return listedFixtures("run-1", true), nil
	}
	engine := &engineClient{api: api}
	containers, err := engine.discover(context.Background(), "run-1", map[string]struct{}{"fix-git": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 2 || containers[0].ID != harnessID || containers[1].ID != taskContainerID {
		t.Fatalf("containers = %+v", containers)
	}
	lists, inspections, _, _ := api.counts()
	if lists != 1 || inspections != 2 {
		t.Fatalf("calls = list %d, inspect %d", lists, inspections)
	}
	listOptions, _ := api.options()
	wantFilters := make(client.Filters).Add("label", "aries.managed=true", "aries.run=run-1")
	if len(listOptions) != 1 || listOptions[0].All || !reflect.DeepEqual(listOptions[0].Filters, wantFilters) {
		t.Fatalf("list options = %+v", listOptions)
	}
}

func TestEngineRejectsUntrustedOrChangedContainerIdentity(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*fakeDocker, *containertypes.Summary)
		message string
	}{
		{
			name: "wrong ownership",
			mutate: func(_ *fakeDocker, item *containertypes.Summary) {
				item.Labels["aries.run"] = "other-run"
			},
			message: "wrong ARIES ownership labels",
		},
		{
			name: "invalid name",
			mutate: func(_ *fakeDocker, item *containertypes.Summary) {
				item.Names = []string{"not-absolute"}
			},
			message: "invalid identity",
		},
		{
			name: "inspect label changed",
			mutate: func(api *fakeDocker, item *containertypes.Summary) {
				api.inspectFn = func(context.Context, string, int) (containertypes.InspectResponse, error) {
					inspection := inspectionFixture(*item)
					inspection.Config.Labels["aries.task"] = "other-task"
					return inspection, nil
				}
			},
			message: `label "aries.task" differs`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeDocker()
			item := summaryFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)
			test.mutate(api, &item)
			api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
				return []containertypes.Summary{item}, nil
			}
			_, err := (&engineClient{api: api}).discover(context.Background(), "run-1", map[string]struct{}{"fix-git": {}})
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("discover error = %v", err)
			}
		})
	}
}

func TestEngineSamplesWithOneShotDockerStats(t *testing.T) {
	api := newFakeDocker()
	engine := &engineClient{api: api}
	listed := listedContainer{
		ID: taskContainerID, Name: "task",
		Labels: map[string]string{"aries.task": "fix-git", "aries.kind": taskContainerKind},
	}
	sample, err := engine.stats(context.Background(), listed)
	if err != nil {
		t.Fatal(err)
	}
	if sample.cpu != 40 || sample.memory != 4096 || sample.memLimit != 8192 || sample.name != "task" {
		t.Fatalf("sample = %+v", sample)
	}
	_, options := api.options()
	if len(options) != 1 || options[0].Stream || options[0].IncludePreviousSample {
		t.Fatalf("stats options = %+v", options)
	}
	api.statsFn = func(context.Context, string, int, int) (containertypes.StatsResponse, error) {
		return containertypes.StatsResponse{}, errdefs.ErrNotFound
	}
	if _, err := engine.stats(context.Background(), listed); !errors.Is(err, errContainerGone) {
		t.Fatalf("gone error = %v", err)
	}
}

func TestValidateStats(t *testing.T) {
	tests := []struct {
		name    string
		stats   containertypes.StatsResponse
		cpu     float64
		memory  uint64
		limit   uint64
		message string
	}{
		{name: "delta", stats: statsFixture(300, 100, 2000, 1000, 2, 4, 8), cpu: 40, memory: 4, limit: 8},
		{name: "no baseline", stats: statsFixture(300, 0, 2000, 0, 2, 4, 8), memory: 4, limit: 8},
		{name: "per CPU fallback", stats: statsFixture(300, 0, 2000, 0, 0, 4, 8), memory: 4, limit: 8},
		{name: "no CPUs", stats: containertypes.StatsResponse{}, message: "online CPU count 0"},
		{name: "counters decreased", stats: statsFixture(100, 300, 1000, 2000, 2, 4, 8), message: "CPU counters decreased"},
		{name: "missing system delta", stats: statsFixture(300, 100, 1000, 1000, 2, 4, 8), message: "no system delta"},
		{name: "memory bound", stats: statsFixture(300, 0, 2000, 0, 2, maxMemoryBytes+1, 8), message: "memory measurement exceeds"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpu, memory, limit, err := validateStats(test.stats)
			if test.message != "" {
				if err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("validateStats error = %v", err)
				}
				return
			}
			if err != nil || cpu != test.cpu || memory != test.memory || limit != test.limit {
				t.Fatalf("validateStats = %v, %d/%d, %v", cpu, memory, limit, err)
			}
		})
	}
}

func TestRecorderSamplesOwnedComponentsAndWritesPrivateArtifacts(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		return listedFixtures("run-1", true), nil
	}
	outputDir := filepath.Join(t.TempDir(), "run")
	recorder := newTestRecorder(t, api, outputDir, 15*time.Millisecond)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		lists, _, _, _ := api.counts()
		return lists >= 3
	})
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := reports["fix-git"]
	if report.Status != core.StatusSucceeded || report.Error != "" || report.SampleCount < 4 || report.Duration <= 0 {
		t.Fatalf("report = %+v", report)
	}
	samples := readSamplesStrict(t, report.LogPaths[0])
	if len(samples) != report.SampleCount {
		t.Fatalf("samples = %d, report = %d", len(samples), report.SampleCount)
	}
	components := map[string]int{}
	for index, sample := range samples {
		if sample.Sequence != uint64(index) || sample.TaskID != "fix-git" || sample.CPUPercent != 40 || sample.MemoryBytes != 4096 || sample.MemoryLimitBytes != 8192 {
			t.Fatalf("sample[%d] = %+v", index, sample)
		}
		components[sample.Component]++
	}
	if components[taskContainerKind] == 0 || components[harnessKind] == 0 || len(components) != 2 {
		t.Fatalf("components = %#v", components)
	}
	index := readIndexStrict(t, report.LogPaths[1])
	if index.Status != core.StatusSucceeded || index.SampleCount != uint64(len(samples)) || len(index.Components) != 2 {
		t.Fatalf("index = %+v", index)
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
}

func TestRecorderContinuesAfterCallerCancellationAndContainerRemoval(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		return listedFixtures("run-1", false), nil
	}
	api.statsFn = func(_ context.Context, id string, _, idCall int) (containertypes.StatsResponse, error) {
		if id == harnessID && idCall > 1 {
			return containertypes.StatsResponse{}, errdefs.ErrNotFound
		}
		return statsFixture(200, 100, 1500, 1000, 1, 1024, 2048), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), 10*time.Millisecond)
	if err := recorder.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitFor(t, time.Second, func() bool {
		lists, _, _, _ := api.counts()
		return lists >= 4
	})
	reports, err := recorder.Stop(context.Background())
	if err != nil || reports["fix-git"].Status != core.StatusSucceeded || reports["fix-git"].SampleCount < 4 {
		t.Fatalf("Stop = %+v, %v", reports, err)
	}
}

func TestRecorderBoundsTransientStatsValidation(t *testing.T) {
	api := newFakeDocker()
	listed := true
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		if !listed {
			return nil, nil
		}
		return []containertypes.Summary{summaryFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)}, nil
	}
	api.statsFn = func(_ context.Context, _ string, _, idCall int) (containertypes.StatsResponse, error) {
		if idCall == 1 {
			return statsFixture(200, 100, 2000, 1000, 1, 100, 200), nil
		}
		return containertypes.StatsResponse{}, nil
	}
	recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), time.Hour)
	clock := newFakeRecorderClock()
	recorder.now = clock.Now
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := recorder.sample(context.Background(), 1, clock.Now()); err != nil {
		t.Fatal(err)
	}
	clock.Advance(transientValidationGrace)
	if err := recorder.sample(context.Background(), 2, clock.Now()); err == nil || !strings.Contains(err.Error(), "online CPU count 0") {
		t.Fatalf("expired validation = %v", err)
	}
	listed = false
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderReportsBackgroundStatsFailure(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		return []containertypes.Summary{summaryFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)}, nil
	}
	api.statsFn = func(_ context.Context, _ string, _, idCall int) (containertypes.StatsResponse, error) {
		if idCall > 1 {
			return containertypes.StatsResponse{}, errors.New("stats unavailable")
		}
		return statsFixture(200, 100, 2000, 1000, 1, 100, 200), nil
	}
	recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), 10*time.Millisecond)
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
	report := reports["fix-git"]
	if report.Status != core.StatusFailed || !strings.Contains(report.Error, "stats unavailable") || report.SampleCount != 1 {
		t.Fatalf("report = %+v", report)
	}
}

func TestRecorderFailedStartRollsBackAndCanRetry(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		return []containertypes.Summary{summaryFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)}, nil
	}
	var fail atomic.Bool
	fail.Store(true)
	api.statsFn = func(context.Context, string, int, int) (containertypes.StatsResponse, error) {
		if fail.Load() {
			return containertypes.StatsResponse{}, errors.New("stats broken")
		}
		return statsFixture(200, 100, 2000, 1000, 1, 100, 200), nil
	}
	outputDir := filepath.Join(t.TempDir(), "run")
	recorder := newTestRecorder(t, api, outputDir, time.Hour)
	if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "stats broken") {
		t.Fatalf("Start error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "monitor", "fix-git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial task directory remains: %v", err)
	}
	_, _, _, closes := api.counts()
	if closes != 1 {
		t.Fatalf("close calls after failed Start = %d", closes)
	}
	fail.Store(false)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	if _, err := recorder.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecorderConcurrentStopCachesIndependentReports(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) { return nil, nil }
	recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), time.Hour)
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const callers = 8
	results := make(chan map[string]core.ObserverResult, callers)
	errs := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			reports, err := recorder.Stop(context.Background())
			results <- reports
			errs <- err
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
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
		t.Fatalf("cached Stop = %+v, %v; want %+v", again, err, want)
	}
}

func TestTimedOutStopWaiterDoesNotOrphanSampler(t *testing.T) {
	api := newFakeDocker()
	api.listFn = func(context.Context, int) ([]containertypes.Summary, error) {
		return []containertypes.Summary{summaryFixture(taskContainerID, "task", "run-1", "fix-git", taskContainerKind)}, nil
	}
	entered := make(chan struct{})
	var once sync.Once
	api.statsFn = func(ctx context.Context, _ string, _, idCall int) (containertypes.StatsResponse, error) {
		if idCall > 1 {
			once.Do(func() { close(entered) })
			<-ctx.Done()
			return containertypes.StatsResponse{}, ctx.Err()
		}
		return statsFixture(200, 100, 2000, 1000, 1, 100, 200), nil
	}
	recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), 10*time.Millisecond)
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
		t.Fatalf("timed-out waiter = %v", err)
	}
	select {
	case <-finalizeEntered:
	default:
		t.Fatal("Stop did not continue to finalization")
	}
	close(releaseFinalize)
	reports, err := recorder.Stop(context.Background())
	if err != nil || reports["fix-git"].Status != core.StatusSucceeded {
		t.Fatalf("Stop = %+v, %v", reports, err)
	}
	select {
	case <-recorder.sampleDone:
	default:
		t.Fatal("sampler was orphaned")
	}
}

func TestRecorderRetriesFinalization(t *testing.T) {
	t.Run("index", func(t *testing.T) {
		api := newFakeDocker()
		recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), time.Hour)
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
			t.Fatalf("first Stop = %v", err)
		}
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatalf("retry Stop = %v", err)
		}
	})
	t.Run("artifact close", func(t *testing.T) {
		api := newFakeDocker()
		recorder := newTestRecorder(t, api, filepath.Join(t.TempDir(), "run"), time.Hour)
		failure := errors.New("injected close failure")
		var calls atomic.Int32
		recorder.artifactOps.closeFile = func(file *os.File) error {
			calls.Add(1)
			if err := file.Close(); err != nil {
				return err
			}
			return failure
		}
		if err := recorder.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := recorder.Stop(context.Background()); !errors.Is(err, failure) {
			t.Fatalf("first Stop = %v", err)
		}
		if _, err := recorder.Stop(context.Background()); err != nil {
			t.Fatalf("retry Stop = %v", err)
		}
		if calls.Load() != 1 {
			t.Fatalf("close calls = %d", calls.Load())
		}
	})
}

func TestPrepareArtifactsJoinsRollbackFailures(t *testing.T) {
	outputDir := filepath.Join(t.TempDir(), "run")
	monitorRoot := filepath.Join(outputDir, "monitor")
	if err := os.MkdirAll(filepath.Join(monitorRoot, "task-b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(monitorRoot, 0o700); err != nil {
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
		if strings.HasSuffix(path, filepath.Join("task-a", "resources.jsonl")) && err == nil {
			return removeFailure
		}
		return err
	}
	_, _, _, err := prepareArtifacts(outputDir, []string{"task-a", "task-b"}, operations)
	if err == nil || !errors.Is(err, closeFailure) || !errors.Is(err, removeFailure) {
		t.Fatalf("prepareArtifacts error = %v", err)
	}
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

func readSamplesStrict(t *testing.T, path string) []ResourceSample {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	var samples []ResourceSample
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
