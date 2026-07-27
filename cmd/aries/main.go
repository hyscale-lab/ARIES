package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
)

const runResultName = "run-result.json"

func main() {
	logger := newLogger()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCommandWithDependencies(ctx, os.Args[1:], os.Stdout, commandDependencies{logger: logger}); err != nil {
		logger.WithError(err).Error("aries failed")
		os.Exit(1)
	}
}

type commandDependencies struct {
	executablePath  string
	preflightClient httpDoer
	preflightSleep  contextSleep
	prepareProfile  func(context.Context, config.Config) error
	modelRuntime    modelRuntime
	logger          *logrus.Logger
}

func runCommandWithDependencies(ctx context.Context, args []string, stdout io.Writer, dependencies commandDependencies) (returnErr error) {
	logger := dependencies.logger
	if logger == nil {
		logger = newLogger()
	}
	if len(args) == 2 && args[0] == "setup" {
		cfg, err := config.Load(args[1])
		if err != nil {
			return err
		}
		prepare := dependencies.prepareProfile
		if prepare == nil {
			prepare = prepareProfile
		}
		if err := prepare(ctx, cfg); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "profile ready: %s\n", args[1])
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: aries PROFILE.json | aries setup PROFILE.json")
	}
	cfg, err := config.Load(args[0])
	if err != nil {
		return err
	}
	runID := newRunID(time.Now(), cfg.Name)
	outputRoot, err := filepath.Abs(filepath.Join(cfg.OutputDir, runID))
	if err != nil {
		return fmt.Errorf("resolve run output root: %w", err)
	}
	if err := createRunOutputRoot(outputRoot); err != nil {
		return fmt.Errorf("create private run output root: %w", err)
	}
	detachRunLog, err := attachRunLog(logger, outputRoot)
	if err != nil {
		return err
	}
	runEntry := logger.WithFields(logrus.Fields{"run_id": runID, "profile": cfg.Name, "output_dir": outputRoot})
	runEntry.Info("experiment run started")
	defer func() {
		if returnErr != nil {
			runEntry.WithError(returnErr).Error("experiment run finished with errors")
		} else {
			runEntry.Info("experiment run finished")
		}
		if closeErr := detachRunLog(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close run log: %w", closeErr))
		}
	}()
	var apiKeys *apiKeySource
	exists := false
	if isOfficialDeepSeek(cfg.Model) {
		if keyPath, anchored := repositoryAPIKeyPath(dependencies.executablePath); anchored {
			apiKeys, exists, err = loadLocalAPIKeySource(keyPath)
			if err != nil {
				validation := failedLiveValidation(cfg.Model, liveValidationCredentialInvalid, 0)
				persistErr := persistLiveValidation(outputRoot, validation)
				return errors.Join(fmt.Errorf("load local API key: %w", err), persistErr)
			}
			if exists {
				defer apiKeys.Clear()
			}
		}
	}

	preflightLookup := environmentAPIKeyLookup
	var harnessLookup func(string) ([]byte, bool)
	if exists {
		preflightLookup = apiKeys.Lookup
		harnessLookup = apiKeys.Lookup
	}
	activeModelRuntime := dependencies.modelRuntime
	if activeModelRuntime == nil {
		switch cfg.ModelRuntime.Mode {
		case "external":
			activeModelRuntime = externalModelRuntime{}
		case "managed":
			switch cfg.Model.Provider {
			case "sglang":
				activeModelRuntime, err = newSGLangProcessRuntime(sglangProcessOptions{
					Executable: cfg.ModelRuntime.Executable,
					ConfigPath: cfg.SGLangPath,
					OutputDir:  outputRoot,
				})
			default:
				err = fmt.Errorf("unsupported managed model provider %q", cfg.Model.Provider)
			}
		default:
			err = fmt.Errorf("unsupported model runtime mode %q", cfg.ModelRuntime.Mode)
		}
		if err != nil {
			return err
		}
	}
	if err := activeModelRuntime.Start(ctx); err != nil {
		return fmt.Errorf("start model runtime: %w", err)
	}
	if cfg.ModelRuntime.Mode == "managed" {
		runEntry.Info("managed model runtime started")
	}
	stopTimeout := cfg.ModelRuntime.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = time.Second
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		stopErr := activeModelRuntime.Stop(cleanupCtx)
		cancel()
		if stopErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("stop model runtime: %w", stopErr))
		} else if cfg.ModelRuntime.Mode == "managed" {
			runEntry.Info("managed model runtime stopped")
		}
	}()

	validation, preflightErr := validateLiveModelForRuntime(
		ctx, cfg, activeModelRuntime, preflightLookup,
		dependencies.preflightClient, dependencies.preflightSleep,
	)
	persistErr := persistLiveValidation(outputRoot, validation)
	if preflightErr != nil || persistErr != nil {
		return errors.Join(preflightErr, persistErr)
	}

	returnErr = executeAndRecord(ctx, func(runCtx context.Context) (core.RunResult, error) {
		return runProfile(runCtx, cfg.Name, runID, cfg.Benchmark.Tasks, cfg.Execution.Concurrency, cfg.Execution.Loop,
			func(taskCtx context.Context, occurrence taskOccurrence) (core.RunResult, error) {
				experiment, err := buildTaskExperiment(cfg, runID, outputRoot, occurrence.logicalID, occurrence.executionID, harnessLookup, logger)
				if err != nil {
					return core.RunResult{}, err
				}
				return experiment.Run(taskCtx)
			})
	}, outputRoot, stdout)
	return returnErr
}

func buildTaskExperiment(
	cfg config.Config,
	runID, outputRoot, logicalID, occurrenceID string,
	apiKeyLookup func(string) ([]byte, bool),
	logger *logrus.Logger,
) (*experiment, error) {
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ARIES executable: %w", err)
	}
	binDir := filepath.Dir(executable)
	var benchmark runner.Benchmark
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		var executionIDs []string
		if occurrenceID != logicalID {
			executionIDs = []string{occurrenceID}
		}
		benchmark, err = terminalbench.New(terminalbench.Options{
			Root: cfg.Benchmark.Root, TaskIDs: []string{logicalID}, ExecutionTaskIDs: executionIDs, OutputDir: outputRoot,
			Revision: cfg.Versions.TerminalBench2.Revision,
		})
		if err != nil {
			return nil, fmt.Errorf("construct terminalbench2 benchmark: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
	var harness runner.AgentHarness
	var harnessManager *openclawharness.Manager
	switch cfg.Harness.Type {
	case "openclaw":
		harnessManager, err = openclawharness.New(openclawharness.Options{
			Image: cfg.Versions.OpenClaw.Image, OutputDir: outputRoot, APIKeyLookup: apiKeyLookup, Logger: logger,
		})
		if err != nil {
			return nil, fmt.Errorf("construct OpenClaw harness: %w", err)
		}
		harness = harnessManager
	default:
		return nil, fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
	var sandbox runner.ToolSandbox
	var sandboxManager *dockersandbox.Manager
	var resourceSource monitor.ResourceSource
	switch cfg.Sandbox.Type {
	case "docker":
		sandboxManager, err = dockersandbox.New(dockersandbox.Options{
			OutputDir: outputRoot, Logger: logger,
		})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("construct Docker sandbox: %w", err), closeOccurrenceClients(nil, harnessManager.Close))
		}
		sandbox = sandboxManager
		resourceSource, err = dockersandbox.NewResourceSource(dockersandbox.ResourceOptions{
			RunID: runID, TaskIDs: []string{occurrenceID},
		})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("construct Docker resource source: %w", err), closeOccurrenceClients(sandboxManager.Close, harnessManager.Close))
		}
	default:
		return nil, errors.Join(fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type), closeOccurrenceClients(nil, harnessManager.Close))
	}
	var bridge runner.ToolBridge
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
		bridge, err = openclawssh.New(openclawssh.Options{
			OutputDir:  outputRoot,
			ClientPath: filepath.Join(binDir, "aries-ssh"),
			Logger:     logger,
		})
		if err != nil {
			return nil, errors.Join(fmt.Errorf("construct OpenClaw SSH bridge: %w", err), resourceSource.Close(), closeOccurrenceClients(sandboxManager.Close, harnessManager.Close))
		}
	default:
		return nil, errors.Join(fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type), resourceSource.Close(), closeOccurrenceClients(sandboxManager.Close, harnessManager.Close))
	}
	benchmarkRunner, err := runner.New(benchmark, harness, sandbox, bridge, runner.Options{
		Name: cfg.Name, RunID: runID, OutputDir: outputRoot,
		Model:  cfg.Model,
		Logger: logger,
		RuntimeOverrides: runner.RuntimeOverrides{
			HarnessResources:      runner.ResourceOverrides{CPU: cfg.Overrides.HarnessResources.CPU, MemoryMB: cfg.Overrides.HarnessResources.MemoryMB},
			AgentSandboxResources: runner.ResourceOverrides{CPU: cfg.Overrides.AgentSandboxResources.CPU, MemoryMB: cfg.Overrides.AgentSandboxResources.MemoryMB},
			AgentTimeout:          cfg.Overrides.AgentTimeout,
		},
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct runner: %w", err), resourceSource.Close(), closeOccurrenceClients(sandboxManager.Close, harnessManager.Close))
	}
	recorder, err := monitor.New(monitor.Options{
		RunID: runID, TaskIDs: []string{occurrenceID}, OutputDir: outputRoot, Source: resourceSource, Logger: logger,
	})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("construct monitor: %w", err), resourceSource.Close(), closeOccurrenceClients(sandboxManager.Close, harnessManager.Close))
	}
	return &experiment{runner: benchmarkRunner, recorder: recorder, close: func() error {
		return closeOccurrenceClients(sandboxManager.Close, harnessManager.Close)
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

func newRunID(now time.Time, experimentName string) string {
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + experimentName
}

func newLogger() *logrus.Logger {
	logger := logrus.New()
	logger.SetOutput(os.Stderr)
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano})
	return logger
}

func attachRunLog(logger *logrus.Logger, outputRoot string) (func() error, error) {
	path := filepath.Join(outputRoot, "aries.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create run log: %w", err)
	}
	original := logger.Out
	logger.SetOutput(io.MultiWriter(original, file))
	return func() error {
		logger.SetOutput(original)
		return file.Close()
	}, nil
}

func createRunOutputRoot(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.Mkdir(path, 0o700)
}

func executeAndRecord(
	ctx context.Context,
	execute func(context.Context) (core.RunResult, error),
	outputRoot string,
	stdout io.Writer,
) error {
	result, runErr := execute(ctx)
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return errors.Join(runErr, fmt.Errorf("encode run result: %w", err))
	}
	content = append(content, '\n')

	persistErr := persistRunResult(filepath.Join(outputRoot, runResultName), content)
	_, stdoutErr := io.Copy(stdout, bytes.NewReader(content))
	if persistErr != nil {
		persistErr = fmt.Errorf("persist run result: %w", persistErr)
	}
	if stdoutErr != nil {
		stdoutErr = fmt.Errorf("write run result to stdout: %w", stdoutErr)
	}
	return errors.Join(runErr, persistErr, stdoutErr)
}

func persistRunResult(path string, content []byte) error {
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("run output root is not a directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("run output root permissions are %04o; want 0700", info.Mode().Perm())
	}

	temporary, err := os.CreateTemp(directory, ".result-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporaryPath != "" {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Link(temporaryPath, path); err != nil {
		return err
	}
	if err := os.Remove(temporaryPath); err != nil {
		return err
	}
	temporaryPath = ""
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryFile.Close()
	return directoryFile.Sync()
}

func prepareProfile(ctx context.Context, cfg config.Config) error {
	if cfg.Benchmark.Type != "terminalbench2" {
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
	if cfg.Harness.Type != "openclaw" {
		return fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
	if cfg.Sandbox.Type != "docker" {
		return fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
	if cfg.Bridge.Type != "openclaw-ssh" {
		return fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}

	preparationTasks := uniqueStrings(cfg.Benchmark.Tasks)
	benchmark, err := terminalbench.New(terminalbench.Options{
		Root: cfg.Benchmark.Root, TaskIDs: preparationTasks, OutputDir: cfg.OutputDir,
		Revision: cfg.Versions.TerminalBench2.Revision,
	})
	if err != nil {
		return fmt.Errorf("validate terminalbench2 profile: %w", err)
	}
	if err := terminalbench.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.TerminalBench2.RepositoryURL, cfg.Versions.TerminalBench2.Revision); err != nil {
		return fmt.Errorf("setup terminalbench2: %w", err)
	}
	tasks, err := benchmark.Tasks(ctx)
	if err != nil {
		return fmt.Errorf("load terminalbench2 tasks: %w", err)
	}
	images := make([]string, 0, len(tasks)+1)
	images = append(images, cfg.Versions.OpenClaw.Image)
	for _, task := range tasks {
		images = append(images, task.Environment.Image)
	}
	if err := dockersandbox.PullImages(ctx, images); err != nil {
		return fmt.Errorf("prepare Docker images: %w", err)
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}
