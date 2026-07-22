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
