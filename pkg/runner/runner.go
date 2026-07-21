package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

const defaultCleanupTimeout = 30 * time.Second

// Options contains the few experiment-level inputs needed by the Runner.
type Options struct {
	Name           string
	OutputDir      string
	Model          core.ModelConfig
	CleanupTimeout time.Duration
	Logger         *slog.Logger
}

// Runner composes exactly the benchmark, harness, sandbox, and bridge roles.
type Runner struct {
	benchmark      Benchmark
	harness        AgentHarness
	toolSandbox    ToolSandbox
	bridge         ToolBridge
	name           string
	outputDir      string
	model          core.ModelConfig
	cleanupTimeout time.Duration
	logger         *slog.Logger
}

// New constructs a Runner explicitly and rejects missing roles.
func New(benchmark Benchmark, harness AgentHarness, toolSandbox ToolSandbox, bridge ToolBridge, options Options) (*Runner, error) {
	if benchmark == nil {
		return nil, errors.New("benchmark is required")
	}
	if harness == nil {
		return nil, errors.New("agent harness is required")
	}
	if toolSandbox == nil {
		return nil, errors.New("tool sandbox is required")
	}
	if bridge == nil {
		return nil, errors.New("tool bridge is required")
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Runner{
		benchmark:      benchmark,
		harness:        harness,
		toolSandbox:    toolSandbox,
		bridge:         bridge,
		name:           options.Name,
		outputDir:      options.OutputDir,
		model:          options.Model,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
	}, nil
}

// Run executes tasks sequentially. A task failure is recorded without hiding
// later independent task results unless the run context itself is cancelled.
func (r *Runner) Run(ctx context.Context) (core.RunResult, error) {
	started := time.Now()
	result := core.RunResult{Name: r.name}

	tasks, err := r.benchmark.Tasks(ctx)
	if err != nil {
		result.Duration = time.Since(started)
		return result, fmt.Errorf("list benchmark tasks: %w", err)
	}

	var runErrors []error
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			runErrors = append(runErrors, fmt.Errorf("schedule task %q: %w", task.ID, err))
			break
		}
		taskResult, taskErr := r.runTask(ctx, task)
		result.Tasks = append(result.Tasks, taskResult)
		if taskErr != nil {
			runErrors = append(runErrors, fmt.Errorf("task %q: %w", task.ID, taskErr))
		}
	}

	result.Summary = summarize(result.Tasks)
	result.Duration = time.Since(started)
	return result, errors.Join(runErrors...)
}

func (r *Runner) runTask(ctx context.Context, task core.Task) (core.TaskResult, error) {
	started := time.Now()
	result := newTaskResult(task.ID)
	r.logger.InfoContext(ctx, "task started", "task_id", task.ID)

	var (
		sandbox       Sandbox
		sandboxActive bool
		bridgeActive  bool
		harnessActive bool
		allErrors     []error
		cleanupErrors []error
		cleanupCtx    context.Context
		cleanupCancel context.CancelFunc
		cleanupUsed   bool
	)
	defer func() {
		if cleanupCancel != nil {
			cleanupCancel()
		}
	}()
	ensureCleanupContext := func() context.Context {
		if cleanupCtx == nil {
			cleanupCtx, cleanupCancel = context.WithTimeout(context.WithoutCancel(ctx), r.cleanupTimeout)
		}
		return cleanupCtx
	}
	finish := func() (core.TaskResult, error) {
		cleanup := ensureCleanupContext()
		if harnessActive {
			cleanupUsed = true
			if err := r.harness.Stop(cleanup); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup harness: %w", err))
			} else {
				harnessActive = false
			}
		}
		if bridgeActive {
			cleanupUsed = true
			if err := r.bridge.Stop(cleanup); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup bridge: %w", err))
			} else {
				bridgeActive = false
			}
		}
		if sandboxActive {
			cleanupUsed = true
			if err := sandbox.Stop(cleanup); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("cleanup sandbox: %w", err))
			} else {
				sandboxActive = false
			}
		}
		if !cleanupUsed {
			result.Cleanup.Status = core.StatusNotNeeded
		} else if len(cleanupErrors) == 0 {
			result.Cleanup.Status = core.StatusSucceeded
		} else {
			result.Cleanup.Status = core.StatusFailed
			result.Cleanup.Error = errors.Join(cleanupErrors...).Error()
			allErrors = append(allErrors, cleanupErrors...)
		}
		if runContextErr := ctx.Err(); runContextErr != nil && !errors.Is(errors.Join(allErrors...), runContextErr) {
			allErrors = append(allErrors, fmt.Errorf("run context: %w", runContextErr))
		}
		result.Duration = time.Since(started)
		r.logger.InfoContext(context.WithoutCancel(ctx), "task finished", "task_id", task.ID, "harness_status", result.Harness.Status, "evaluation_status", result.Evaluation.Status, "cleanup_status", result.Cleanup.Status)
		return result, errors.Join(allErrors...)
	}

	var err error
	sandbox, err = r.toolSandbox.Start(ctx, task.Environment)
	if err != nil {
		allErrors = append(allErrors, fmt.Errorf("start sandbox: %w", err))
		return finish()
	}
	if sandbox == nil {
		allErrors = append(allErrors, errors.New("start sandbox: returned a nil sandbox"))
		return finish()
	}
	sandboxActive = true

	endpoint, err := r.bridge.Start(ctx, sandbox)
	if err != nil {
		allErrors = append(allErrors, fmt.Errorf("start bridge: %w", err))
		return finish()
	}
	bridgeActive = true

	err = r.harness.Start(ctx, core.HarnessRequest{
		TaskID:    task.ID,
		Endpoint:  endpoint,
		Model:     r.model,
		OutputDir: r.outputDir,
	})
	if err != nil {
		result.Harness.Status = failureStatus(err)
		result.Harness.Error = err.Error()
		allErrors = append(allErrors, fmt.Errorf("start harness: %w", err))
		return finish()
	}
	harnessActive = true

	harnessResult, runErr := r.harness.Run(ctx, task.Instruction)
	result.Harness = harnessResult
	if runErr == nil {
		if result.Harness.Status == "" {
			result.Harness.Status = core.StatusSucceeded
		}
	} else {
		result.Harness.Status = failureStatus(runErr)
		result.Harness.Error = runErr.Error()
		allErrors = append(allErrors, fmt.Errorf("run harness: %w", runErr))
	}

	isolationErrors := make([]error, 0, 2)
	isolationCtx, cancelIsolation := context.WithTimeout(context.WithoutCancel(ctx), r.cleanupTimeout)
	if err := r.harness.Stop(isolationCtx); err != nil {
		isolationErrors = append(isolationErrors, fmt.Errorf("confirm harness stopped: %w", err))
	} else {
		harnessActive = false
		result.Isolation.HarnessStopped = true
	}
	if err := r.bridge.Stop(isolationCtx); err != nil {
		isolationErrors = append(isolationErrors, fmt.Errorf("confirm bridge revoked: %w", err))
	} else {
		bridgeActive = false
		result.Isolation.BridgeRevoked = true
	}
	cancelIsolation()

	if len(isolationErrors) != 0 {
		isolationErr := errors.Join(isolationErrors...)
		result.Isolation.Status = core.StatusBlockedIsolation
		result.Isolation.Error = isolationErr.Error()
		result.Evaluation.Status = core.StatusBlockedIsolation
		result.Evaluation.Error = "evaluation blocked because harness termination or bridge revocation was not confirmed"
		allErrors = append(allErrors, isolationErrors...)
		return finish()
	}

	result.Isolation.Status = core.StatusConfirmed
	evaluation, evaluationErr := r.benchmark.Evaluate(ctx, task, sandbox)
	result.Evaluation = evaluation
	if evaluationErr == nil {
		if result.Evaluation.Status == "" {
			result.Evaluation.Status = core.StatusSucceeded
		}
	} else {
		result.Evaluation.Status = failureStatus(evaluationErr)
		result.Evaluation.Error = evaluationErr.Error()
		allErrors = append(allErrors, fmt.Errorf("evaluate sandbox: %w", evaluationErr))
	}

	return finish()
}

func newTaskResult(taskID string) core.TaskResult {
	return core.TaskResult{
		TaskID:     taskID,
		Harness:    core.HarnessResult{Status: core.StatusNotStarted},
		Isolation:  core.IsolationResult{Status: core.StatusNotStarted},
		Evaluation: core.Evaluation{Status: core.StatusNotRun},
		Observer:   core.ObserverResult{Status: core.StatusNotEnabled},
		Cleanup:    core.CleanupResult{Status: core.StatusNotNeeded},
	}
}

func failureStatus(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return core.StatusCanceled
	}
	return core.StatusFailed
}

func summarize(tasks []core.TaskResult) core.RunSummary {
	var summary core.RunSummary
	summary.Tasks = len(tasks)
	for _, task := range tasks {
		if task.Harness.Status == core.StatusSucceeded {
			summary.HarnessSucceeded++
		} else if task.Harness.Status == core.StatusFailed || task.Harness.Status == core.StatusCanceled {
			summary.HarnessFailed++
		}
		switch task.Evaluation.Status {
		case core.StatusSucceeded:
			summary.EvaluationsRun++
			summary.EvaluationsSucceeded++
		case core.StatusFailed, core.StatusCanceled:
			summary.EvaluationsRun++
			summary.EvaluationsFailed++
		case core.StatusBlockedIsolation:
			summary.EvaluationsBlocked++
		}
		if task.Cleanup.Status == core.StatusFailed {
			summary.CleanupFailed++
		}
	}
	return summary
}
