package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	containertypes "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

const (
	resourceSandboxID = "1111111111111111111111111111111111111111111111111111111111111111"
	resourceHarnessID = "2222222222222222222222222222222222222222222222222222222222222222"
)

type fakeResourceAPI struct {
	mu           sync.Mutex
	items        []containertypes.Summary
	inspections  map[string]containertypes.InspectResponse
	stats        map[string]containertypes.StatsResponse
	inspectError map[string]error
	statsError   map[string]error
	listOptions  []client.ContainerListOptions
	statsOptions []client.ContainerStatsOptions
	closes       int
}

func (fake *fakeResourceAPI) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.listOptions = append(fake.listOptions, options)
	return client.ContainerListResult{Items: append([]containertypes.Summary(nil), fake.items...)}, nil
}

func (fake *fakeResourceAPI) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if err := fake.inspectError[id]; err != nil {
		return client.ContainerInspectResult{}, err
	}
	return client.ContainerInspectResult{Container: fake.inspections[id]}, nil
}

func (fake *fakeResourceAPI) ContainerStats(_ context.Context, id string, options client.ContainerStatsOptions) (client.ContainerStatsResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.statsOptions = append(fake.statsOptions, options)
	if err := fake.statsError[id]; err != nil {
		return client.ContainerStatsResult{}, err
	}
	content, err := json.Marshal(fake.stats[id])
	if err != nil {
		return client.ContainerStatsResult{}, err
	}
	return client.ContainerStatsResult{Body: io.NopCloser(bytes.NewReader(content))}, nil
}

func (fake *fakeResourceAPI) Close() error {
	fake.mu.Lock()
	fake.closes++
	fake.mu.Unlock()
	return nil
}

func resourceSummary(id, name, kind string) containertypes.Summary {
	component := map[string]string{"task-container": "sandbox", "openclaw-harness": "harness"}[kind]
	return containertypes.Summary{
		ID: id, Names: []string{"/" + name}, State: containertypes.StateRunning,
		Labels: map[string]string{"aries.managed": "true", "aries.run": "run-1", "aries.task": "fix-git", "aries.kind": kind, "aries.component": component},
	}
}

func resourceInspection(summary containertypes.Summary) containertypes.InspectResponse {
	labels := make(map[string]string, len(summary.Labels))
	for key, value := range summary.Labels {
		labels[key] = value
	}
	return containertypes.InspectResponse{
		ID: summary.ID, Name: summary.Names[0], Config: &containertypes.Config{Labels: labels},
		State: &containertypes.State{Status: containertypes.StateRunning, Running: true},
	}
}

func resourceStats(observed time.Time, cpu, memory, limit uint64) containertypes.StatsResponse {
	return containertypes.StatsResponse{
		Read: observed, CPUStats: containertypes.CPUStats{CPUUsage: containertypes.CPUUsage{TotalUsage: cpu}},
		MemoryStats: containertypes.MemoryStats{Usage: memory, Limit: limit},
	}
}

func newFakeResourceSource() (*DockerResourceSource, *fakeResourceAPI) {
	sandbox := resourceSummary(resourceSandboxID, "aries-task", "task-container")
	harness := resourceSummary(resourceHarnessID, "aries-openclaw", "openclaw-harness")
	ignored := resourceSummary("3333333333333333333333333333333333333333333333333333333333333333", "aries-network-helper", "task-network")
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	fake := &fakeResourceAPI{
		items: []containertypes.Summary{sandbox, ignored, harness},
		inspections: map[string]containertypes.InspectResponse{
			sandbox.ID: resourceInspection(sandbox), harness.ID: resourceInspection(harness), ignored.ID: resourceInspection(ignored),
		},
		stats: map[string]containertypes.StatsResponse{
			sandbox.ID: resourceStats(now, 123, 4096, 8192), harness.ID: resourceStats(now.Add(time.Millisecond), 456, 2048, 8192),
		},
		inspectError: make(map[string]error), statsError: make(map[string]error),
	}
	return &DockerResourceSource{api: fake, runID: "run-1", tasks: map[string]struct{}{"fix-git": {}}}, fake
}

func TestDockerResourceSourceReturnsRawPortableReadings(t *testing.T) {
	source, fake := newFakeResourceSource()
	readings, err := source.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 2 || readings[0].Component != "harness" || readings[1].Component != "sandbox" {
		t.Fatalf("readings = %+v", readings)
	}
	if readings[0].CPUUsageNanoseconds != 456 || readings[1].CPUUsageNanoseconds != 123 || readings[1].MemoryUsageBytes != 4096 {
		t.Fatalf("raw readings = %+v", readings)
	}
	wantFilters := make(client.Filters).Add("label", "aries.managed=true", "aries.run=run-1")
	if len(fake.listOptions) != 1 || !reflect.DeepEqual(fake.listOptions[0].Filters, wantFilters) {
		t.Fatalf("list options = %+v", fake.listOptions)
	}
	if len(fake.statsOptions) != 2 {
		t.Fatalf("stats options = %+v", fake.statsOptions)
	}
	for _, options := range fake.statsOptions {
		if options.Stream || options.IncludePreviousSample {
			t.Fatalf("stats must be immediate one-shot: %+v", options)
		}
	}
	if err := source.Close(); err != nil || fake.closes != 1 {
		t.Fatalf("Close = %v, calls %d", err, fake.closes)
	}
}

func TestDockerResourceSourceSelectsGenericComponentNotConcreteKind(t *testing.T) {
	source, fake := newFakeResourceSource()
	for index := range fake.items {
		if fake.items[index].ID == resourceHarnessID {
			fake.items[index].Labels["aries.kind"] = "future-harness-runtime"
		}
	}
	inspection := fake.inspections[resourceHarnessID]
	inspection.Config.Labels["aries.kind"] = "future-harness-runtime"
	fake.inspections[resourceHarnessID] = inspection
	readings, err := source.Sample(context.Background())
	if err != nil || len(readings) != 2 || readings[0].Component != "harness" {
		t.Fatalf("generic component readings = %+v, %v", readings, err)
	}
}

func TestDockerResourceSourceIgnoresLifecycleRacesAndEmptyTail(t *testing.T) {
	t.Run("stopped between list and inspect", func(t *testing.T) {
		source, fake := newFakeResourceSource()
		inspection := fake.inspections[resourceSandboxID]
		inspection.State.Running = false
		inspection.State.Status = containertypes.StateExited
		fake.inspections[resourceSandboxID] = inspection
		readings, err := source.Sample(context.Background())
		if err != nil || len(readings) != 1 || readings[0].RuntimeID != resourceHarnessID {
			t.Fatalf("readings = %+v, %v", readings, err)
		}
	})
	t.Run("removed before stats", func(t *testing.T) {
		source, fake := newFakeResourceSource()
		fake.statsError[resourceSandboxID] = errdefs.ErrNotFound
		readings, err := source.Sample(context.Background())
		if err != nil || len(readings) != 1 || readings[0].RuntimeID != resourceHarnessID {
			t.Fatalf("readings = %+v, %v", readings, err)
		}
	})
	t.Run("zero tail", func(t *testing.T) {
		source, fake := newFakeResourceSource()
		fake.stats[resourceSandboxID] = resourceStats(time.Now(), 999, 0, 0)
		readings, err := source.Sample(context.Background())
		if err != nil || len(readings) != 1 || readings[0].RuntimeID != resourceHarnessID {
			t.Fatalf("readings = %+v, %v", readings, err)
		}
	})
}

func TestDockerResourceSourceRejectsChangedIdentityAndInvalidStats(t *testing.T) {
	t.Run("changed labels", func(t *testing.T) {
		source, fake := newFakeResourceSource()
		inspection := fake.inspections[resourceSandboxID]
		inspection.Config.Labels["aries.task"] = "other"
		fake.inspections[resourceSandboxID] = inspection
		if _, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), `label "aries.task" differs`) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing observation time", func(t *testing.T) {
		source, fake := newFakeResourceSource()
		fake.stats[resourceSandboxID] = resourceStats(time.Time{}, 1, 1, 2)
		if _, err := source.Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "omit observation time") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("stats error", func(t *testing.T) {
		source, fake := newFakeResourceSource()
		want := errors.New("stats failed")
		fake.statsError[resourceSandboxID] = want
		if _, err := source.Sample(context.Background()); !errors.Is(err, want) {
			t.Fatalf("error = %v", err)
		}
	})
}
