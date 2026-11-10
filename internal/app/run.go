package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sync"
	"time"

	bridgegrpc "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/replay"
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

// PreparedBenchmark keeps one run's selected catalog and creates fresh occurrence state.
type PreparedBenchmark struct {
	Tasks         []core.Task
	NewOccurrence func(logicalID, occurrenceID string) (runner.Benchmark, error)
}

type Wiring struct {
	recordForReplay       bool
	BenchmarkRevisions    func(config.Config) map[string]string
	NewInfrastructure     func(config.Config, string, string, bool, *logrus.Logger) (*RunInfrastructure, error)
	PrepareBridge         func(context.Context, config.Config) error
	PrepareBackend        func(config.Config, string) (PreparedBackend, error)
	ValidateComponents    func(config.Config) error
	SetupBenchmark        func(context.Context, config.Config) error
	LoadBenchmark         func(context.Context, config.Config, string, []string, func(string) ([]byte, bool)) (PreparedBenchmark, error)
	PullImages            func(context.Context, config.Config, []string) error
	BuildHarnessImage     func(context.Context, config.Config) error
	NewHarness            func(config.Config, string, func(string) ([]byte, bool), *logrus.Logger) (HarnessInstance, error)
	NewSandbox            func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error)
	NewBridge             func(config.Config, string, *logrus.Logger) (runner.ToolBridge, error)
	NewReplayCaller       func(config.Config, core.RuntimePlacement, time.Duration, *logrus.Logger) (runner.ReplayCaller, error)
	PreflightReplaySource func(context.Context, config.Config, []core.Task, *logrus.Entry) (*bridgegrpc.ReplaySelection, error)
	LoadReplaySource      func(context.Context, config.Config, core.Task) (replay.Plan, error)
	ReplayPlacement       core.RuntimePlacement
}

type Dependencies struct {
	// RecordForReplay selects live source recording; replay execution enables its own capture.
	RecordForReplay bool
	ExecutablePath  string
	PreflightClient httpDoer
	PreflightSleep  contextSleep
	Logger          *logrus.Logger
	Wiring          Wiring
}

func Setup(ctx context.Context, profilePath string, stdout io.Writer, dependencies Dependencies) error {
	if dependencies.RecordForReplay {
		return errors.New("--record-for-replay is not supported by setup")
	}
	cfg, err := config.Load(profilePath)
	if err != nil {
		return err
	}
	if err := validateWiredComponents(cfg, dependencies.Wiring); err != nil {
		return err
	}
	if cfg.Replay != nil {
		if _, err := ensurePrepared(ctx, cfg, cfg.OutputDir, dependencies.Wiring, nil); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "profile ready: %s\n", profilePath)
		return err
	}
	if dependencies.Wiring.PrepareBackend == nil {
		return errors.New("command wiring is incomplete")
	}
	prepared, err := dependencies.Wiring.PrepareBackend(cfg, cfg.OutputDir)
	if err != nil {
		return err
	}
	apiKeyLookup, releaseAPIKeys, err := resolveAPIKeyLookup(prepared.Model, dependencies.ExecutablePath)
	if err != nil {
		return fmt.Errorf("load local API key: %w", err)
	}
	defer releaseAPIKeys()
	if _, err := ensurePrepared(ctx, cfg, cfg.OutputDir, dependencies.Wiring, apiKeyLookup); err != nil {
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
	dependencies.Wiring.recordForReplay = dependencies.RecordForReplay
	if dependencies.RecordForReplay {
		if cfg.Replay != nil {
			return errors.New("--record-for-replay is not supported with a replay profile")
		}
		if cfg.Bridge.Type != "hermes-grpc" {
			return errors.New("recording requires bridge.type hermes-grpc")
		}
		if cfg.Execution.ArrivalsFile != "" {
			return errors.New("recording does not support an arrival schedule")
		}
	}
	if err := validateWiredComponents(cfg, dependencies.Wiring); err != nil {
		return err
	}
	runID := newRunID(time.Now(), cfg.Name)
	outputRoot, err := resolveOutputRoot(cfg.OutputDir, runID)
	if err != nil {
		return err
	}
	if cfg.Replay != nil {
		return runReplay(ctx, cfg, runID, outputRoot, stdout, dependencies.Wiring, logger)
	}
	if dependencies.Wiring.PrepareBackend == nil {
		return errors.New("backend preparer is required")
	}
	prepared, err := dependencies.Wiring.PrepareBackend(cfg, outputRoot)
	if err != nil {
		return err
	}
	apiKeyLookup, releaseAPIKeys, keyErr := resolveAPIKeyLookup(prepared.Model, dependencies.ExecutablePath)
	if keyErr != nil {
		validation := failedLiveValidation(prepared.Model, liveValidationCredentialInvalid, 0)
		errs := []error{fmt.Errorf("load local API key: %w", keyErr)}
		if mkErr := createRunOutputRoot(outputRoot); mkErr != nil {
			errs = append(errs, mkErr)
		} else if persistErr := persistLiveValidation(outputRoot, validation); persistErr != nil {
			errs = append(errs, persistErr)
		}
		return errors.Join(errs...)
	}
	defer releaseAPIKeys()
	preparationEntry := logger.WithFields(logrus.Fields{"run_id": runID, "profile": cfg.Name, "backend": cfg.Runtime.Backend, "mode": cfg.Runtime.Mode, "component": "preparation"})
	preparationEntry.WithField("preparation_state", "preparing").Info("profile preparation")
	catalog, err := ensurePrepared(ctx, cfg, outputRoot, dependencies.Wiring, apiKeyLookup)
	if err != nil {
		preparationEntry.WithFields(logrus.Fields{"preparation_state": "not_prepared", "error_category": "preparation_failed"}).Error("profile preparation")
		return err
	}
	if dependencies.RecordForReplay {
		resolvedIDs := make([]string, 0, len(catalog.Tasks))
		for _, task := range catalog.Tasks {
			resolvedIDs = append(resolvedIDs, task.ID)
		}
		for _, id := range cfg.Benchmark.Tasks {
			if !slices.Contains(resolvedIDs, id) {
				cfg.Benchmark.Tasks = resolvedIDs
				break
			}
		}
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

	preflightLookup := apiKeyLookup
	harnessLookup := apiKeyLookup

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
		healthErr := waitForRuntimeHealth(startupCtx, runtime, dependencies.PreflightSleep, func() {
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

	var schedule []arrival
	if cfg.Execution.ArrivalsFile != "" {
		loaded, err := config.LoadArrivals(cfg.Execution.ArrivalsFile, cfg.Execution.ArrivalRatePerMin, cfg.Benchmark.Tasks)
		if err != nil {
			return fmt.Errorf("load arrival schedule: %w", err)
		}
		for _, entry := range loaded {
			schedule = append(schedule, arrival{logicalID: entry.TaskID, at: entry.At})
		}
		logger.WithFields(logrus.Fields{"arrivals": len(schedule), "rate_per_min": cfg.Execution.ArrivalRatePerMin, "last_offset": schedule[len(schedule)-1].at.String()}).Info("open-loop arrival schedule loaded")
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	completed := make(chan error, 1)
	go func() {
		completed <- executeAndRecord(runCtx, func(executionCtx context.Context) (core.RunResult, error) {
			return runWithInfrastructure(executionCtx, cfg, runID, outputRoot, logger, dependencies.Wiring, func(executionCtx context.Context, taskWiring Wiring) (core.RunResult, error) {
				return runProfile(executionCtx, cfg.Name, runID, cfg.Benchmark.Tasks, cfg.Execution.Concurrency, cfg.Execution.Loop, schedule,
					func(taskCtx context.Context, occurrence taskOccurrence) (core.RunResult, error) {
						experiment, err := buildTaskExperiment(cfg, prepared.Model, prepared.EffectiveGPUIndices, runID, outputRoot, occurrence.logicalID, occurrence.executionID, harnessLookup, logger, catalog, taskWiring)
						if err != nil {
							return core.RunResult{}, err
						}
						return experiment.Run(taskCtx)
					})
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

func runReplay(ctx context.Context, cfg config.Config, runID, outputRoot string, stdout io.Writer, wiring Wiring, logger *logrus.Logger) (returnErr error) {
	catalog, err := ensurePrepared(ctx, cfg, outputRoot, wiring, nil)
	if err != nil {
		return err
	}
	if err := createRunOutputRoot(outputRoot); err != nil {
		return fmt.Errorf("create private run output root: %w", err)
	}
	detachRunLog, err := attachRunLog(logger, outputRoot)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, detachRunLog()) }()
	if wiring.PreflightReplaySource == nil {
		return errors.New("replay source preflight is required")
	}
	selection, err := wiring.PreflightReplaySource(ctx, cfg, catalog.Tasks, logger.WithField("run_id", runID))
	if err != nil {
		return err
	}
	return runWithReplayInfrastructure(ctx, cfg, runID, outputRoot, stdout, wiring, logger, catalog, selection)
}

type replayCaller struct {
	runner.ReplayCaller
	replayAt func() time.Time
	origin   *time.Time
}

// ReplayAt returns the epoch set by Run without starting an unused clock.
func (caller *replayCaller) ReplayAt() *time.Time { return caller.origin }

func (caller *replayCaller) Run(ctx context.Context, plan replay.Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(plan.Calls) != 0 {
		origin := caller.replayAt()
		caller.origin = &origin
		plan.ReplayAt = &origin
	}
	return caller.ReplayCaller.Run(ctx, plan)
}

func runWithReplayInfrastructure(ctx context.Context, cfg config.Config, runID, outputRoot string, stdout io.Writer, wiring Wiring, logger *logrus.Logger, catalog PreparedBenchmark, selection *bridgegrpc.ReplaySelection) error {
	taskIDs := make([]string, 0, len(selection.Occurrences))
	for _, occurrence := range selection.Occurrences {
		taskIDs = append(taskIDs, occurrence.LogicalTaskID)
	}
	return executeAndRecord(ctx, func(ctx context.Context) (core.RunResult, error) {
		return runWithInfrastructure(ctx, cfg, runID, outputRoot, logger, wiring, func(ctx context.Context, taskWiring Wiring) (core.RunResult, error) {
			if len(taskIDs) > 1 && taskWiring.NewReplayCaller != nil {
				replayAt := sync.OnceValue(func() time.Time { return time.Now().UTC() })
				newReplayCaller := taskWiring.NewReplayCaller
				taskWiring.NewReplayCaller = func(cfg config.Config, placement core.RuntimePlacement, timeout time.Duration, logger *logrus.Logger) (runner.ReplayCaller, error) {
					caller, err := newReplayCaller(cfg, placement, timeout, logger)
					if err != nil {
						return nil, err
					}
					return &replayCaller{ReplayCaller: caller, replayAt: replayAt}, nil
				}
			}
			pending := make([]struct {
				experiment *experiment
				result     core.RunResult
			}, len(taskIDs))
			result, runErr := runProfile(ctx, cfg.Name, runID, taskIDs, cfg.Execution.Concurrency, 0, nil,
				func(taskCtx context.Context, occurrence taskOccurrence) (core.RunResult, error) {
					index := occurrence.index
					occurrenceWiring := taskWiring
					occurrenceWiring.LoadReplaySource = func(ctx context.Context, _ config.Config, _ core.Task) (replay.Plan, error) {
						return selection.Load(ctx, index)
					}
					experiment, err := buildTaskExperiment(cfg, core.ModelConfig{}, nil, runID, outputRoot, selection.Occurrences[index].LogicalTaskID, occurrence.executionID, nil, logger, catalog, occurrenceWiring)
					if err != nil {
						return core.RunResult{}, err
					}
					result, err := experiment.Run(taskCtx)
					if errors.Is(err, errReplayCleanupUnconfirmed) {
						pending[index].experiment, pending[index].result = experiment, result
					}
					return result, err
				})
			retryStarted := time.Now()
			// Keep failed owners until admitted peers drain, before shared cleanup removes their attachment.
			for _, owner := range pending {
				if owner.experiment == nil {
					continue
				}
				previousFailures := owner.result.Summary.CleanupFailed
				runErr = errors.Join(runErr, owner.experiment.closeReplay(&owner.result))
				result.Summary.CleanupFailed += owner.result.Summary.CleanupFailed - previousFailures
				for _, task := range owner.result.Tasks {
					for index := range result.Tasks {
						if result.Tasks[index].TaskID == task.TaskID {
							result.Tasks[index] = task
							break
						}
					}
				}
			}
			result.Duration += time.Since(retryStarted)
			return result, runErr
		})
	}, outputRoot, stdout)
}

func ensurePrepared(ctx context.Context, cfg config.Config, outputRoot string, wiring Wiring, apiKeyLookup func(string) ([]byte, bool)) (PreparedBenchmark, error) {
	if wiring.SetupBenchmark == nil || wiring.LoadBenchmark == nil || wiring.PullImages == nil || cfg.Replay == nil && wiring.BuildHarnessImage == nil {
		return PreparedBenchmark{}, errors.New("preparation wiring is incomplete")
	}
	if wiring.PrepareBridge != nil {
		if err := wiring.PrepareBridge(ctx, cfg); err != nil {
			return PreparedBenchmark{}, err
		}
	}
	if err := wiring.SetupBenchmark(ctx, cfg); err != nil {
		return PreparedBenchmark{}, err
	}
	catalog, err := wiring.LoadBenchmark(ctx, cfg, outputRoot, uniqueStrings(cfg.Benchmark.Tasks), apiKeyLookup)
	if err != nil {
		return PreparedBenchmark{}, err
	}
	if wiring.recordForReplay {
		if len(catalog.Tasks) == 0 {
			return PreparedBenchmark{}, errors.New("recording requires at least one resolved selected task")
		}
		if wiring.BenchmarkRevisions == nil {
			return PreparedBenchmark{}, errors.New("recording requires source provenance")
		}
		for _, task := range catalog.Tasks {
			if task.ID == "" || task.Environment.Image == "" {
				return PreparedBenchmark{}, errors.New("recording requires source provenance")
			}
		}
		revisions := wiring.BenchmarkRevisions(cfg)
		if len(revisions) == 0 {
			return PreparedBenchmark{}, errors.New("recording requires source provenance")
		}
		for _, revision := range revisions {
			if revision == "" {
				return PreparedBenchmark{}, errors.New("recording requires source provenance")
			}
		}
	}
	images := make([]string, 0, len(catalog.Tasks)+1)
	if cfg.Replay == nil {
		harnessImage, err := cfg.Versions.HarnessImage(cfg.Harness.Type)
		if err != nil {
			return PreparedBenchmark{}, err
		}
		images = append(images, harnessImage)
	}
	for _, task := range catalog.Tasks {
		images = append(images, task.Environment.Image)
	}
	if err := wiring.PullImages(ctx, cfg, uniqueStrings(images)); err != nil {
		return PreparedBenchmark{}, err
	}
	if cfg.Replay == nil {
		if err := wiring.BuildHarnessImage(ctx, cfg); err != nil {
			return PreparedBenchmark{}, err
		}
	}
	return catalog, nil
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

func buildTaskExperiment(cfg config.Config, model core.ModelConfig, effectiveGPUIndices []int, runID, outputRoot, logicalID, occurrenceID string, apiKeyLookup func(string) ([]byte, bool), logger *logrus.Logger, catalog PreparedBenchmark, wiring Wiring) (*experiment, error) {
	if cfg.Replay != nil {
		return buildReplayTaskExperiment(cfg, runID, outputRoot, logicalID, occurrenceID, logger, catalog, wiring)
	}
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	if catalog.NewOccurrence == nil || wiring.NewHarness == nil || wiring.NewSandbox == nil || wiring.NewBridge == nil {
		return nil, errors.New("component wiring is incomplete")
	}
	benchmark, err := catalog.NewOccurrence(logicalID, occurrenceID)
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
		Name: cfg.Name, RunID: runID, OutputDir: outputRoot, Model: model, Logger: logger, RecordForReplay: wiring.recordForReplay,
		RuntimeOverrides: runner.RuntimeOverrides{
			HarnessResources:      runner.ResourceOverrides{CPU: cfg.Overrides.HarnessResources.CPU, MemoryMB: cfg.Overrides.HarnessResources.MemoryMB},
			AgentSandboxResources: runner.ResourceOverrides{CPU: cfg.Overrides.AgentSandboxResources.CPU, MemoryMB: cfg.Overrides.AgentSandboxResources.MemoryMB},
			AgentTimeout:          cfg.Overrides.AgentTimeout,
		},
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct runner: %w", err), sandbox.Resources.Close(), closeOccurrenceClients(sandbox.Close, harness.Close))
	}
	if provenance, ok := bridge.(interface{ RecordSource(string, string) }); ok && wiring.recordForReplay {
		for _, selected := range catalog.Tasks {
			if selected.ID == logicalID {
				provenance.RecordSource(logicalID, selected.Environment.Image)
				break
			}
		}
	}
	recorder, err := monitor.New(monitor.Options{RunID: runID, TaskIDs: []string{occurrenceID}, OutputDir: outputRoot, Source: sandbox.Resources, Logger: logger})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct monitor: %w", err), sandbox.Resources.Close(), closeOccurrenceClients(sandbox.Close, harness.Close))
	}
	return &experiment{runner: benchmarkRunner, recorder: recorder, close: func() error { return closeOccurrenceClients(sandbox.Close, harness.Close) }}, nil
}

func buildReplayTaskExperiment(cfg config.Config, runID, outputRoot, logicalID, occurrenceID string, logger *logrus.Logger, catalog PreparedBenchmark, wiring Wiring) (*experiment, error) {
	if catalog.NewOccurrence == nil || wiring.NewSandbox == nil || wiring.NewBridge == nil || wiring.NewReplayCaller == nil || wiring.LoadReplaySource == nil {
		return nil, errors.New("replay component wiring is incomplete")
	}
	var selected core.Task
	found := false
	for _, task := range catalog.Tasks {
		if task.ID == logicalID {
			selected, found = task, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("replay catalog has no logical task %q", logicalID)
	}
	benchmark, err := catalog.NewOccurrence(logicalID, occurrenceID)
	if err != nil {
		return nil, err
	}
	sandbox, err := wiring.NewSandbox(cfg, outputRoot, runID, occurrenceID, nil, logger)
	if err != nil {
		return nil, err
	}
	closeSandbox := func() error {
		return errors.Join(sandbox.Resources.Close(), closeOccurrenceClients(sandbox.Close, nil))
	}
	bridge, err := wiring.NewBridge(cfg, outputRoot, logger)
	if err != nil {
		return nil, errors.Join(err, closeSandbox())
	}
	if provenance, ok := bridge.(interface{ RecordSource(string, string) }); ok {
		provenance.RecordSource(logicalID, selected.Environment.Image)
	}
	timeout := selected.Timeout
	if cfg.Overrides.AgentTimeout != nil {
		timeout = *cfg.Overrides.AgentTimeout
	}
	caller, err := wiring.NewReplayCaller(cfg, wiring.ReplayPlacement, timeout, logger)
	if err != nil {
		return nil, errors.Join(err, closeSandbox())
	}
	closeCaller := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		return caller.Stop(cleanupCtx)
	}
	benchmarkRunner, err := runner.NewReplay(benchmark, sandbox.Sandbox, bridge, caller,
		func(ctx context.Context) (replay.Plan, error) {
			plan, err := wiring.LoadReplaySource(ctx, cfg, selected)
			return plan, err
		},
		runner.Options{Name: cfg.Name, RunID: runID, OutputDir: outputRoot, Logger: logger,
			RuntimeOverrides: runner.RuntimeOverrides{AgentSandboxResources: runner.ResourceOverrides{
				CPU: cfg.Overrides.AgentSandboxResources.CPU, MemoryMB: cfg.Overrides.AgentSandboxResources.MemoryMB,
			}}})
	if err != nil {
		return nil, errors.Join(err, closeCaller(), closeSandbox())
	}
	recorder, err := monitor.New(monitor.Options{RunID: runID, TaskIDs: []string{occurrenceID}, OutputDir: outputRoot, Source: sandbox.Resources, Logger: logger})
	if err != nil {
		return nil, errors.Join(err, closeCaller(), closeSandbox())
	}
	return &experiment{runner: benchmarkRunner, recorder: recorder, closeReplay: func(result *core.RunResult) error {
		cleanupErr := benchmarkRunner.RetryReplayCleanup(context.Background())
		if runner.CleanupConfirmed(cleanupErr) {
			cleanupErr = errors.Join(cleanupErr, closeCaller(), closeOccurrenceClients(sandbox.Close, nil))
		}
		for index := range result.Tasks {
			task := &result.Tasks[index]
			wasFailed := task.Cleanup.Status == core.StatusFailed
			switch {
			case cleanupErr != nil:
				if !wasFailed {
					result.Summary.CleanupFailed++
				}
				task.Cleanup.Status = core.StatusFailed
				task.Cleanup.Error = cleanupErr.Error()
				if task.Error != "" {
					task.Error = errors.Join(errors.New(task.Error), cleanupErr).Error()
				} else {
					task.Error = cleanupErr.Error()
				}
			case wasFailed:
				// The retry confirms release; the task error retains the first failure.
				task.Cleanup.Status = core.StatusSucceeded
				task.Cleanup.Error = ""
				result.Summary.CleanupFailed--
			}
		}
		if !runner.CleanupConfirmed(cleanupErr) {
			return fmt.Errorf("%w: %w", errReplayCleanupUnconfirmed, cleanupErr)
		}
		return cleanupErr
	}}, nil
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
