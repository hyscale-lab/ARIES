package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// removalPolicy is how long a container removal keeps insisting. A loaded
// Engine rejects a removal with a conflict while an earlier force removal is
// still running, and drops the request once its own queue outlasts the
// caller's deadline. Both are transient: the daemon finishes the removal on
// its own, so the absence of the container, not the success of any single
// call, is the outcome.
type removalPolicy struct {
	attempts       int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	// attemptTimeout bounds one removal or confirmation call so a single
	// stalled request cannot consume the whole budget.
	attemptTimeout time.Duration
	// budget bounds the whole removal, however long the caller's deadline,
	// so the cleanup that follows it (a network release, say) keeps time.
	budget time.Duration
}

// defaultRemoval spans the roughly 30s a contended Engine has been observed
// to need before it drains a queued force removal: four backoffs of 2s, 4s,
// 8s, 16s. Its budget leaves the calls between them room on top of that
// span; owners size their cleanup deadlines from it.
var defaultRemoval = removalPolicy{
	attempts:       5,
	initialBackoff: 2 * time.Second,
	maxBackoff:     30 * time.Second,
	attemptTimeout: 15 * time.Second,
	budget:         60 * time.Second,
}

// removeContainer removes the container and returns nil only once the Engine
// no longer knows it. A conflicting or timed-out removal is retried with
// backoff for as long as ctx and the policy budget allow; any other failure
// is reported once the container is confirmed to have outlived it. recovered
// is the last failure a successful removal outlasted, for the caller to log.
func (manager *Manager) removeContainer(ctx context.Context, id string) (recovered, err error) {
	policy := manager.removal
	if policy.attempts <= 0 {
		policy = defaultRemoval
	}
	ctx, cancel := context.WithTimeout(ctx, policy.budget)
	defer cancel()
	var lastErr error
	backoff := policy.initialBackoff
	for attempt := 1; attempt <= policy.attempts; attempt++ {
		if attempt > 1 {
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, errors.Join(lastErr, ctx.Err())
			}
			backoff = min(2*backoff, policy.maxBackoff)
		}
		removeErr := manager.removeOnce(ctx, id, policy.attemptTimeout)
		if removeErr != nil {
			lastErr = removeErr
		}
		// An accepted removal is still asynchronous, "already in progress"
		// means the daemon owns one, and a failed call may have removed the
		// container anyway; in every case the only conclusive signal is
		// whether the container has gone.
		absenceErr := manager.confirmAbsent(ctx, id, policy.attemptTimeout)
		if absenceErr == nil {
			return lastErr, nil
		}
		if removeErr != nil && !retryableRemoval(removeErr) {
			return nil, errors.Join(removeErr, absenceErr)
		}
		if lastErr == nil {
			lastErr = absenceErr
		}
	}
	return nil, fmt.Errorf("after %d removal attempts: %w", policy.attempts, lastErr)
}

func (manager *Manager) removeOnce(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := manager.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove deployment: %w", err)
	}
	return nil
}

// confirmAbsent returns nil only when the Engine reports the container gone.
func (manager *Manager) confirmAbsent(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify deployment removal: %w", err)
	}
	return errors.New("deployment remains after removal")
}

// retryableRemoval reports whether the Engine may still complete the removal:
// a conflict with a removal it already started, or a request it dropped
// because the deadline elapsed while it was busy.
func retryableRemoval(err error) bool {
	return errdefs.IsConflict(err) || strings.Contains(err.Error(), "is already in progress") ||
		errors.Is(err, context.DeadlineExceeded) || errdefs.IsDeadlineExceeded(err)
}
