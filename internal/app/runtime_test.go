package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRuntime struct {
	health         []error
	calls          int
	done           chan struct{}
	err            error
	stopped        bool
	stopContextErr error
}

func (f *fakeRuntime) Start(context.Context) error  { return nil }
func (f *fakeRuntime) Health(context.Context) error { e := f.health[f.calls]; f.calls++; return e }
func (f *fakeRuntime) Done() <-chan struct{}        { return f.done }
func (f *fakeRuntime) Err() error                   { return f.err }
func (f *fakeRuntime) Stop(ctx context.Context) error {
	f.stopped = true
	f.stopContextErr = ctx.Err()
	return nil
}

type classifiedHealth struct{ retry bool }

func (e classifiedHealth) Error() string   { return "opaque" }
func (e classifiedHealth) Retryable() bool { return e.retry }

func TestHealthRetryabilityIsStructuralAndUnknownIsTerminal(t *testing.T) {
	runtime := &fakeRuntime{health: []error{classifiedHealth{true}, nil}, done: make(chan struct{})}
	if err := waitForRuntimeHealth(context.Background(), runtime, func(context.Context, time.Duration) error { return nil }); err != nil || runtime.calls != 2 {
		t.Fatalf("calls=%d err=%v", runtime.calls, err)
	}
	runtime = &fakeRuntime{health: []error{errors.New("retryable-looking")}, done: make(chan struct{})}
	err := waitForRuntimeHealth(context.Background(), runtime, nil)
	if err == nil || runtime.calls != 1 {
		t.Fatalf("calls=%d err=%v", runtime.calls, err)
	}
	runtime = &fakeRuntime{health: []error{classifiedHealth{false}}, done: make(chan struct{})}
	if err := waitForRuntimeHealth(context.Background(), runtime, nil); err == nil || runtime.calls != 1 {
		t.Fatalf("calls=%d err=%v", runtime.calls, err)
	}
}

func TestRuntimeDoneDuringHealthReturnsPublishedError(t *testing.T) {
	done := make(chan struct{})
	close(done)
	runtime := &fakeRuntime{health: []error{classifiedHealth{true}}, done: done, err: errors.New("exit canary")}
	err := waitForRuntimeHealth(context.Background(), runtime, func(context.Context, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exit canary") {
		t.Fatalf("err=%v", err)
	}
}

func TestRuntimeDoneInterruptsStartupRetrySleepWithoutAnotherHealthCall(t *testing.T) {
	exitErr := errors.New("published exit canary")
	runtime := &fakeRuntime{health: []error{classifiedHealth{true}}, done: make(chan struct{}), err: exitErr}
	sleepEntered := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- waitForRuntimeHealth(context.Background(), runtime, func(ctx context.Context, _ time.Duration) error {
			close(sleepEntered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-sleepEntered
	close(runtime.done)
	select {
	case err := <-result:
		if !errors.Is(err, exitErr) {
			t.Fatalf("err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime exit did not interrupt startup retry sleep")
	}
	if runtime.calls != 1 {
		t.Fatalf("health calls=%d", runtime.calls)
	}
}

func TestRuntimeDoneInterruptsStartupRetrySleepStress(t *testing.T) {
	for i := 0; i < 200; i++ {
		exitErr := errors.New("published exit canary")
		runtime := &fakeRuntime{health: []error{classifiedHealth{true}}, done: make(chan struct{}), err: exitErr}
		sleepEntered := make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- waitForRuntimeHealth(context.Background(), runtime, func(ctx context.Context, _ time.Duration) error {
				close(sleepEntered)
				<-ctx.Done()
				return ctx.Err()
			})
		}()
		<-sleepEntered
		close(runtime.done)
		if err := <-result; !errors.Is(err, exitErr) {
			t.Fatalf("iteration=%d err=%v", i, err)
		}
		if runtime.calls != 1 {
			t.Fatalf("iteration=%d health calls=%d", i, runtime.calls)
		}
	}
}

type cleanupClassifiedError struct {
	err    error
	failed bool
}

func (e cleanupClassifiedError) Error() string       { return e.err.Error() }
func (e cleanupClassifiedError) Unwrap() error       { return e.err }
func (e cleanupClassifiedError) CleanupFailed() bool { return e.failed }

func TestRuntimeCleanupFailureClassificationIsStructuralAndConservative(t *testing.T) {
	unexpected := errors.New("unexpected")
	if runtimeCleanupFailed(nil) {
		t.Fatal("nil stop result was classified as cleanup failure")
	}
	if runtimeCleanupFailed(cleanupClassifiedError{err: unexpected}) {
		t.Fatal("unexpected-exit-only result was classified as cleanup failure")
	}
	if !runtimeCleanupFailed(cleanupClassifiedError{err: unexpected, failed: true}) {
		t.Fatal("classified cleanup failure was accepted")
	}
	if !runtimeCleanupFailed(unexpected) {
		t.Fatal("unknown stop error must default to cleanup failure")
	}
}

func TestStopRuntimeUsesFreshContext(t *testing.T) {
	runtime := &fakeRuntime{}
	if err := stopRuntime(runtime, time.Second); err != nil {
		t.Fatal(err)
	}
	if !runtime.stopped || runtime.stopContextErr != nil {
		t.Fatalf("stopped=%t ctxerr=%v", runtime.stopped, runtime.stopContextErr)
	}
}

func TestAdmittedRunCompletionJoinsPublishedExitWhenBothReady(t *testing.T) {
	exitErr := errors.New("published runtime exit")
	runtime := &fakeRuntime{done: make(chan struct{}), err: exitErr}
	completed := make(chan error, 1)
	completed <- errors.New("completed")
	close(runtime.done)
	result, runtimeErr := awaitRuntimeCompletion[error](runtime, completed, func() {})
	joined := errors.Join(result, runtimeErr)
	if result == nil || !errors.Is(joined, exitErr) {
		t.Fatalf("result=%v runtimeErr=%v", result, runtimeErr)
	}
}

func TestPreflightCompletionJoinsPublishedExitWhenBothReady(t *testing.T) {
	type preflightResult struct{ err error }
	exitErr := errors.New("published runtime exit")
	runtime := &fakeRuntime{done: make(chan struct{}), err: exitErr}
	completed := make(chan preflightResult, 1)
	completed <- preflightResult{err: errors.New("preflight completed")}
	close(runtime.done)
	result, runtimeErr := awaitRuntimeCompletion(runtime, completed, func() {})
	if !errors.Is(errors.Join(result.err, runtimeErr), exitErr) {
		t.Fatalf("result=%v runtimeErr=%v", result.err, runtimeErr)
	}
}

func TestRuntimeCompletionBothReadyStress(t *testing.T) {
	exitErr := errors.New("published runtime exit")
	for i := 0; i < 10_000; i++ {
		runtime := &fakeRuntime{done: make(chan struct{}), err: exitErr}
		completed := make(chan int, 1)
		completed <- i
		close(runtime.done)
		got, runtimeErr := awaitRuntimeCompletion(runtime, completed, func() {})
		if got != i || !errors.Is(runtimeErr, exitErr) {
			t.Fatalf("iteration=%d got=%d runtimeErr=%v", i, got, runtimeErr)
		}
	}
}
