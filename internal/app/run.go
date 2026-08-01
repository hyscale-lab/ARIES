package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

type HarnessInstance struct {
	Harness runner.AgentHarness
	Close   func() error
}

type SandboxInstance struct {
	Sandbox   runner.ToolSandbox
	Resources monitor.ResourceSource
	Close     func() error
}

type Wiring struct {
	PrepareBackend       func(config.Config, string) (PreparedBackend, error)
	ValidateComponents   func(config.Config) error
	SetupBenchmark       func(context.Context, config.Config) error
	LoadPreparationTasks func(context.Context, config.Config, []string) ([]core.Task, error)
	PullImages           func(context.Context, []string) error
	NewBenchmark         func(config.Config, string, string, string) (runner.Benchmark, error)
	NewHarness           func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error)
	NewSandbox           func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error)
	NewBridge            func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error)
}

type Dependencies struct {
	ExecutablePath  string
	PreflightClient httpDoer
	PreflightSleep  contextSleep
	Logger          *logrus.Logger
	Wiring          Wiring
}

func Setup(ctx context.Context, profilePath string, stdout io.Writer, dependencies Dependencies) error {
	cfg, err := config.Load(profilePath)
	if err != nil {
		return err
	}
	if err := validateWiredComponents(cfg, dependencies.Wiring); err != nil {
		return err
	}
	if dependencies.Wiring.PrepareBackend == nil {
		return errors.New("command wiring is incomplete")
	}
	if _, err := dependencies.Wiring.PrepareBackend(cfg, cfg.OutputDir); err != nil {
		return err
	}
	if err := ensurePrepared(ctx, cfg, dependencies.Wiring); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "profile ready: %s\n", profilePath)
	return err
}

func Run(ctx context.Context, profilePath string, stdout io.Writer, dependencies Dependencies) (returnErr error) {
	logger := dependencies.Logger
	if logger == nil {
		logger = newLogger()
	}
	cfg, err := config.Load(profilePath)
	if err != nil {
		return err
	}
	if err := validateWiredComponents(cfg, dependencies.Wiring); err != nil {
		return err
	}
	runID := newRunID(time.Now(), cfg.Name)
	outputRoot, err := resolveOutputRoot(cfg.OutputDir, runID)
	if err != nil {
		return err
	}
	if dependencies.Wiring.PrepareBackend == nil {
		return errors.New("backend preparer is required")
	}
	prepared, err := dependencies.Wiring.PrepareBackend(cfg, outputRoot)
	if err != nil {
		return err
	}
	preparationEntry := logger.WithFields(logrus.Fields{"run_id": runID, "profile": cfg.Name, "backend": cfg.Runtime.Backend, "mode": cfg.Runtime.Mode, "component": "preparation"})
	preparationEntry.WithField("preparation_state", "preparing").Info("profile preparation")
	if err := ensurePrepared(ctx, cfg, dependencies.Wiring); err != nil {
		preparationEntry.WithFields(logrus.Fields{"preparation_state": "not_prepared", "error_category": "preparation_failed"}).Error("profile preparation")
		return err
	}
	preparationEntry.WithField("preparation_state", "prepared").Info("profile preparation")
	if err := createRunOutputRoot(outputRoot); err != nil {
		return fmt.Errorf("create private run output root: %w", err)
	}
	detachRunLog, err := attachRunLog(logger, outputRoot)
	if err != nil {
		return err
	}
	runEntry := logger.WithFields(logrus.Fields{"run_id": runID, "profile": cfg.Name, "output_dir": outputRoot, "backend": cfg.Runtime.Backend, "mode": cfg.Runtime.Mode})
	runEntry.Info("experiment run started")
	defer func() {
		if returnErr != nil {
			runEntry.WithField("error_category", "run_failed").Error("experiment run finished with errors")
		} else {
			runEntry.Info("experiment run finished")
		}
		returnErr = errors.Join(returnErr, detachRunLog())
	}()

	preflightLookup := environmentAPIKeyLookup
	var harnessLookup func(string) ([]byte, bool)
	if isOfficialDeepSeek(prepared.Model) {
		if keyPath, anchored := repositoryAPIKeyPath(dependencies.ExecutablePath); anchored {
			apiKeys, exists, keyErr := loadLocalAPIKeySource(keyPath)
			if keyErr != nil {
				validation := failedLiveValidation(prepared.Model, liveValidationCredentialInvalid, 0)
				return errors.Join(fmt.Errorf("load local API key: %w", keyErr), persistLiveValidation(outputRoot, validation))
			}
			if exists {
				defer apiKeys.Clear()
				preflightLookup, harnessLookup = apiKeys.Lookup, apiKeys.Lookup
			}
		}
	}

	runtime := prepared.Runtime
	runtimeEntry := runEntry.WithField("component", "model_runtime")
	runtimeExitLogged := false
	logRuntimeExit := func(runtimeErr error) {
		if runtimeErr == nil {
			return
		}
		if !runtimeExitLogged {
			runtimeEntry.WithFields(logrus.Fields{"runtime_state": "unexpected_exit", "error_category": "runtime_exit"}).Error("model runtime lifecycle")
			runtimeExitLogged = true
		}
	}
	if runtime != nil {
		if err := runtime.Start(ctx); err != nil {
			return fmt.Errorf("start model runtime: %w", err)
		}
		runtimeEntry.WithField("runtime_state", "started").Info("model runtime lifecycle")
		runtimeEntry.WithField("runtime_state", "loading").Info("model runtime lifecycle")
		defer func() {
			stopErr := stopRuntime(runtime, cfg.Runtime.Config.StopTimeout)
			logRuntimeExit(runtime.Err())
			if runtimeCleanupFailed(stopErr) {
				runtimeEntry.WithFields(logrus.Fields{"runtime_state": "stop_failed", "error_category": "runtime_stop"}).Error("model runtime lifecycle")
			} else {
				runtimeEntry.WithField("runtime_state", "stopped").Info("model runtime lifecycle")
			}
			returnErr = errors.Join(returnErr, stopErr)
		}()
		startupCtx, cancel := context.WithTimeout(ctx, cfg.Runtime.Config.StartupTimeout)
		healthErr := waitForRuntimeHealthObserved(startupCtx, runtime, dependencies.PreflightSleep, func() {
			runtimeEntry.WithFields(logrus.Fields{"runtime_state": "loading", "error_category": "retryable_health_failure"}).Info("model runtime lifecycle")
		})
		cancel()
		if healthErr != nil {
			runtimeErr, _ := runtimeExitIfDone(runtime)
			logRuntimeExit(runtimeErr)
			if runtimeErr == nil {
				runtimeEntry.WithFields(logrus.Fields{"runtime_state": "terminal_configuration_or_model_mismatch", "error_category": "runtime_health"}).Error("model runtime lifecycle")
			}
			return fmt.Errorf("wait for model runtime health: %w", errors.Join(healthErr, runtimeErr))
		}
		runtimeEntry.WithField("runtime_state", "healthy").Info("model runtime lifecycle")
	}

	preflightCtx, cancelPreflight := context.WithCancel(ctx)
	type preflightResult struct {
		validation liveValidation
		err        error
	}
	preflightDone := make(chan preflightResult, 1)
	go func() {
		validation, err := validateLiveModel(preflightCtx, prepared.Model, preflightLookup, dependencies.PreflightClient, dependencies.PreflightSleep)
		preflightDone <- preflightResult{validation: validation, err: err}
	}()
	var preflight preflightResult
	if runtime == nil {
		preflight = <-preflightDone
	} else {
		var runtimeErr error
		preflight, runtimeErr = awaitRuntimeCompletion(runtime, preflightDone, cancelPreflight)
		logRuntimeExit(runtimeErr)
		preflight.err = errors.Join(preflight.err, runtimeErr)
	}
	cancelPreflight()
	validation, preflightErr := preflight.validation, preflight.err
	persistErr := persistLiveValidation(outputRoot, validation)
	if preflightErr != nil {
		runtimeEntry.WithFields(logrus.Fields{"runtime_state": "terminal_configuration_or_model_mismatch", "error_category": "model_preflight"}).Error("model runtime lifecycle")
	}
	if preflightErr != nil || persistErr != nil {
		return errors.Join(preflightErr, persistErr)
	}
	if runtime == nil {
		runtimeEntry.WithField("runtime_state", "healthy").Info("model runtime lifecycle")
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	completed := make(chan error, 1)
	go func() {
		completed <- executeAndRecord(runCtx, func(executionCtx context.Context) (core.RunResult, error) {
			return runProfile(executionCtx, cfg.Name, runID, cfg.Benchmark.Tasks, cfg.Execution.Concurrency, cfg.Execution.Loop,
				func(taskCtx context.Context, occurrence taskOccurrence) (core.RunResult, error) {
					experiment, err := buildTaskExperiment(cfg, prepared.Model, prepared.EffectiveGPUIndices, runID, outputRoot, occurrence.logicalID, occurrence.executionID, harnessLookup, logger, dependencies.Wiring)
					if err != nil {
						return core.RunResult{}, err
					}
					return experiment.Run(taskCtx)
				})
		}, outputRoot, stdout)
	}()
	if runtime == nil {
		return <-completed
	}
	var runtimeErr error
	returnErr, runtimeErr = awaitRuntimeCompletion(runtime, completed, cancelRun)
	logRuntimeExit(runtimeErr)
	returnErr = errors.Join(returnErr, runtimeErr)
	return returnErr
}

func ensurePrepared(ctx context.Context, cfg config.Config, wiring Wiring) error {
	if wiring.SetupBenchmark == nil || wiring.LoadPreparationTasks == nil || wiring.PullImages == nil {
		return errors.New("preparation wiring is incomplete")
	}
	if err := wiring.SetupBenchmark(ctx, cfg); err != nil {
		return err
	}
	tasks, err := wiring.LoadPreparationTasks(ctx, cfg, uniqueStrings(cfg.Benchmark.Tasks))
	if err != nil {
		return err
	}
	images := []string{cfg.Versions.OpenClaw.Image}
	for _, task := range tasks {
		images = append(images, task.Environment.Image)
	}
	return wiring.PullImages(ctx, uniqueStrings(images))
}

func validateWiredComponents(cfg config.Config, wiring Wiring) error {
	if wiring.ValidateComponents == nil {
		return errors.New("component validator is required")
	}
	return wiring.ValidateComponents(cfg)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			unique = append(unique, value)
		}
	}
	return unique
}

func resolveOutputRoot(root, runID string) (string, error) {
	outputRoot, err := filepath.Abs(filepath.Join(root, runID))
	if err != nil {
		return "", fmt.Errorf("resolve run output root: %w", err)
	}
	return outputRoot, nil
}

func buildTaskExperiment(cfg config.Config, model core.ModelConfig, effectiveGPUIndices []int, runID, outputRoot, logicalID, occurrenceID string, apiKeyLookup func(string) ([]byte, bool), logger *logrus.Logger, wiring Wiring) (*experiment, error) {
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	if wiring.NewBenchmark == nil || wiring.NewHarness == nil || wiring.NewSandbox == nil || wiring.NewBridge == nil {
		return nil, errors.New("component wiring is incomplete")
	}
	benchmark, err := wiring.NewBenchmark(cfg, outputRoot, logicalID, occurrenceID)
	if err != nil {
		return nil, err
	}
	harness, err := wiring.NewHarness(cfg, outputRoot, apiKeyLookup, logger)
	if err != nil {
		return nil, err
	}
	sandbox, err := wiring.NewSandbox(cfg, outputRoot, runID, occurrenceID, append([]int(nil), effectiveGPUIndices...), logger)
	if err != nil {
		return nil, errors.Join(err, closeOccurrenceClients(nil, harness.Close))
	}
	bridge, err := wiring.NewBridge(cfg, outputRoot, logger)
	if err != nil {
		return nil, errors.Join(err, sandbox.Resources.Close(), closeOccurrenceClients(sandbox.Close, harness.Close))
	}
	benchmarkRunner, err := runner.New(benchmark, harness.Harness, sandbox.Sandbox, bridge, runner.Options{
		Name: cfg.Name, RunID: runID, OutputDir: outputRoot, Model: model, Logger: logger,
		RuntimeOverrides: runner.RuntimeOverrides{
			HarnessResources:      runner.ResourceOverrides{CPU: cfg.Overrides.HarnessResources.CPU, MemoryMB: cfg.Overrides.HarnessResources.MemoryMB},
			AgentSandboxResources: runner.ResourceOverrides{CPU: cfg.Overrides.AgentSandboxResources.CPU, MemoryMB: cfg.Overrides.AgentSandboxResources.MemoryMB},
			AgentTimeout:          cfg.Overrides.AgentTimeout,
		},
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct runner: %w", err), sandbox.Resources.Close(), closeOccurrenceClients(sandbox.Close, harness.Close))
	}
	recorder, err := monitor.New(monitor.Options{RunID: runID, TaskIDs: []string{occurrenceID}, OutputDir: outputRoot, Source: sandbox.Resources, Logger: logger})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct monitor: %w", err), sandbox.Resources.Close(), closeOccurrenceClients(sandbox.Close, harness.Close))
	}
	return &experiment{runner: benchmarkRunner, recorder: recorder, close: func() error { return closeOccurrenceClients(sandbox.Close, harness.Close) }}, nil
}

func closeOccurrenceClients(sandboxClose, harnessClose func() error) error {
	var errs []error
	if sandboxClose != nil {
		errs = append(errs, sandboxClose())
	}
	if harnessClose != nil {
		errs = append(errs, harnessClose())
	}
	return errors.Join(errs...)
}
