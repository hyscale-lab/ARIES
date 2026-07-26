package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const observerStopTimeout = 15 * time.Second

type experiment struct {
	runner   *runner.Runner
	recorder *monitor.Recorder
	close    func() error
}

func (experiment *experiment) Run(ctx context.Context) (core.RunResult, error) {
	result, err := runObserved(ctx, experiment.runner.Run, experiment.recorder.Start, experiment.recorder.Stop, observerStopTimeout)
	if experiment.close != nil {
		err = errors.Join(err, experiment.close())
	}
	return result, err
}

type taskOccurrence struct {
	logicalID   string
	executionID string
}

func nextTaskOccurrence(logicalID string, index *uint64) (taskOccurrence, error) {
	if *index == math.MaxUint64 {
		return taskOccurrence{}, errors.New("task occurrence index overflow")
	}
	*index++
	return taskOccurrence{logicalID: logicalID, executionID: fmt.Sprintf("%s-%03d", logicalID, *index)}, nil
}

type occurrenceRunner func(context.Context, taskOccurrence) (core.RunResult, error)

type occurrenceSlot struct {
	occurrence taskOccurrence
	result     core.RunResult
	err        error
}

func runProfile(ctx context.Context, name, runID string, taskIDs []string, concurrency int, loopDuration time.Duration, run occurrenceRunner) (core.RunResult, error) {
	started := time.Now()
	result := core.RunResult{Name: name, RunID: runID}
	if concurrency <= 0 || len(taskIDs) == 0 {
		return result, errors.New("execution requires positive concurrency and at least one task")
	}
	var deadline <-chan time.Time
	var deadlineAt time.Time
	var timer *time.Timer
	if loopDuration > 0 {
		deadlineAt = started.Add(loopDuration)
		timer = time.NewTimer(loopDuration)
		defer timer.Stop()
		deadline = timer.C
	}
	capacity := make(chan struct{}, concurrency)
	var slots []*occurrenceSlot
	var wg sync.WaitGroup
	var index uint64
	admit := func(logicalID string) bool {
		if !deadlineAt.IsZero() && !time.Now().Before(deadlineAt) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline:
			return false
		case capacity <- struct{}{}:
		}
		if ctx.Err() != nil || !deadlineAt.IsZero() && !time.Now().Before(deadlineAt) {
			<-capacity
			return false
		}
		occurrence, err := nextTaskOccurrence(logicalID, &index)
		if err != nil {
			<-capacity
			slots = append(slots, &occurrenceSlot{err: err})
			return false
		}
		slot := &occurrenceSlot{occurrence: occurrence}
		slots = append(slots, slot)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-capacity }()
			slot.result, slot.err = run(ctx, occurrence)
		}()
		return true
	}
	if loopDuration == 0 {
		for _, logicalID := range taskIDs {
			if !admit(logicalID) {
				break
			}
		}
	} else {
	admissions:
		for {
			for _, logicalID := range taskIDs {
				if !admit(logicalID) {
					break admissions
				}
			}
		}
	}
	wg.Wait()
	var joined []error
	for _, slot := range slots {
		result.Tasks = append(result.Tasks, slot.result.Tasks...)
		addSummary(&result.Summary, slot.result.Summary)
		if slot.err != nil {
			if slot.occurrence.executionID != "" {
				joined = append(joined, fmt.Errorf("task occurrence %s: %w", slot.occurrence.executionID, slot.err))
			} else {
				joined = append(joined, slot.err)
			}
		}
	}
	if ctx.Err() != nil {
		joined = append(joined, ctx.Err())
	}
	result.Duration = time.Since(started)
	return result, errors.Join(joined...)
}

func addSummary(total *core.RunSummary, next core.RunSummary) {
	total.Tasks += next.Tasks
	total.HarnessSucceeded += next.HarnessSucceeded
	total.HarnessFailed += next.HarnessFailed
	total.EvaluationsRun += next.EvaluationsRun
	total.EvaluationsSucceeded += next.EvaluationsSucceeded
	total.EvaluationsFailed += next.EvaluationsFailed
	total.EvaluationsBlocked += next.EvaluationsBlocked
	total.CleanupFailed += next.CleanupFailed
}

func runObserved(
	ctx context.Context,
	execute func(context.Context) (core.RunResult, error),
	startObserver func(context.Context) error,
	stopObserver func(context.Context) (map[string]core.ObserverResult, error),
	stopTimeout time.Duration,
) (core.RunResult, error) {
	startErr := startObserver(ctx)
	result, runErr := execute(ctx)

	var reports map[string]core.ObserverResult
	var stopErr error
	if startErr == nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
		reports, stopErr = stopObserver(stopCtx)
		cancel()
	}

	var reportErrors []error
	for index := range result.Tasks {
		task := &result.Tasks[index]
		switch {
		case startErr != nil:
			task.Observer = failedObserverResult(fmt.Sprintf("start observer: %v", startErr))
		case reports[task.TaskID].Status != "":
			task.Observer = reports[task.TaskID]
			task.Observer.LogPaths = append([]string(nil), task.Observer.LogPaths...)
			if stopErr != nil {
				task.Observer.Status = core.StatusFailed
				stopMessage := fmt.Errorf("stop observer: %w", stopErr)
				if task.Observer.Error != "" {
					stopMessage = errors.Join(errors.New(task.Observer.Error), stopMessage)
				}
				task.Observer.Error = stopMessage.Error()
			}
		default:
			missingErr := fmt.Errorf("observer report missing for task %q", task.TaskID)
			message := error(missingErr)
			if stopErr != nil {
				message = errors.Join(fmt.Errorf("stop observer: %w", stopErr), missingErr)
			}
			task.Observer = failedObserverResult(message.Error())
			reportErrors = append(reportErrors, missingErr)
		}
	}

	if startErr != nil {
		startErr = fmt.Errorf("start observer: %w", startErr)
	}
	if stopErr != nil {
		stopErr = fmt.Errorf("stop observer: %w", stopErr)
	}
	return result, errors.Join(runErr, startErr, stopErr, errors.Join(reportErrors...))
}

func failedObserverResult(message string) core.ObserverResult {
	return core.ObserverResult{Status: core.StatusFailed, Error: message}
}
