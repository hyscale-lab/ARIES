package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/internal/app"
	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
	"github.com/sirupsen/logrus"
)

func commandWiring() app.Wiring {
	return app.Wiring{
		PrepareBackend:       prepareBackend,
		ValidateComponents:   validateComponents,
		SetupBenchmark:       setupBenchmark,
		LoadPreparationTasks: loadPreparationTasks,
		PullImages:           dockersandbox.PullImages,
		NewBenchmark:         newBenchmark,
		NewHarness:           newHarness,
		NewSandbox:           newSandbox,
		NewBridge:            newBridge,
	}
}

func validateComponents(cfg config.Config) error {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
	default:
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
	switch cfg.Harness.Type {
	case "openclaw":
	default:
		return fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
	switch cfg.Sandbox.Type {
	case "docker":
	default:
		return fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
	default:
		return fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	return nil
}

func prepareBackend(cfg config.Config, outputDir string) (app.PreparedBackend, error) {
	model := cfg.CoreModel()
	switch cfg.Runtime.Backend {
	case "deepseek":
		if cfg.Runtime.Mode != "external" {
			return app.PreparedBackend{}, errors.New("DeepSeek runtime must be external")
		}
		return app.PreparedBackend{Model: model}, nil
	case "sglang":
		if _, err := runtimesglang.LoadNativeConfig(cfg.Runtime.Config.ResolvedFile, cfg.Model.ID, cfg.Model.BaseURL); err != nil {
			return app.PreparedBackend{}, err
		}
		switch cfg.Runtime.Mode {
		case "external":
			return app.PreparedBackend{Model: model}, nil
		case "managed":
			runtime, err := runtimesglang.New(runtimesglang.Options{Executable: cfg.Runtime.Config.Executable, ConfigPath: cfg.Runtime.Config.ResolvedFile, OutputDir: outputDir, BaseURL: cfg.Model.BaseURL})
			if err != nil {
				return app.PreparedBackend{}, err
			}
			return app.PreparedBackend{Model: model, Runtime: runtime}, nil
		default:
			return app.PreparedBackend{}, fmt.Errorf("unsupported SGLang runtime mode %q", cfg.Runtime.Mode)
		}
	default:
		return app.PreparedBackend{}, fmt.Errorf("unsupported model runtime backend %q", cfg.Runtime.Backend)
	}
}

func newBenchmark(cfg config.Config, outputRoot, logicalID, occurrenceID string) (runner.Benchmark, error) {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		var executionIDs []string
		if occurrenceID != logicalID {
			executionIDs = []string{occurrenceID}
		}
		benchmark, err := terminalbench.New(terminalbench.Options{Root: cfg.Benchmark.Root, TaskIDs: []string{logicalID}, ExecutionTaskIDs: executionIDs, OutputDir: outputRoot, Revision: cfg.Versions.TerminalBench2.Revision})
		if err != nil {
			return nil, fmt.Errorf("construct terminalbench2 benchmark: %w", err)
		}
		return benchmark, nil
	default:
		return nil, fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}

func newHarness(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger) (app.HarnessInstance, error) {
	switch cfg.Harness.Type {
	case "openclaw":
		manager, err := openclawharness.New(openclawharness.Options{Image: cfg.Versions.OpenClaw.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger})
		if err != nil {
			return app.HarnessInstance{}, fmt.Errorf("construct OpenClaw harness: %w", err)
		}
		return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
	default:
		return app.HarnessInstance{}, fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
}

func newSandbox(cfg config.Config, outputRoot, runID, occurrenceID string, logger *logrus.Logger) (app.SandboxInstance, error) {
	switch cfg.Sandbox.Type {
	case "docker":
		manager, err := dockersandbox.New(dockersandbox.Options{OutputDir: outputRoot, Logger: logger})
		if err != nil {
			return app.SandboxInstance{}, fmt.Errorf("construct Docker sandbox: %w", err)
		}
		source, err := dockersandbox.NewResourceSource(dockersandbox.ResourceOptions{RunID: runID, TaskIDs: []string{occurrenceID}})
		if err != nil {
			return app.SandboxInstance{}, errors.Join(fmt.Errorf("construct Docker resource source: %w", err), manager.Close())
		}
		return app.SandboxInstance{Sandbox: manager, Resources: source, Close: manager.Close}, nil
	default:
		return app.SandboxInstance{}, fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
}

func newBridge(cfg config.Config, outputRoot string, logger *logrus.Logger) (runner.ToolBridge, error) {
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("locate ARIES executable: %w", err)
		}
		bridge, err := openclawssh.New(openclawssh.Options{OutputDir: outputRoot, ClientPath: filepath.Join(filepath.Dir(executable), "aries-ssh"), Logger: logger})
		if err != nil {
			return nil, fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
		}
		return bridge, nil
	default:
		return nil, fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
}

func setupBenchmark(ctx context.Context, cfg config.Config) error {
	return terminalbench.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.TerminalBench2.RepositoryURL, cfg.Versions.TerminalBench2.Revision)
}

func loadPreparationTasks(ctx context.Context, cfg config.Config, taskIDs []string) ([]core.Task, error) {
	benchmark, err := terminalbench.New(terminalbench.Options{Root: cfg.Benchmark.Root, TaskIDs: taskIDs, OutputDir: cfg.OutputDir, Revision: cfg.Versions.TerminalBench2.Revision})
	if err != nil {
		return nil, fmt.Errorf("validate terminalbench2 profile: %w", err)
	}
	tasks, err := benchmark.Tasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("load terminalbench2 tasks: %w", err)
	}
	return tasks, nil
}
