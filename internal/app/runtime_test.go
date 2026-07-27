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

func TestStopRuntimeUsesFreshContext(t *testing.T) {
	runtime := &fakeRuntime{}
	if err := stopRuntime(runtime, time.Second); err != nil {
		t.Fatal(err)
	}
	if !runtime.stopped || runtime.stopContextErr != nil {
		t.Fatalf("stopped=%t ctxerr=%v", runtime.stopped, runtime.stopContextErr)
	}
}
