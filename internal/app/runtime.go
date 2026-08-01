package app

import (
	"context"
	"errors"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ModelRuntime is run-scoped infrastructure for a self-hosted model server.
// Inference remains the responsibility of model clients and harnesses.
type ModelRuntime interface {
	Start(context.Context) error
	Health(context.Context) error
	Done() <-chan struct{}
	Err() error
	Stop(context.Context) error
}

type retryableHealthError interface {
	error
	Retryable() bool
}

type PreparedBackend struct {
	Model               core.ModelConfig
	Runtime             ModelRuntime
	EffectiveGPUIndices []int
}

func waitForRuntimeHealth(ctx context.Context, runtime ModelRuntime, sleep contextSleep, observeRetry func()) error {
	if sleep == nil {
		sleep = sleepWithContext
	}
	retryCtx, cancelRetry := context.WithCancel(ctx)
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-runtime.Done():
			cancelRetry()
		case <-retryCtx.Done():
		}
	}()
	defer func() {
		cancelRetry()
		<-watcherDone
	}()
	for {
		if runtimeErr, exited := runtimeExitIfDone(runtime); exited {
			return runtimeErr
		}
		err := runtime.Health(retryCtx)
		if err == nil {
			if runtimeErr, exited := runtimeExitIfDone(runtime); exited {
				return runtimeErr
			}
			return nil
		}
		if runtimeErr, exited := runtimeExitIfDone(runtime); exited {
			return runtimeErr
		}
		var retryable retryableHealthError
		if !errors.As(err, &retryable) || !retryable.Retryable() {
			return err
		}
		if observeRetry != nil {
			observeRetry()
		}
		if err := sleep(retryCtx, managedStartupRetryDelay); err != nil {
			if runtimeErr, exited := runtimeExitIfDone(runtime); exited {
				return runtimeErr
			}
			return err
		}
	}
}

type runtimeCleanupError interface {
	error
	CleanupFailed() bool
}

func runtimeCleanupFailed(err error) bool {
	if err == nil {
		return false
	}
	var classified runtimeCleanupError
	return !errors.As(err, &classified) || classified.CleanupFailed()
}

func stopRuntime(runtime ModelRuntime, timeout time.Duration) error {
	if runtime == nil {
		return nil
	}
	if timeout <= 0 {
		timeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runtime.Stop(ctx)
}

func runtimeExitIfDone(runtime ModelRuntime) (error, bool) {
	select {
	case <-runtime.Done():
		if err := runtime.Err(); err != nil {
			return err, true
		}
		return errors.New("model runtime exited unexpectedly"), true
	default:
		return nil, false
	}
}

func awaitRuntimeCompletion[T any](runtime ModelRuntime, completed <-chan T, cancel func()) (T, error) {
	var result T
	select {
	case result = <-completed:
	case <-runtime.Done():
		cancel()
		result = <-completed
	}
	runtimeErr, _ := runtimeExitIfDone(runtime)
	return result, runtimeErr
}
