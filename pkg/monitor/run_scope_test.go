package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestRunRecorderMeasuresBridgeOnceWithoutTaskIdentity(t *testing.T) {
	observed := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{sample: func(_ context.Context, call int) ([]core.ResourceReading, error) {
		return []core.ResourceReading{{
			Scope: "run", RunID: "run-1", Component: "bridge", RuntimeID: "shared-bridge", RuntimeName: "aries-bridge-one",
			ObservedAt: observed.Add(time.Duration(call-1) * 2 * time.Second), CPUUsageNanoseconds: uint64(call-1) * 500_000_000,
			MemoryUsageBytes: 4096, MemoryLimitBytes: 8192,
		}}, nil
	}}
	output := filepath.Join(t.TempDir(), "run", "infrastructure", "bridge")
	recorder, err := New(Options{Scope: "run", RunID: "run-1", OutputDir: output, Source: source, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = recorder.Stop(context.Background()) })
	if err := recorder.sample(context.Background(), 1, observed.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report, ok := reports[""]
	if !ok || len(reports) != 1 || report.Status != core.StatusSucceeded || report.SampleCount != 2 {
		t.Fatalf("run reports = %#v", reports)
	}
	if got := filepath.Dir(report.LogPaths[0]); got != filepath.Join(output, "monitor") {
		t.Fatalf("run artifact path = %q", got)
	}
	samples := readSamplesStrict(t, report.LogPaths[0])
	if len(samples) != 2 || samples[0].CPUPercent != 0 || samples[1].CPUPercent != 25 {
		t.Fatalf("CPU samples = %#v", samples)
	}
	for i, sample := range samples {
		if sample.Scope != "run" || sample.RunID != "run-1" || sample.TaskID != "" || sample.Component != "bridge" || sample.RuntimeID != "shared-bridge" || sample.Sequence != uint64(i) {
			t.Fatalf("sample %d = %#v", i, sample)
		}
	}
	index := readIndexStrict(t, report.LogPaths[1])
	if index.SchemaVersion != 4 || index.Scope != "run" || index.RunID != "run-1" || index.TaskID != "" || len(index.Components) != 1 || index.Components[0].SampleCount != 2 {
		t.Fatalf("run index = %#v", index)
	}
	for _, path := range report.LogPaths {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), `"task_id"`) {
			t.Fatalf("run artifact fabricates task metadata: %s", content)
		}
	}
	calls, closes := source.counts()
	if calls != 2 || closes != 1 {
		t.Fatalf("source samples=%d closes=%d", calls, closes)
	}
}

func TestRunRecorderRejectsWrongResourceScopeAndIdentity(t *testing.T) {
	valid := core.ResourceReading{Scope: "run", RunID: "run-1", Component: "bridge", RuntimeID: "bridge", RuntimeName: "aries-bridge", ObservedAt: time.Now()}
	for name, mutate := range map[string]func(*core.ResourceReading){
		"task reading":    func(r *core.ResourceReading) { r.Scope = "task" },
		"absent scope":    func(r *core.ResourceReading) { r.Scope = "" },
		"other run":       func(r *core.ResourceReading) { r.RunID = "other-run" },
		"absent run":      func(r *core.ResourceReading) { r.RunID = "" },
		"fabricated task": func(r *core.ResourceReading) { r.TaskID = "bridge" },
	} {
		t.Run(name, func(t *testing.T) {
			reading := valid
			mutate(&reading)
			source := &fakeSource{sample: func(context.Context, int) ([]core.ResourceReading, error) {
				return []core.ResourceReading{reading}, nil
			}}
			output := filepath.Join(t.TempDir(), "run")
			recorder, err := New(Options{Scope: "run", RunID: "run-1", OutputDir: output, Source: source})
			if err != nil {
				t.Fatal(err)
			}
			if err := recorder.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "sampled unexpected") {
				t.Fatalf("mismatched resource result: %v", err)
			}
			if _, err := os.Stat(filepath.Join(output, "monitor")); !os.IsNotExist(err) {
				t.Fatalf("failed start left artifacts: %v", err)
			}
			_, closes := source.counts()
			if closes != 1 {
				t.Fatalf("source closes = %d", closes)
			}
		})
	}
}

func TestRunRecorderRejectsTaskSelectionAndPreservesUnsupported(t *testing.T) {
	if _, err := New(Options{Scope: "run", RunID: "run-1", TaskIDs: []string{"bridge"}, OutputDir: t.TempDir(), Source: &fakeSource{}}); err == nil {
		t.Fatal("run recorder accepted fake task selection")
	}
	source := &fakeSource{sample: func(context.Context, int) ([]core.ResourceReading, error) { return nil, ErrUnsupported }}
	recorder, err := New(Options{Scope: "run", RunID: "run-1", OutputDir: filepath.Join(t.TempDir(), "run"), Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	reports, err := recorder.Stop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	report := reports[""]
	if report.Status != core.StatusUnsupported || report.SampleCount != 0 {
		t.Fatalf("unsupported run report = %#v", report)
	}
	content, err := os.ReadFile(report.LogPaths[1])
	if err != nil {
		t.Fatal(err)
	}
	var index map[string]any
	if err := json.Unmarshal(content, &index); err != nil {
		t.Fatal(err)
	}
	if index["scope"] != "run" || index["status"] != core.StatusUnsupported || index["task_id"] != nil {
		t.Fatalf("unsupported index = %#v", index)
	}
}
