package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
)

const runResultName = "run-result.json"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("aries failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	return runCommand(context.Background(), args, os.Stdout)
}

func runCommand(ctx context.Context, args []string, stdout io.Writer) error {
	return runCommandWithDependencies(ctx, args, stdout, commandDependencies{})
}

type commandDependencies struct {
	executablePath  string
	preflightClient httpDoer
	preflightSleep  contextSleep
	prepareProfile  func(context.Context, config.Config) error
}

func runCommandWithDependencies(ctx context.Context, args []string, stdout io.Writer, dependencies commandDependencies) error {
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
	runID, err := newRunID(time.Now(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate run ID: %w", err)
	}
	outputRoot, err := filepath.Abs(filepath.Join(cfg.OutputDir, runID))
	if err != nil {
		return fmt.Errorf("resolve run output root: %w", err)
	}
	if err := createRunOutputRoot(outputRoot); err != nil {
		return fmt.Errorf("create private run output root: %w", err)
	}
	var apiKeys *apiKeySource
	exists := false
	if isOfficialDeepSeek(cfg.Model) {
		repositoryRoot, rootErr := ariesRepositoryRoot(dependencies.executablePath)
		if rootErr != nil {
			validation := failedLiveValidation(cfg.Model, liveValidationConfigurationInvalid, 0)
			persistErr := persistLiveValidation(outputRoot, validation)
			return errors.Join(fmt.Errorf("resolve ARIES repository root: %w", rootErr), persistErr)
		}
		apiKeys, exists, err = loadLocalAPIKeySource(filepath.Join(repositoryRoot, localAPIKeyFile))
		if err != nil {
			validation := failedLiveValidation(cfg.Model, liveValidationCredentialInvalid, 0)
			persistErr := persistLiveValidation(outputRoot, validation)
			return errors.Join(fmt.Errorf("load local API key: %w", err), persistErr)
		}
		if exists {
			defer apiKeys.Clear()
		}
	}

	preflightLookup := environmentAPIKeyLookup
	var harnessLookup func(string) ([]byte, bool)
	if exists {
		preflightLookup = apiKeys.Lookup
		harnessLookup = apiKeys.Lookup
	}
	validation, preflightErr := validateLiveModel(ctx, cfg.Model, preflightLookup, dependencies.preflightClient, dependencies.preflightSleep)
	persistErr := persistLiveValidation(outputRoot, validation)
	if preflightErr != nil || persistErr != nil {
		return errors.Join(preflightErr, persistErr)
	}

	experiment, err := buildExperiment(cfg, runID, outputRoot, harnessLookup)
	if err != nil {
		return err
	}
	return executeAndRecord(ctx, experiment.Run, outputRoot, stdout)
}

func buildExperiment(
	cfg config.Config,
	runID, outputRoot string,
	apiKeyLookup func(string) ([]byte, bool),
) (*experiment, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ARIES executable: %w", err)
	}
	binDir := filepath.Dir(executable)
	var benchmark runner.Benchmark
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		benchmark, err = terminalbench.New(terminalbench.Options{
			Root: cfg.Benchmark.Root, TaskIDs: cfg.Benchmark.Tasks, OutputDir: outputRoot,
			Revision: cfg.Versions.TerminalBench2.Revision, FixGitImage: cfg.Versions.TerminalBench2.FixGitImage,
		})
		if err != nil {
			return nil, fmt.Errorf("construct terminalbench2 benchmark: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
	var harness runner.AgentHarness
	switch cfg.Harness.Type {
	case "openclaw":
		harness, err = openclawharness.New(openclawharness.Options{
			Image: cfg.Versions.OpenClaw.Image, OutputDir: outputRoot, APIKeyLookup: apiKeyLookup,
		})
		if err != nil {
			return nil, fmt.Errorf("construct OpenClaw harness: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
	var sandbox runner.ToolSandbox
	switch cfg.Sandbox.Type {
	case "docker":
		sandbox, err = dockersandbox.New(dockersandbox.Options{
			OutputDir: outputRoot,
		})
		if err != nil {
			return nil, fmt.Errorf("construct Docker sandbox: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
	var bridge runner.ToolBridge
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
		bridge, err = openclawssh.New(openclawssh.Options{
			OutputDir:  outputRoot,
			ClientPath: filepath.Join(binDir, "aries-ssh"),
		})
		if err != nil {
			return nil, fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	benchmarkRunner, err := runner.New(benchmark, harness, sandbox, bridge, runner.Options{
		Name: cfg.Name, RunID: runID, OutputDir: outputRoot,
		Model: core.ModelConfig{BaseURL: cfg.Model.BaseURL, Model: cfg.Model.Model, APIKeyEnv: cfg.Model.APIKeyEnv},
	})
	if err != nil {
		return nil, fmt.Errorf("construct runner: %w", err)
	}
	recorder, err := monitor.New(monitor.Options{
		RunID: runID, TaskIDs: cfg.Benchmark.Tasks, OutputDir: outputRoot,
	})
	if err != nil {
		return nil, fmt.Errorf("construct monitor: %w", err)
	}
	return &experiment{Runner: benchmarkRunner, Recorder: recorder}, nil
}

func newRunID(now time.Time, random io.Reader) (string, error) {
	var suffix [12]byte
	if _, err := io.ReadFull(random, suffix[:]); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix[:]), nil
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

	benchmark, err := terminalbench.New(terminalbench.Options{
		Root: cfg.Benchmark.Root, TaskIDs: cfg.Benchmark.Tasks, OutputDir: cfg.OutputDir,
		Revision: cfg.Versions.TerminalBench2.Revision, FixGitImage: cfg.Versions.TerminalBench2.FixGitImage,
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
