// Package runtime supplies shared harness runtime ownership mechanics.
package runtime

import (
	"context"
	"sync"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

// StopState coordinates stop attempts under the manager's existing mutex.
// Begin, Running, Finish, and Reset must be called while holding that mutex.
// The manager retains its concrete session and invokes its own cleanup explicitly.
type StopState struct {
	attempt *StopAttempt
	running bool
}

// StopAttempt retains one attempt's outcome even if a later retry starts.
type StopAttempt struct {
	done chan struct{}
	err  error
}

// Running reports whether cleanup is already in progress.
func (state *StopState) Running() bool { return state.running }

// Begin returns an attempt and whether the caller owns its cleanup.
// owned remains true after unconfirmed removal, allowing a subsequent retry.
func (state *StopState) Begin(owned bool) (*StopAttempt, bool) {
	if state.running {
		return state.attempt, false
	}
	if !owned {
		if state.attempt == nil {
			state.Reset(nil)
		}
		return state.attempt, false
	}
	state.attempt = &StopAttempt{done: make(chan struct{})}
	state.running = true
	return state.attempt, true
}

// Finish publishes the cleanup result to all waiters on this attempt.
func (state *StopState) Finish(err error) {
	state.attempt.err = err
	state.running = false
	close(state.attempt.done)
}

// Reset records startup/rollback state when no stop attempt is running.
func (state *StopState) Reset(err error) {
	state.attempt = &StopAttempt{done: make(chan struct{}), err: err}
	close(state.attempt.done)
}

// Wait respects the waiter's context without canceling the owning cleanup.
func (attempt *StopAttempt) Wait(ctx context.Context) error {
	select {
	case <-attempt.done:
		return attempt.err
	default:
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// StopOwned removes an owned runtime and clears its identity only after the
// deployment confirms absence. A failure retains the identity for retry.
// The manager serializes access and supplies its fresh bounded cleanup context.
func StopOwned(ctx context.Context, provider deployment.Deployment, id *string) error {
	if *id == "" {
		return nil
	}
	if err := provider.Stop(ctx, *id); err != nil {
		return err
	}
	*id = ""
	return nil
}

// TransportClose closes a manager-owned deployment transport once.
type TransportClose struct {
	once sync.Once
	err  error
}

// Close retains the result for subsequent callers.
func (state *TransportClose) Close(provider deployment.Deployment) error {
	state.once.Do(func() { state.err = provider.Close() })
	return state.err
}
