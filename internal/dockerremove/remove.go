// Package dockerremove removes Docker containers under host contention. A
// loaded Engine rejects a removal with a conflict while an earlier force
// removal is still running, and drops the request outright once its own work
// queue outlasts the caller's deadline. Both are transient: the daemon
// finishes the removal on its own, so the absence of the container, not the
// success of any single API call, is the outcome that matters.
package dockerremove

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// Client is the Engine surface a retried removal needs. Both the official
// client and the sandbox and harness fakes satisfy it.
type Client interface {
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

// Policy is how long a removal keeps insisting.
type Policy struct {
	Attempts       int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
	// AttemptTimeout bounds one removal or confirmation call so a single
	// stalled request cannot consume the whole retry budget.
	AttemptTimeout time.Duration
}

// Default spans the roughly 30s a contended Engine has been observed to need
// before it drains a queued force removal: four backoffs of 2s, 4s, 8s, 16s.
func Default() Policy {
	return Policy{Attempts: 5, InitialBackoff: 2 * time.Second, MaxBackoff: 30 * time.Second, AttemptTimeout: 30 * time.Second}
}

// Container removes containerID and returns only once the Engine no longer
// knows the container. A container that is already gone is a success, and a
// conflicting or timed-out removal is retried with backoff for as long as ctx
// allows. The returned error reports the last removal failure, or the fact
// that the container outlived every attempt.
func (p Policy) Container(ctx context.Context, engine Client, containerID string, options client.ContainerRemoveOptions) error {
	var lastErr error
	backoff := p.InitialBackoff
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		if attempt > 1 {
			if err := wait(ctx, backoff); err != nil {
				return errors.Join(lastErr, err)
			}
			backoff = min(2*backoff, p.MaxBackoff)
		}
		removeErr := p.remove(ctx, engine, containerID, options)
		if removeErr != nil && !cerrdefs.IsNotFound(removeErr) {
			lastErr = removeErr
			if !retryable(removeErr) {
				return removeErr
			}
		}
		// An accepted removal is still asynchronous, and "already in
		// progress" means the daemon owns one; either way the only
		// conclusive signal is whether the container has gone.
		gone, inspectErr := p.absent(ctx, engine, containerID)
		if gone {
			return nil
		}
		if lastErr == nil {
			lastErr = inspectErr
		}
	}
	if lastErr == nil {
		lastErr = errors.New("container still exists")
	}
	return fmt.Errorf("after %d attempts: %w", p.Attempts, lastErr)
}

func (p Policy) remove(ctx context.Context, engine Client, containerID string, options client.ContainerRemoveOptions) error {
	attemptCtx, cancel := context.WithTimeout(ctx, p.AttemptTimeout)
	defer cancel()
	_, err := engine.ContainerRemove(attemptCtx, containerID, options)
	return err
}

// absent reports whether the Engine has stopped knowing the container.
func (p Policy) absent(ctx context.Context, engine Client, containerID string) (bool, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, p.AttemptTimeout)
	defer cancel()
	_, err := engine.ContainerInspect(attemptCtx, containerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("confirm container absence: %w", err)
	}
	return false, errors.New("container still exists")
}

// retryable reports whether the Engine may still complete the removal: a
// conflict with a removal it already started, or a request it dropped because
// the deadline elapsed while it was busy.
func retryable(err error) bool {
	switch {
	case err == nil:
		return false
	case cerrdefs.IsConflict(err), strings.Contains(err.Error(), "is already in progress"):
		return true
	case errors.Is(err, context.DeadlineExceeded), cerrdefs.IsDeadlineExceeded(err):
		return true
	default:
		return false
	}
}

// wait sleeps for the backoff unless ctx ends first.
func wait(ctx context.Context, backoff time.Duration) error {
	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
