package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// fastRemoval keeps the retry shape but not its waiting.
func fastRemoval() removalPolicy {
	return removalPolicy{attempts: 5, initialBackoff: time.Millisecond, maxBackoff: 2 * time.Millisecond, attemptTimeout: time.Second, budget: time.Minute}
}

// contendedDocker answers a scripted removal error per attempt and keeps the
// container present until more than presentUntil removals have been attempted.
type contendedDocker struct {
	*fakeDocker
	removeErrs   []error
	presentUntil int
	removes      int
}

func (f *contendedDocker) ContainerRemove(ctx context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removes++
	if err := ctx.Err(); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	if f.removes <= len(f.removeErrs) {
		return client.ContainerRemoveResult{}, f.removeErrs[f.removes-1]
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *contendedDocker) ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if f.removes > f.presentUntil {
		return client.ContainerInspectResult{}, errdefs.ErrNotFound
	}
	return f.fakeDocker.ContainerInspect(ctx, id, options)
}

// contended returns a running deployment whose removal follows the script.
func contended(t *testing.T, policy removalPolicy, presentUntil int, removeErrs ...error) (*Manager, *contendedDocker, string) {
	t.Helper()
	engine := &contendedDocker{fakeDocker: newFakeDocker(), removeErrs: removeErrs, presentUntil: presentUntil}
	manager := &Manager{client: engine, removal: policy}
	id, err := manager.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return manager, engine, id
}

func conflict() error {
	return errors.Join(errdefs.ErrConflict, errors.New("Error response from daemon: removal of container abc is already in progress"))
}

// The daemon owns a removal it has already started: the retry has to outlast
// the conflict rather than report it.
func TestStopRetriesRemovalAlreadyInProgress(t *testing.T) {
	manager, engine, id := contended(t, fastRemoval(), 2, conflict(), conflict())
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 3 {
		t.Fatalf("removes = %d, want 3", engine.removes)
	}
}

// A conflict the daemon resolves on its own is a success even though every
// removal call the retry makes keeps failing.
func TestStopAcceptsAbsenceAfterConflict(t *testing.T) {
	manager, engine, id := contended(t, fastRemoval(), 1, conflict(), conflict(), conflict(), conflict(), conflict())
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 2 {
		t.Fatalf("removes = %d, want 2", engine.removes)
	}
}

// A dropped request is retried, because the Engine may have queued the work.
func TestStopRetriesRemovalDeadlineExceeded(t *testing.T) {
	manager, engine, id := contended(t, fastRemoval(), 1, context.DeadlineExceeded)
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 2 {
		t.Fatalf("removes = %d, want 2", engine.removes)
	}
}

// A container the Engine no longer knows is already the wanted outcome.
func TestStopTreatsRemovalNotFoundAsRemoved(t *testing.T) {
	manager, engine, id := contended(t, fastRemoval(), 0, errdefs.ErrNotFound)
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 1 {
		t.Fatalf("removes = %d, want 1", engine.removes)
	}
}

// A refusal the daemon will not resolve must surface at once rather than
// spend the retry budget.
func TestStopDoesNotRetryUnrelatedRemovalFailure(t *testing.T) {
	refused := errors.New("Error response from daemon: permission denied")
	manager, engine, id := contended(t, fastRemoval(), 5, refused)
	err := manager.Stop(context.Background(), id)
	if !errors.Is(err, refused) || !strings.Contains(err.Error(), "remains after removal") {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 1 {
		t.Fatalf("removes = %d, want 1", engine.removes)
	}
}

// A removal call can fail outright while the daemon still removes the
// container. Absence decides the outcome, so the failure is not reported.
func TestStopAcceptsAbsenceAfterUnrelatedRemovalFailure(t *testing.T) {
	failed := errors.New("Error response from daemon: driver failed to remove root filesystem")
	manager, engine, id := contended(t, fastRemoval(), 0, failed)
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 1 {
		t.Fatalf("removes = %d, want 1", engine.removes)
	}
}

// A container that outlives every attempt is still a failure that names the
// attempts spent on it, and a later Stop can still confirm its absence.
func TestStopFailsClosedWhenContainerSurvivesRemoval(t *testing.T) {
	manager, engine, id := contended(t, fastRemoval(), 5)
	err := manager.Stop(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "after 5 removal attempts") || !strings.Contains(err.Error(), "remains after removal") {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 5 {
		t.Fatalf("removes = %d, want 5", engine.removes)
	}
	for range 2 {
		if err := manager.Stop(context.Background(), id); err != nil {
			t.Fatalf("retried Stop: %v", err)
		}
	}
}

// The retry stops when the cleanup context does, instead of sleeping past it.
func TestStopRemovalEndsWithItsContext(t *testing.T) {
	policy := removalPolicy{attempts: 5, initialBackoff: time.Hour, maxBackoff: time.Hour, attemptTimeout: time.Second, budget: 2 * time.Hour}
	manager, _, id := contended(t, policy, 99, conflict())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := manager.Stop(ctx, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Minute {
		t.Fatalf("waited %s past the deadline", elapsed)
	}
}

// The policy bounds itself, so a caller's longer deadline still leaves time
// for the cleanup that follows the removal.
func TestStopRemovalEndsAtItsBudget(t *testing.T) {
	policy := removalPolicy{attempts: 5, initialBackoff: time.Hour, maxBackoff: time.Hour, attemptTimeout: time.Second, budget: 5 * time.Millisecond}
	manager, _, id := contended(t, policy, 99, conflict())
	started := time.Now()
	if err := manager.Stop(context.Background(), id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the budget's deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Minute {
		t.Fatalf("waited %s past the budget", elapsed)
	}
}

// The default has to leave room for the Engine to drain before it gives up.
func TestDefaultRemovalSpansTheObservedDrain(t *testing.T) {
	policy := defaultRemoval
	total := time.Duration(0)
	backoff := policy.initialBackoff
	for attempt := 2; attempt <= policy.attempts; attempt++ {
		total += backoff
		backoff = min(2*backoff, policy.maxBackoff)
	}
	if total < 30*time.Second {
		t.Fatalf("backoff span = %s, want at least 30s", total)
	}
	if policy.budget <= total+policy.attemptTimeout {
		t.Fatalf("budget = %s, want more than the %s backoff span plus one %s attempt", policy.budget, total, policy.attemptTimeout)
	}
}
