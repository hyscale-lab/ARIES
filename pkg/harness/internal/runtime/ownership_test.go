package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

func TestStopAttemptRetainsOutcomeAcrossRetry(t *testing.T) {
	var state StopState
	first, owner := state.Begin(true)
	if !owner || !state.Running() {
		t.Fatal("first caller did not own cleanup")
	}
	waiter, owner := state.Begin(true)
	if owner || waiter != first {
		t.Fatal("concurrent caller did not join original attempt")
	}
	failure := errors.New("absence unconfirmed")
	state.Finish(failure)
	retry, owner := state.Begin(true)
	if !owner || retry == first {
		t.Fatal("failed cleanup cannot be retried")
	}
	state.Finish(nil)
	if err := waiter.Wait(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("waiter lost original result: %v", err)
	}
	if err := retry.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	done, owner := state.Begin(false)
	if owner || done != retry {
		t.Fatal("completed cleanup restarted")
	}
}

func TestConcurrentWaitersObservePublishedResult(t *testing.T) {
	var state StopState
	attempt, _ := state.Begin(true)
	const waiters = 12
	results := make(chan error, waiters)
	for range waiters {
		go func() { results <- attempt.Wait(context.Background()) }()
	}
	failure := errors.New("failed first attempt")
	state.Finish(failure)
	_, _ = state.Begin(true)
	state.Finish(nil)
	for range waiters {
		if err := <-results; !errors.Is(err, failure) {
			t.Fatalf("waiter result: %v", err)
		}
	}
}

func TestCanceledWaiterDoesNotCancelCleanup(t *testing.T) {
	var state StopState
	attempt, _ := state.Begin(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := attempt.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait: %v", err)
	}
	if !state.Running() {
		t.Fatal("waiter canceled owning cleanup")
	}
	state.Finish(nil)
	if err := attempt.Wait(ctx); err != nil {
		t.Fatalf("completed outcome should take precedence: %v", err)
	}
}

type fakeProvider struct {
	deployment.Deployment
	stopErr  error
	stops    int
	closes   int
	closeErr error
}

func (p *fakeProvider) Stop(context.Context, string) error { p.stops++; return p.stopErr }
func (p *fakeProvider) Close() error                       { p.closes++; return p.closeErr }

func TestOwnedRuntimeRetainedUntilConfirmedAbsent(t *testing.T) {
	provider := &fakeProvider{stopErr: errors.New("remove failed")}
	id := "runtime"
	if err := StopOwned(context.Background(), provider, &id); err == nil || id != "runtime" {
		t.Fatalf("lost ownership after failure: %q, %v", id, err)
	}
	provider.stopErr = nil
	if err := StopOwned(context.Background(), provider, &id); err != nil || id != "" {
		t.Fatalf("confirmed cleanup: %q, %v", id, err)
	}
	if err := StopOwned(context.Background(), provider, &id); err != nil || provider.stops != 2 {
		t.Fatalf("cleanup not idempotent: %v", err)
	}
}

func TestTransportCloseRetainsError(t *testing.T) {
	failure := errors.New("close failed")
	provider := &fakeProvider{closeErr: failure}
	var close TransportClose
	for range 2 {
		if err := close.Close(provider); !errors.Is(err, failure) {
			t.Fatal(err)
		}
	}
	if provider.closes != 1 {
		t.Fatal("closed deployment more than once")
	}
}
