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
	Model   core.ModelConfig
	Runtime ModelRuntime
}

func waitForRuntimeHealth(ctx context.Context, runtime ModelRuntime, sleep contextSleep) error {
	if sleep == nil {
		sleep = sleepWithContext
	}
	for {
		err := runtime.Health(ctx)
		if err == nil {
			return nil
		}
		var retryable retryableHealthError
		if !errors.As(err, &retryable) || !retryable.Retryable() {
			return err
		}
		select {
		case <-runtime.Done():
			if runtimeErr := runtime.Err(); runtimeErr != nil {
				return runtimeErr
			}
			return errors.New("model runtime exited during startup")
		default:
		}
		if err := sleep(ctx, managedStartupRetryDelay); err != nil {
			return err
		}
	}
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
