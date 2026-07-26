package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

func TestRunObservedOrdersObserverAroundCompleteRunnerAndAttachesReport(t *testing.T) {
	var events []string
	wantReport := core.ObserverResult{
		Status: core.StatusSucceeded, Duration: time.Second, SampleCount: 2,
		LogPaths: []string{"/private/resources.jsonl", "/private/index.json"},
	}
	startObserver := func(context.Context) error {
		events = append(events, "observer-start")
		return nil
	}
	stopObserver := func(ctx context.Context) (map[string]core.ObserverResult, error) {
		events = append(events, "observer-stop")
		if ctx.Err() != nil {
			t.Fatalf("stop context already canceled: %v", ctx.Err())
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("stop context has no deadline")
		}
		return map[string]core.ObserverResult{"fix-git": wantReport}, nil
	}
	result, err := runObserved(context.Background(), func(context.Context) (core.RunResult, error) {
		events = append(events, "runner-run-and-cleanup")
		return runResultForObserver("fix-git"), nil
	}, startObserver, stopObserver, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, []string{"observer-start", "runner-run-and-cleanup", "observer-stop"}) {
		t.Fatalf("events = %v", events)
	}
	if !reflect.DeepEqual(result.Tasks[0].Observer, wantReport) {
		t.Fatalf("observer result = %+v", result.Tasks[0].Observer)
	}
	wantReport.LogPaths[0] = "changed"
	if result.Tasks[0].Observer.LogPaths[0] == "changed" {
		t.Fatal("observer paths were not cloned")
	}
}

func TestRunProfileConcurrentDuplicatesPreserveDispatchOrder(t *testing.T) {
	release := make(chan struct{})
	started := make(chan taskOccurrence, 5)
	var active atomic.Int32
	var peak atomic.Int32
	run := func(_ context.Context, occurrence taskOccurrence) (core.RunResult, error) {
		current := active.Add(1)
		for current > peak.Load() && !peak.CompareAndSwap(peak.Load(), current) {
		}
		started <- occurrence
		<-release
		active.Add(-1)
		return core.RunResult{Tasks: []core.TaskResult{{TaskID: occurrence.executionID}}, Summary: core.RunSummary{Tasks: 1}}, nil
	}
	done := make(chan core.RunResult, 1)
	go func() {
		result, err := runProfile(context.Background(), "name", "run", []string{"fix-git", "fix-git", "fix-git", "fix-git", "fix-git"}, 5, 0, run)
		if err != nil {
			t.Errorf("runProfile: %v", err)
		}
		done <- result
	}()
	for range 5 {
		<-started
	}
	if peak.Load() != 5 {
		t.Fatalf("peak = %d", peak.Load())
	}
	close(release)
	result := <-done
	for index, task := range result.Tasks {
		want := fmt.Sprintf("fix-git-%03d", index+1)
		if task.TaskID != want {
			t.Fatalf("task %d = %q, want %q", index, task.TaskID, want)
		}
	}
}

func TestRunProfileGlobalIDsErrorsAndLimit(t *testing.T) {
	var active, peak atomic.Int32
	result, err := runProfile(context.Background(), "name", "run", []string{"a", "b", "a", "b"}, 2, 0,
		func(_ context.Context, occurrence taskOccurrence) (core.RunResult, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for current > peak.Load() && !peak.CompareAndSwap(peak.Load(), current) {
			}
			var err error
			if occurrence.executionID == "b-002" {
				err = errors.New("failed")
			}
			return core.RunResult{Tasks: []core.TaskResult{{TaskID: occurrence.executionID}}, Summary: core.RunSummary{Tasks: 1}}, err
		})
	if err == nil || peak.Load() > 2 {
		t.Fatalf("error=%v peak=%d", err, peak.Load())
	}
	if !strings.Contains(err.Error(), "task occurrence b-002: failed") {
		t.Fatalf("error = %v", err)
	}
	want := []string{"a-001", "b-002", "a-003", "b-004"}
	for i := range want {
		if result.Tasks[i].TaskID != want[i] {
			t.Fatalf("tasks = %+v", result.Tasks)
		}
	}
}

func TestRunProfileLoopStopsAdmissionsAndDrains(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	done := make(chan core.RunResult, 1)
	go func() {
		result, _ := runProfile(context.Background(), "name", "run", []string{"a"}, 2, 25*time.Millisecond,
			func(_ context.Context, occurrence taskOccurrence) (core.RunResult, error) {
				started <- struct{}{}
				<-release
				return core.RunResult{Tasks: []core.TaskResult{{TaskID: occurrence.executionID}}, Summary: core.RunSummary{Tasks: 1}}, nil
			})
		done <- result
	}()
	<-started
	<-started
	time.Sleep(40 * time.Millisecond)
	close(release)
	result := <-done
	if len(result.Tasks) != 2 {
		t.Fatalf("admitted %d tasks", len(result.Tasks))
	}
}

func TestRunProfileCancellationStopsAdmissionsAndWaitsForActive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := runProfile(ctx, "name", "run", []string{"a", "b", "c"}, 1, 0,
			func(context.Context, taskOccurrence) (core.RunResult, error) {
				close(started)
				<-release
				return core.RunResult{Summary: core.RunSummary{Tasks: 1}}, nil
			})
		done <- err
	}()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("returned before active occurrence drained")
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestRunProfileRepeatedOccurrencesCloseEachObservedExperimentOnce(t *testing.T) {
	var mu sync.Mutex
	closes := make(map[string]int)
	result, err := runProfile(context.Background(), "name", "run", []string{"a", "a", "a"}, 3, 0,
		func(ctx context.Context, occurrence taskOccurrence) (core.RunResult, error) {
			closeExperiment := func() error { mu.Lock(); closes[occurrence.executionID]++; mu.Unlock(); return nil }
			observed, runErr := runObserved(ctx,
				func(context.Context) (core.RunResult, error) {
					return core.RunResult{Tasks: []core.TaskResult{{TaskID: occurrence.executionID}}, Summary: core.RunSummary{Tasks: 1}}, nil
				},
				func(context.Context) error { return nil },
				func(context.Context) (map[string]core.ObserverResult, error) {
					return map[string]core.ObserverResult{occurrence.executionID: {Status: core.StatusSucceeded}}, nil
				},
				time.Second)
			return observed, errors.Join(runErr, closeExperiment())
		})
	if err != nil || len(result.Tasks) != 3 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	for _, id := range []string{"a-001", "a-002", "a-003"} {
		if closes[id] != 1 {
			t.Fatalf("close count %s = %d", id, closes[id])
		}
	}
}

func TestTaskOccurrenceIdentityBounds(t *testing.T) {
	index := uint64(999)
	occurrence, err := nextTaskOccurrence("a", &index)
	if err != nil || occurrence.executionID != "a-1000" {
		t.Fatalf("occurrence = %+v, %v", occurrence, err)
	}
	logical := strings.Repeat("a", 128)
	index = math.MaxUint64 - 1
	occurrence, err = nextTaskOccurrence(logical, &index)
	if err != nil || len(occurrence.executionID) != 149 {
		t.Fatalf("max occurrence length = %d, %v", len(occurrence.executionID), err)
	}
	if _, err := nextTaskOccurrence(logical, &index); err == nil {
		t.Fatal("overflow accepted")
	}
}

func TestRunObservedAlwaysRunsAfterObserverStartFailure(t *testing.T) {
	startErr := errors.New("start failed")
	runErr := errors.New("runner failed after evaluation")
	var events []string
	startObserver := func(context.Context) error {
		events = append(events, "start")
		return startErr
	}
	stopObserver := func(context.Context) (map[string]core.ObserverResult, error) {
		t.Fatal("Stop called after Start failed")
		return nil, nil
	}
	result, err := runObserved(context.Background(), func(context.Context) (core.RunResult, error) {
		events = append(events, "run")
		result := runResultForObserver("fix-git")
		result.Tasks[0].Evaluation.Status = core.StatusSucceeded
		return result, runErr
	}, startObserver, stopObserver, time.Second)
	if !errors.Is(err, startErr) || !errors.Is(err, runErr) {
		t.Fatalf("joined error = %v", err)
	}
	if !reflect.DeepEqual(events, []string{"start", "run"}) {
		t.Fatalf("events = %v", events)
	}
	if result.Tasks[0].Evaluation.Status != core.StatusSucceeded || result.Tasks[0].Observer.Status != core.StatusFailed || !strings.Contains(result.Tasks[0].Observer.Error, "start failed") {
		t.Fatalf("result = %+v", result.Tasks[0])
	}
}

func TestRunObservedAttachesReportsAndJoinsStopFailure(t *testing.T) {
	stopErr := errors.New("stop failed")
	wantReport := core.ObserverResult{Status: core.StatusFailed, Error: "sampling failed", SampleCount: 1}
	result, err := runObserved(context.Background(), func(context.Context) (core.RunResult, error) {
		return runResultForObserver("fix-git"), nil
	}, func(context.Context) error { return nil },
		func(context.Context) (map[string]core.ObserverResult, error) {
			return map[string]core.ObserverResult{"fix-git": wantReport}, stopErr
		},
		time.Second)
	if !errors.Is(err, stopErr) {
		t.Fatalf("error = %v", err)
	}
	if result.Tasks[0].Observer.Status != core.StatusFailed || result.Tasks[0].Observer.SampleCount != wantReport.SampleCount || !strings.Contains(result.Tasks[0].Observer.Error, "sampling failed") || !strings.Contains(result.Tasks[0].Observer.Error, "stop failed") {
		t.Fatalf("observer result = %+v", result.Tasks[0].Observer)
	}
}

func TestRunObservedUsesFreshBoundedStopContextAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), observerContextKey{}, "retained"))
	var stopCalled bool
	result, err := runObserved(ctx, func(context.Context) (core.RunResult, error) {
		cancel()
		return runResultForObserver("fix-git"), context.Canceled
	}, func(context.Context) error { return nil },
		func(stopCtx context.Context) (map[string]core.ObserverResult, error) {
			stopCalled = true
			if stopCtx.Err() != nil || stopCtx.Value(observerContextKey{}) != "retained" {
				t.Fatalf("stop context = err %v value %v", stopCtx.Err(), stopCtx.Value(observerContextKey{}))
			}
			deadline, ok := stopCtx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
				t.Fatalf("stop deadline = %v, %v", deadline, ok)
			}
			return map[string]core.ObserverResult{"fix-git": {Status: core.StatusSucceeded}}, nil
		},
		time.Second)
	if !stopCalled || !errors.Is(err, context.Canceled) || result.Tasks[0].Observer.Status != core.StatusSucceeded {
		t.Fatalf("stop=%v err=%v result=%+v", stopCalled, err, result)
	}
}

func TestRunObservedMarksMissingTaskReportWithoutChangingEvaluation(t *testing.T) {
	result, err := runObserved(context.Background(), func(context.Context) (core.RunResult, error) {
		result := runResultForObserver("fix-git")
		result.Tasks[0].Evaluation.Status = core.StatusSucceeded
		return result, nil
	}, func(context.Context) error { return nil },
		func(context.Context) (map[string]core.ObserverResult, error) {
			return map[string]core.ObserverResult{}, nil
		},
		time.Second)
	if err == nil || !strings.Contains(err.Error(), `observer report missing for task "fix-git"`) {
		t.Fatalf("error = %v", err)
	}
	if result.Tasks[0].Evaluation.Status != core.StatusSucceeded || result.Tasks[0].Observer.Status != core.StatusFailed {
		t.Fatalf("result = %+v", result.Tasks[0])
	}
}

func TestExecuteAndRecordPersistsAttachedObserverResult(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	want := core.ObserverResult{Status: core.StatusSucceeded, Duration: 2 * time.Second, SampleCount: 3, LogPaths: []string{"/private/resources.jsonl"}}
	var stdout bytes.Buffer
	err := executeAndRecord(context.Background(), func(ctx context.Context) (core.RunResult, error) {
		return runObserved(ctx, func(context.Context) (core.RunResult, error) {
			return runResultForObserver("fix-git"), nil
		}, func(context.Context) error { return nil },
			func(context.Context) (map[string]core.ObserverResult, error) {
				return map[string]core.ObserverResult{"fix-git": want}, nil
			},
			time.Second)
	}, root, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, runResultName))
	if err != nil {
		t.Fatal(err)
	}
	var persisted core.RunResult
	if err := json.Unmarshal(content, &persisted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.Tasks[0].Observer, want) {
		t.Fatalf("persisted observer = %+v", persisted.Tasks[0].Observer)
	}
}

type observerContextKey struct{}

func runResultForObserver(taskID string) core.RunResult {
	return core.RunResult{Tasks: []core.TaskResult{{
		TaskID:   taskID,
		Observer: core.ObserverResult{Status: core.StatusNotEnabled},
	}}}
}
