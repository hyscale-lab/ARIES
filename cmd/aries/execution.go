package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const observerStopTimeout = 15 * time.Second

type experiment struct {
	Runner   *runner.Runner
	Recorder *monitor.Recorder
}

func (experiment *experiment) Run(ctx context.Context) (core.RunResult, error) {
	return runObserved(ctx, experiment.Runner.Run, experiment.Recorder.Start, experiment.Recorder.Stop, observerStopTimeout)
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
			task.Observer = cloneObserverResult(reports[task.TaskID])
			if stopErr != nil {
				task.Observer.Status = core.StatusFailed
				task.Observer.Error = errors.Join(
					observerResultError(task.Observer.Error),
					fmt.Errorf("stop observer: %w", stopErr),
				).Error()
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

func cloneObserverResult(result core.ObserverResult) core.ObserverResult {
	result.LogPaths = append([]string(nil), result.LogPaths...)
	return result
}

func observerResultError(message string) error {
	if message == "" {
		return nil
	}
	return errors.New(message)
}
