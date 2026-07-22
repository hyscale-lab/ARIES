package runner

import (
	"context"
	"errors"
	"github.com/sirupsen/logrus"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

var errInjected = errors.New("injected failure")

const fakeRollbackTimeout = 100 * time.Millisecond

type callLog struct {
	mu        sync.Mutex
	calls     []string
	ctxErrs   map[string][]error
	deadlines map[string][]bool
	hooks     map[string]func()
}

func newCallLog() *callLog {
	return &callLog{
		ctxErrs:   make(map[string][]error),
		deadlines: make(map[string][]bool),
		hooks:     make(map[string]func()),
	}
}

func (l *callLog) add(name string, ctx context.Context) {
	l.mu.Lock()
	l.calls = append(l.calls, name)
	if ctx != nil {
		l.ctxErrs[name] = append(l.ctxErrs[name], ctx.Err())
		_, hasDeadline := ctx.Deadline()
		l.deadlines[name] = append(l.deadlines[name], hasDeadline)
	}
	hook := l.hooks[name]
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (l *callLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func (l *callLog) setHook(name string, hook func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hooks[name] = hook
}

func (l *callLog) contextErrors(name string) []error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]error(nil), l.ctxErrs[name]...)
}

func (l *callLog) contextDeadlines(name string) []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bool(nil), l.deadlines[name]...)
}

func (l *callLog) addRollback(name string, ctx context.Context) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fakeRollbackTimeout)
	defer cancel()
	l.add(name, rollbackCtx)
}

type fakeBenchmark struct {
	mu              sync.Mutex
	log             *callLog
	tasks           []core.Task
	tasksErr        error
	evaluation      core.Evaluation
	evaluationError error
	evaluationErrs  []error
	evaluations     int
	evaluationDelay time.Duration
	afterTasks      func()
}

func (f *fakeBenchmark) Tasks(ctx context.Context) ([]core.Task, error) {
	f.log.add("benchmark.tasks", ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.afterTasks != nil {
		f.afterTasks()
	}
	return f.tasks, f.tasksErr
}

func (f *fakeBenchmark) Evaluate(ctx context.Context, _ core.Task, _ Sandbox) (core.Evaluation, error) {
	f.log.add("benchmark.evaluate", ctx)
	if err := ctx.Err(); err != nil {
		return core.Evaluation{}, err
	}
	if f.evaluationDelay > 0 {
		timer := time.NewTimer(f.evaluationDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return core.Evaluation{}, ctx.Err()
		case <-timer.C:
		}
	}
	f.mu.Lock()
	index := f.evaluations
	f.evaluations++
	f.mu.Unlock()
	if index < len(f.evaluationErrs) {
		return f.evaluation, f.evaluationErrs[index]
	}
	return f.evaluation, f.evaluationError
}

type fakeToolSandbox struct {
	mu         sync.Mutex
	log        *callLog
	startErr   error
	stopErrors []error
	stopWait   bool
	stops      int
	sandbox    *fakeSandbox
	requests   []core.SandboxRequest
}

func (f *fakeToolSandbox) Start(ctx context.Context, request core.SandboxRequest) (Sandbox, error) {
	f.log.add("sandbox.start", ctx)
	f.mu.Lock()
	f.requests = append(f.requests, request)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		f.log.addRollback("sandbox.rollback", ctx)
		return nil, err
	}
	if f.startErr != nil {
		f.log.addRollback("sandbox.rollback", ctx)
		return nil, f.startErr
	}
	return f.sandbox, nil
}

func (f *fakeToolSandbox) Stop(ctx context.Context, _ Sandbox) error {
	f.log.add("sandbox.stop", ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	stopWait := f.stopWait
	err := indexedError(f.stopErrors, f.stops)
	f.stops++
	f.mu.Unlock()
	if stopWait {
		<-ctx.Done()
		return ctx.Err()
	}
	return err
}

func (f *fakeToolSandbox) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

type fakeSandbox struct {
}

func (f *fakeSandbox) Exec(context.Context, core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, nil
}

func (f *fakeSandbox) Upload(context.Context, string, string) error { return nil }

func (f *fakeSandbox) Download(context.Context, string, string) error { return nil }

type fakeBridge struct {
	mu         sync.Mutex
	log        *callLog
	startErr   error
	stopErrors []error
	stops      int
}

func (f *fakeBridge) Start(ctx context.Context, _ Sandbox) (core.ToolEndpoint, error) {
	f.log.add("bridge.start", ctx)
	if err := ctx.Err(); err != nil {
		f.log.addRollback("bridge.rollback", ctx)
		return core.ToolEndpoint{}, err
	}
	if f.startErr != nil {
		f.log.addRollback("bridge.rollback", ctx)
		return core.ToolEndpoint{}, f.startErr
	}
	return core.ToolEndpoint{Protocol: "fake", Address: "task", LogPaths: []string{"/runs/tool-calls.jsonl"}}, nil
}

func (f *fakeBridge) Stop(ctx context.Context) error {
	f.log.add("bridge.stop", ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	err := indexedError(f.stopErrors, f.stops)
	f.stops++
	f.mu.Unlock()
	return err
}

func (f *fakeBridge) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

type fakeHarness struct {
	mu         sync.Mutex
	log        *callLog
	requests   []core.HarnessRequest
	startErr   error
	runErrors  []error
	stopErrors []error
	runs       int
	stops      int
}

func (f *fakeHarness) Start(ctx context.Context, request core.HarnessRequest) error {
	f.log.add("harness.start", ctx)
	if err := ctx.Err(); err != nil {
		f.log.addRollback("harness.rollback", ctx)
		return err
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	startErr := f.startErr
	f.mu.Unlock()
	if startErr != nil {
		f.log.addRollback("harness.rollback", ctx)
	}
	return startErr
}

func (f *fakeHarness) Run(ctx context.Context, _ string) (core.HarnessResult, error) {
	f.log.add("harness.run", ctx)
	if err := ctx.Err(); err != nil {
		return core.HarnessResult{}, err
	}
	f.mu.Lock()
	err := indexedError(f.runErrors, f.runs)
	f.runs++
	f.mu.Unlock()
	return core.HarnessResult{FinalResponse: "done"}, err
}

func (f *fakeHarness) Stop(ctx context.Context) error {
	f.log.add("harness.stop", ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	err := indexedError(f.stopErrors, f.stops)
	f.stops++
	f.mu.Unlock()
	return err
}

func (f *fakeHarness) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stops
}

func indexedError(errs []error, index int) error {
	if index < len(errs) {
		return errs[index]
	}
	return nil
}

type rig struct {
	log       *callLog
	benchmark *fakeBenchmark
	factory   *fakeToolSandbox
	bridge    *fakeBridge
	harness   *fakeHarness
	runner    *Runner
}

func newRig(t *testing.T, tasks int) *rig {
	t.Helper()
	log := newCallLog()
	taskList := make([]core.Task, tasks)
	for i := range taskList {
		taskList[i] = core.Task{ID: string(rune('a' + i)), Instruction: "do work", Environment: core.Environment{Image: "fixture", Workdir: "/work"}}
	}
	sandbox := &fakeSandbox{}
	benchmark := &fakeBenchmark{log: log, tasks: taskList, evaluation: core.Evaluation{Reward: 1}}
	factory := &fakeToolSandbox{log: log, sandbox: sandbox}
	bridge := &fakeBridge{log: log}
	harness := &fakeHarness{log: log}
	runner, err := New(benchmark, harness, factory, bridge, Options{
		Name:           "test",
		RunID:          "test-run",
		OutputDir:      "runs",
		CleanupTimeout: time.Second,
		Logger:         logrus.New(),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return &rig{log: log, benchmark: benchmark, factory: factory, bridge: bridge, harness: harness, runner: runner}
}

func TestRunnerSuccessOrdering(t *testing.T) {
	rig := newRig(t, 1)
	result, err := rig.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	wantCalls := []string{
		"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run",
		"harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop",
	}
	if got := rig.log.snapshot(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("calls = %v, want %v", got, wantCalls)
	}
	task := result.Tasks[0]
	if task.Harness.Status != core.StatusSucceeded || task.Isolation.Status != core.StatusConfirmed || task.Evaluation.Status != core.StatusSucceeded || task.Cleanup.Status != core.StatusSucceeded {
		t.Fatalf("unexpected task result: %#v", task)
	}
	if task.Observer.Status != core.StatusNotEnabled {
		t.Fatalf("observer status = %q", task.Observer.Status)
	}
	if !reflect.DeepEqual(task.ToolLogPaths, []string{"/runs/tool-calls.jsonl"}) {
		t.Fatalf("tool log paths = %q", task.ToolLogPaths)
	}
	if task.Observer.Duration != 0 || task.Observer.SampleCount != 0 || len(task.Observer.LogPaths) != 0 {
		t.Fatalf("disabled observer recorded data: %#v", task.Observer)
	}
	if result.RunID != "test-run" {
		t.Fatalf("run ID = %q, want test-run", result.RunID)
	}
	rig.factory.mu.Lock()
	requests := append([]core.SandboxRequest(nil), rig.factory.requests...)
	rig.factory.mu.Unlock()
	if len(requests) != 1 || requests[0].RunID != "test-run" || requests[0].TaskID != "a" || !reflect.DeepEqual(requests[0].Environment, rig.benchmark.tasks[0].Environment) {
		t.Fatalf("sandbox requests = %#v", requests)
	}
	rig.harness.mu.Lock()
	harnessRequests := append([]core.HarnessRequest(nil), rig.harness.requests...)
	rig.harness.mu.Unlock()
	if len(harnessRequests) != 1 || harnessRequests[0].RunID != "test-run" || harnessRequests[0].TaskID != "a" || harnessRequests[0].OutputDir != "runs" {
		t.Fatalf("harness requests = %#v", harnessRequests)
	}
}

func TestRunnerFailuresAtEveryCall(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*rig, error)
		wantCalls []string
		wantTasks int
		wantEval  string
		rollback  string
	}{
		{
			name: "tasks", configure: func(r *rig, err error) { r.benchmark.tasksErr = err }, wantTasks: 0,
			wantCalls: []string{"benchmark.tasks"},
		},
		{
			name: "sandbox start", configure: func(r *rig, err error) { r.factory.startErr = err }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "sandbox.rollback"}, wantEval: core.StatusNotRun, rollback: "sandbox.rollback",
		},
		{
			name: "bridge start", configure: func(r *rig, err error) { r.bridge.startErr = err }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "bridge.rollback", "sandbox.stop"}, wantEval: core.StatusNotRun, rollback: "bridge.rollback",
		},
		{
			name: "harness start", configure: func(r *rig, err error) { r.harness.startErr = err }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.rollback", "harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop"}, wantEval: core.StatusSucceeded, rollback: "harness.rollback",
		},
		{
			name: "harness run", configure: func(r *rig, err error) { r.harness.runErrors = []error{err} }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run", "harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop"}, wantEval: core.StatusSucceeded,
		},
		{
			name: "harness stop", configure: func(r *rig, err error) { r.harness.stopErrors = []error{err, nil} }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run", "harness.stop", "bridge.stop", "harness.stop", "sandbox.stop"}, wantEval: core.StatusBlockedIsolation,
		},
		{
			name: "bridge stop", configure: func(r *rig, err error) { r.bridge.stopErrors = []error{err, nil} }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run", "harness.stop", "bridge.stop", "bridge.stop", "sandbox.stop"}, wantEval: core.StatusBlockedIsolation,
		},
		{
			name: "evaluate", configure: func(r *rig, err error) { r.benchmark.evaluationError = err }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run", "harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop"}, wantEval: core.StatusFailed,
		},
		{
			name: "sandbox stop", configure: func(r *rig, err error) { r.factory.stopErrors = []error{err} }, wantTasks: 1,
			wantCalls: []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run", "harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop"}, wantEval: core.StatusSucceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newRig(t, 1)
			test.configure(rig, errInjected)
			result, err := rig.runner.Run(context.Background())
			if !errors.Is(err, errInjected) {
				t.Fatalf("Run() error = %v, want errors.Is(_, injected failure)", err)
			}
			if len(result.Tasks) != test.wantTasks {
				t.Fatalf("task count = %d, want %d", len(result.Tasks), test.wantTasks)
			}
			if got := rig.log.snapshot(); !reflect.DeepEqual(got, test.wantCalls) {
				t.Fatalf("calls = %v, want %v", got, test.wantCalls)
			}
			if test.wantTasks == 1 && result.Tasks[0].Evaluation.Status != test.wantEval {
				t.Fatalf("evaluation status = %q, want %q", result.Tasks[0].Evaluation.Status, test.wantEval)
			}
			if test.rollback != "" {
				assertBoundedLiveContext(t, rig.log, test.rollback)
			}
		})
	}
}

func TestRunnerCancellationAtEveryLifecycleCutPoint(t *testing.T) {
	fullCalls := []string{
		"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.run",
		"harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop",
	}
	tests := []struct {
		name      string
		cancelAt  string
		wantCalls []string
		wantTasks int
		wantEval  string
		rollback  string
	}{
		{"tasks", "benchmark.tasks", []string{"benchmark.tasks"}, 0, "", ""},
		{"sandbox start", "sandbox.start", []string{"benchmark.tasks", "sandbox.start", "sandbox.rollback"}, 1, core.StatusNotRun, "sandbox.rollback"},
		{"bridge start", "bridge.start", []string{"benchmark.tasks", "sandbox.start", "bridge.start", "bridge.rollback", "sandbox.stop"}, 1, core.StatusNotRun, "bridge.rollback"},
		{"harness start", "harness.start", []string{"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.rollback", "harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop"}, 1, core.StatusCanceled, "harness.rollback"},
		{"harness run", "harness.run", fullCalls, 1, core.StatusCanceled, ""},
		{"harness stop", "harness.stop", fullCalls, 1, core.StatusCanceled, ""},
		{"bridge stop", "bridge.stop", fullCalls, 1, core.StatusCanceled, ""},
		{"evaluate", "benchmark.evaluate", fullCalls, 1, core.StatusCanceled, ""},
		{"sandbox stop", "sandbox.stop", fullCalls, 1, core.StatusSucceeded, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rig := newRig(t, 1)
			deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), time.Second)
			defer deadlineCancel()
			ctx, cancel := context.WithCancel(deadlineCtx)
			defer cancel()
			rig.log.setHook(test.cancelAt, cancel)

			result, err := rig.runner.Run(ctx)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Run() error = %v, want real caller cancellation", err)
			}
			if len(result.Tasks) != test.wantTasks {
				t.Fatalf("task count = %d, want %d", len(result.Tasks), test.wantTasks)
			}
			if got := rig.log.snapshot(); !reflect.DeepEqual(got, test.wantCalls) {
				t.Fatalf("calls = %v, want %v", got, test.wantCalls)
			}
			if test.wantTasks == 1 && result.Tasks[0].Evaluation.Status != test.wantEval {
				t.Fatalf("evaluation status = %q, want %q", result.Tasks[0].Evaluation.Status, test.wantEval)
			}
			if test.rollback != "" {
				assertBoundedLiveContext(t, rig.log, test.rollback)
			}
			for _, call := range []string{"harness.stop", "bridge.stop", "sandbox.stop"} {
				for i, ctxErr := range rig.log.contextErrors(call) {
					if ctxErr != nil {
						t.Fatalf("%s context %d error = %v, want live WithoutCancel-derived context", call, i, ctxErr)
					}
				}
			}
		})
	}
}

func TestRunnerCanceledBeforeTaskScheduling(t *testing.T) {
	rig := newRig(t, 1)
	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), time.Second)
	defer deadlineCancel()
	ctx, cancel := context.WithCancel(deadlineCtx)
	defer cancel()
	rig.benchmark.afterTasks = cancel

	result, err := rig.runner.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want cancellation", err)
	}
	if len(result.Tasks) != 0 {
		t.Fatalf("task count = %d, want zero", len(result.Tasks))
	}
	if got := rig.log.snapshot(); !reflect.DeepEqual(got, []string{"benchmark.tasks"}) {
		t.Fatalf("calls = %v, want no task lifecycle calls", got)
	}
}

func assertBoundedLiveContext(t *testing.T, log *callLog, name string) {
	t.Helper()
	errs := log.contextErrors(name)
	deadlines := log.contextDeadlines(name)
	if len(errs) == 0 || len(errs) != len(deadlines) {
		t.Fatalf("%s context metadata errors=%v deadlines=%v", name, errs, deadlines)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("%s context %d error = %v, want live rollback context", name, i, errs[i])
		}
		if !deadlines[i] {
			t.Fatalf("%s context %d has no deadline", name, i)
		}
	}
}

func TestRunnerCleanupTimeoutJoinsPrimaryError(t *testing.T) {
	rig := newRig(t, 1)
	rig.harness.runErrors = []error{errInjected}
	rig.factory.stopWait = true
	rig.runner.cleanupTimeout = 10 * time.Millisecond

	result, err := rig.runner.Run(context.Background())
	if !errors.Is(err, errInjected) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run() error = %v, want primary and cleanup deadline", err)
	}
	if result.Tasks[0].Cleanup.Status != core.StatusFailed {
		t.Fatalf("cleanup = %#v, want failed", result.Tasks[0].Cleanup)
	}
	if got := result.Tasks[0].Cleanup.Error; got == "" || !strings.Contains(got, "cleanup sandbox") {
		t.Fatalf("cleanup error = %q, want component context", got)
	}
}

func TestEvaluationDoesNotConsumeSandboxCleanupBudget(t *testing.T) {
	rig := newRig(t, 1)
	rig.runner.cleanupTimeout = 5 * time.Millisecond
	rig.benchmark.evaluationDelay = 10 * time.Millisecond

	result, err := rig.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Tasks[0].Cleanup.Status != core.StatusSucceeded {
		t.Fatalf("cleanup = %#v, want fresh post-evaluation budget", result.Tasks[0].Cleanup)
	}
}

func TestRunnerIsolationFailureNeverEvaluatesEvenAfterCleanupRetry(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*rig)
	}{
		{"harness", func(r *rig) { r.harness.stopErrors = []error{errInjected, nil} }},
		{"bridge", func(r *rig) { r.bridge.stopErrors = []error{errInjected, nil} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			rig := newRig(t, 1)
			test.configure(rig)
			result, err := rig.runner.Run(context.Background())
			if !errors.Is(err, errInjected) {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Tasks[0].Evaluation.Status != core.StatusBlockedIsolation {
				t.Fatalf("evaluation = %#v", result.Tasks[0].Evaluation)
			}
			for _, call := range rig.log.snapshot() {
				if call == "benchmark.evaluate" {
					t.Fatal("Evaluate called after failed isolation gate")
				}
			}
		})
	}
}

func TestRunnerEvaluatesAfterHarnessStartFailureIsIsolated(t *testing.T) {
	rig := newRig(t, 1)
	startErr := errors.New("harness start failed")
	rig.harness.startErr = startErr

	result, err := rig.runner.Run(context.Background())
	if !errors.Is(err, startErr) {
		t.Fatalf("Run() error = %v, want harness start failure", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("task count = %d, want one", len(result.Tasks))
	}
	task := result.Tasks[0]
	if task.Harness.Status != core.StatusFailed || task.Harness.Error != startErr.Error() {
		t.Fatalf("harness result = %#v, want retained start failure", task.Harness)
	}
	if task.Isolation.Status != core.StatusConfirmed || !task.Isolation.HarnessStopped || !task.Isolation.BridgeRevoked {
		t.Fatalf("isolation = %#v, want both gates confirmed", task.Isolation)
	}
	if task.Evaluation.Status != core.StatusSucceeded || task.Evaluation.Reward != 1 {
		t.Fatalf("evaluation = %#v, want independent successful evaluation", task.Evaluation)
	}
	wantCalls := []string{
		"benchmark.tasks", "sandbox.start", "bridge.start", "harness.start", "harness.rollback",
		"harness.stop", "bridge.stop", "benchmark.evaluate", "sandbox.stop",
	}
	if got := rig.log.snapshot(); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("calls = %v, want %v", got, wantCalls)
	}
	if rig.harness.stopCount() != 1 || rig.bridge.stopCount() != 1 || rig.factory.stopCount() != 1 {
		t.Fatalf("stop counts harness=%d bridge=%d sandbox=%d, want one each", rig.harness.stopCount(), rig.bridge.stopCount(), rig.factory.stopCount())
	}
}

func TestRunnerBlocksEvaluationWhenFailedHarnessStartCannotBeStopped(t *testing.T) {
	rig := newRig(t, 1)
	startErr := errors.New("harness start failed")
	stopErr := errors.New("harness stop failed")
	rig.harness.startErr = startErr
	rig.harness.stopErrors = []error{stopErr, nil}

	result, err := rig.runner.Run(context.Background())
	if !errors.Is(err, startErr) || !errors.Is(err, stopErr) {
		t.Fatalf("Run() error = %v, want joined start and stop failures", err)
	}
	task := result.Tasks[0]
	if task.Harness.Status != core.StatusFailed || task.Harness.Error != startErr.Error() {
		t.Fatalf("harness result = %#v, want retained start failure", task.Harness)
	}
	if task.Isolation.Status != core.StatusBlockedIsolation || task.Isolation.HarnessStopped || !task.Isolation.BridgeRevoked {
		t.Fatalf("isolation = %#v, want failed harness gate and revoked bridge", task.Isolation)
	}
	if task.Evaluation.Status != core.StatusBlockedIsolation {
		t.Fatalf("evaluation = %#v, want blocked", task.Evaluation)
	}
	for _, call := range rig.log.snapshot() {
		if call == "harness.run" || call == "benchmark.evaluate" {
			t.Fatalf("unexpected call after failed harness start/isolation: %s", call)
		}
	}
	if rig.bridge.stopCount() != 1 || rig.factory.stopCount() != 1 {
		t.Fatalf("successful cleanup repeated: bridge=%d sandbox=%d, want one each", rig.bridge.stopCount(), rig.factory.stopCount())
	}
}

func TestRunnerAggregatesDistinctOutcomes(t *testing.T) {
	rig := newRig(t, 2)
	rig.harness.runErrors = []error{nil, errInjected}
	rig.benchmark.evaluationErrs = []error{nil, errors.New("verifier failed")}

	result, err := rig.runner.Run(context.Background())
	if !errors.Is(err, errInjected) {
		t.Fatalf("Run() error = %v, want harness failure", err)
	}
	want := core.RunSummary{
		Tasks:                2,
		HarnessSucceeded:     1,
		HarnessFailed:        1,
		EvaluationsRun:       2,
		EvaluationsSucceeded: 1,
		EvaluationsFailed:    1,
	}
	if !reflect.DeepEqual(result.Summary, want) {
		t.Fatalf("summary = %#v, want %#v", result.Summary, want)
	}
	if result.Tasks[1].Isolation.Status != core.StatusConfirmed || result.Tasks[1].Evaluation.Status != core.StatusFailed {
		t.Fatalf("mixed result collapsed: %#v", result.Tasks[1])
	}
}

func TestRunnerHasNoLifecycleStateBetweenRuns(t *testing.T) {
	rig := newRig(t, 1)
	for i := 0; i < 2; i++ {
		if _, err := rig.runner.Run(context.Background()); err != nil {
			t.Fatalf("Run() iteration %d error = %v", i, err)
		}
	}
	if rig.harness.stopCount() != 2 || rig.bridge.stopCount() != 2 || rig.factory.stopCount() != 2 {
		t.Fatalf("stop counts harness=%d bridge=%d sandbox=%d, want two each", rig.harness.stopCount(), rig.bridge.stopCount(), rig.factory.stopCount())
	}
}

func TestNewRejectsMissingRoles(t *testing.T) {
	rig := newRig(t, 0)
	tests := []struct {
		name      string
		benchmark Benchmark
		harness   AgentHarness
		sandbox   ToolSandbox
		bridge    ToolBridge
	}{
		{"benchmark", nil, rig.harness, rig.factory, rig.bridge},
		{"harness", rig.benchmark, nil, rig.factory, rig.bridge},
		{"sandbox", rig.benchmark, rig.harness, nil, rig.bridge},
		{"bridge", rig.benchmark, rig.harness, rig.factory, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.benchmark, test.harness, test.sandbox, test.bridge, Options{}); err == nil {
				t.Fatal("New() error = nil")
			}
		})
	}
}

func TestNewRejectsMissingRunID(t *testing.T) {
	rig := newRig(t, 0)
	if _, err := New(rig.benchmark, rig.harness, rig.factory, rig.bridge, Options{}); err == nil || !strings.Contains(err.Error(), "run ID") {
		t.Fatalf("New() error = %v, want missing run ID", err)
	}
}
