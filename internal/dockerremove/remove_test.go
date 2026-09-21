package dockerremove

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// fastPolicy keeps the retry shape but not its waiting.
func fastPolicy() Policy {
	return Policy{Attempts: 5, InitialBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, AttemptTimeout: time.Second}
}

// fakeEngine answers a scripted removal error per attempt and reports the
// container present until presentUntil removals have been attempted.
type fakeEngine struct {
	removeErrs   []error
	presentUntil int
	inspectErr   error
	removes      int
	inspects     int
}

func (f *fakeEngine) ContainerRemove(ctx context.Context, _ string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.removes++
	if err := ctx.Err(); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	if f.removes <= len(f.removeErrs) {
		return client.ContainerRemoveResult{}, f.removeErrs[f.removes-1]
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeEngine) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.inspects++
	if f.inspectErr != nil {
		return client.ContainerInspectResult{}, f.inspectErr
	}
	if f.removes <= f.presentUntil {
		return client.ContainerInspectResult{}, nil
	}
	return client.ContainerInspectResult{}, cerrdefs.ErrNotFound
}

func conflict() error {
	return errors.Join(cerrdefs.ErrConflict, errors.New("Error response from daemon: removal of container abc is already in progress"))
}

// The daemon owns a removal it has already started: the retry has to outlast
// the conflict rather than report it.
func TestContainerRetriesRemovalAlreadyInProgress(t *testing.T) {
	engine := &fakeEngine{removeErrs: []error{conflict(), conflict()}, presentUntil: 2}
	if err := fastPolicy().Container(context.Background(), engine, "abc", client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 3 {
		t.Fatalf("removes = %d, want 3", engine.removes)
	}
}

// A conflict the daemon resolves on its own is a success even though every
// removal call the retry makes keeps failing.
func TestContainerAcceptsAbsenceAfterConflict(t *testing.T) {
	engine := &fakeEngine{removeErrs: []error{conflict(), conflict(), conflict(), conflict(), conflict()}, presentUntil: 1}
	if err := fastPolicy().Container(context.Background(), engine, "abc", client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 2 {
		t.Fatalf("removes = %d, want 2", engine.removes)
	}
}

// A dropped request is retried, because the Engine may have queued the work.
func TestContainerRetriesDeadlineExceeded(t *testing.T) {
	engine := &fakeEngine{removeErrs: []error{context.DeadlineExceeded}, presentUntil: 1}
	if err := fastPolicy().Container(context.Background(), engine, "abc", client.ContainerRemoveOptions{Force: true}); err != nil {
		t.Fatalf("err = %v", err)
	}
}

// A container the Engine no longer knows is already the wanted outcome.
func TestContainerTreatsNotFoundAsRemoved(t *testing.T) {
	engine := &fakeEngine{removeErrs: []error{cerrdefs.ErrNotFound}}
	if err := fastPolicy().Container(context.Background(), engine, "abc", client.ContainerRemoveOptions{}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 1 {
		t.Fatalf("removes = %d, want 1", engine.removes)
	}
}

// A refusal the daemon will not resolve must surface at once rather than
// spend the retry budget.
func TestContainerDoesNotRetryUnrelatedFailure(t *testing.T) {
	refused := errors.New("Error response from daemon: permission denied")
	engine := &fakeEngine{removeErrs: []error{refused}, presentUntil: 5}
	err := fastPolicy().Container(context.Background(), engine, "abc", client.ContainerRemoveOptions{})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 1 {
		t.Fatalf("removes = %d, want 1", engine.removes)
	}
}

// A container that outlives every attempt is still a failure, and the report
// names the attempts spent on it.
func TestContainerFailsWhenContainerSurvives(t *testing.T) {
	engine := &fakeEngine{presentUntil: 99}
	err := fastPolicy().Container(context.Background(), engine, "abc", client.ContainerRemoveOptions{})
	if err == nil || !strings.Contains(err.Error(), "after 5 attempts") || !strings.Contains(err.Error(), "still exists") {
		t.Fatalf("err = %v", err)
	}
	if engine.removes != 5 {
		t.Fatalf("removes = %d, want 5", engine.removes)
	}
}

// The retry stops when the cleanup budget does, instead of sleeping past it.
func TestContainerStopsWhenContextEnds(t *testing.T) {
	engine := &fakeEngine{removeErrs: []error{conflict(), conflict(), conflict(), conflict(), conflict()}, presentUntil: 99}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	policy := Policy{Attempts: 5, InitialBackoff: time.Hour, MaxBackoff: time.Hour, AttemptTimeout: time.Second}
	started := time.Now()
	if err := policy.Container(ctx, engine, "abc", client.ContainerRemoveOptions{}); err == nil {
		t.Fatal("want an error once the budget is spent")
	}
	if elapsed := time.Since(started); elapsed > time.Minute {
		t.Fatalf("waited %s past the deadline", elapsed)
	}
}

// Default has to leave room for the Engine to drain before it gives up.
func TestDefaultSpansTheObservedDrain(t *testing.T) {
	policy := Default()
	total := time.Duration(0)
	backoff := policy.InitialBackoff
	for attempt := 2; attempt <= policy.Attempts; attempt++ {
		total += backoff
		backoff = min(2*backoff, policy.MaxBackoff)
	}
	if total < 30*time.Second {
		t.Fatalf("backoff span = %s, want at least 30s", total)
	}
}
