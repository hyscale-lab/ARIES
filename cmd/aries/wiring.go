package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	benchmarkwiring "github.com/hyscale-lab/aries/internal/app/wiring/benchmark"
	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	deploymentwiring "github.com/hyscale-lab/aries/internal/app/wiring/deployment"
	harnesswiring "github.com/hyscale-lab/aries/internal/app/wiring/harness"
	runtimewiring "github.com/hyscale-lab/aries/internal/app/wiring/runtime"
	sandboxwiring "github.com/hyscale-lab/aries/internal/app/wiring/sandbox"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

func commandWiring() app.Wiring {
	return app.Wiring{
		PrepareBackend:       prepareBackend,
		ValidateComponents:   validateComponents,
		SetupBenchmark:       setupBenchmark,
		LoadPreparationTasks: loadPreparationTasks,
		PullImages:           pullImages,
		NewBenchmark:         newBenchmark,
		NewHarness:           newHarness,
		NewSandbox:           newSandbox,
		NewBridge:            newBridge,
	}
}

func validateComponents(cfg config.Config) error {
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	switch cfg.Benchmark.Type {
	case "terminalbench2":
	case "deepresearchbench":
	case "sweatlasqa":
	case "swebenchpro":
	default:
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
	switch cfg.Harness.Type {
	case "openclaw":
	case "hermes":
	default:
		return fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
	case "hermes-ssh":
	default:
		return fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	// Each bridge speaks one harness's SSH grammar, so the pair is checked
	// here rather than left to fail at the first tool call.
	if (cfg.Harness.Type == "hermes") != (cfg.Bridge.Type == "hermes-ssh") {
		return fmt.Errorf("harness type %q requires its paired bridge, not %q", cfg.Harness.Type, cfg.Bridge.Type)
	}
	return nil
}

func prepareBackend(cfg config.Config, outputDir string) (app.PreparedBackend, error) {
	switch cfg.Runtime.Mode {
	case "external":
		switch cfg.Runtime.Backend {
		case "deepseek", "openai", "sglang":
			return app.PreparedBackend{Model: cfg.CoreModel()}, nil
		default:
			return app.PreparedBackend{}, fmt.Errorf("unsupported model runtime backend %q", cfg.Runtime.Backend)
		}
	case "managed":
		switch cfg.Runtime.Backend {
		case "sglang":
			return runtimewiring.NewSGLang(cfg, outputDir)
		default:
			return app.PreparedBackend{}, fmt.Errorf("runtime.backend %q must be external", cfg.Runtime.Backend)
		}
	default:
		return app.PreparedBackend{}, fmt.Errorf("unsupported model runtime mode %q", cfg.Runtime.Mode)
	}
}

func newBenchmark(cfg config.Config, outputRoot, logicalID, occurrenceID string, lookup func(string) ([]byte, bool)) (runner.Benchmark, error) {
	var executionIDs []string
	if occurrenceID != logicalID {
		executionIDs = []string{occurrenceID}
	}
	benchmark, err := benchmarkForTasks(cfg, outputRoot, []string{logicalID}, executionIDs, lookup)
	if err != nil {
		return nil, fmt.Errorf("construct %s benchmark: %w", cfg.Benchmark.Type, err)
	}
	return benchmark, nil
}

func benchmarkForTasks(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (runner.Benchmark, error) {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		return benchmarkwiring.NewTerminalBench(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "swebenchpro":
		return benchmarkwiring.NewSWEbenchPro(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "deepresearchbench":
		return benchmarkwiring.NewDeepResearchBench(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "sweatlasqa":
		return benchmarkwiring.NewSWEAtlas(cfg, outputRoot, taskIDs, executionIDs, lookup)
	default:
		return nil, fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}

func newHarness(cfg config.Config, outputRoot string, lookup func(string) ([]byte, bool), logger *logrus.Logger) (app.HarnessInstance, error) {
	if err := validateDeployment(&cfg); err != nil {
		return app.HarnessInstance{}, err
	}
	if err := harnesswiring.ValidateMCPServers(cfg.Harness); err != nil {
		return app.HarnessInstance{}, err
	}
	transport, err := newDeployment(cfg.Harness.Deployment, logger)
	if err != nil {
		return app.HarnessInstance{}, fmt.Errorf("construct harness deployment: %w", err)
	}
	switch cfg.Harness.Type {
	case "openclaw":
		return harnesswiring.NewOpenClaw(cfg, outputRoot, lookup, logger, transport)
	case "hermes":
		return harnesswiring.NewHermes(cfg, outputRoot, lookup, logger, transport)
	default:
		return app.HarnessInstance{}, errors.Join(fmt.Errorf("unsupported harness type %q", cfg.Harness.Type), transport.Close())
	}
}

func newSandbox(cfg config.Config, outputRoot, runID, occurrenceID string, gpuIndices []int, logger *logrus.Logger) (app.SandboxInstance, error) {
	if err := validateDeployment(&cfg); err != nil {
		return app.SandboxInstance{}, err
	}
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		transport, resources, err := deploymentwiring.NewDockerSandbox(cfg.Sandbox.Deployment, runID, occurrenceID, logger)
		if err != nil {
			return app.SandboxInstance{}, err
		}
		return sandboxwiring.New(outputRoot, occurrenceID, gpuIndices, logger, transport, transport.NewTaskEnvironment, resources)
	default:
		return app.SandboxInstance{}, fmt.Errorf("unsupported sandbox.deployment.backend %q", cfg.Sandbox.Deployment.Backend)
	}
}

func newBridge(cfg config.Config, outputRoot string, resolveListen func(context.Context) (core.BridgeListen, error), logger *logrus.Logger) (runner.ToolBridge, error) {
	if err := validateDeployment(&cfg); err != nil {
		return nil, err
	}
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
		return bridgewiring.NewOpenClaw(cfg, outputRoot, resolveListen, logger)
	case "hermes-ssh":
		return bridgewiring.NewHermes(cfg, outputRoot, resolveListen, logger)
	default:
		return nil, fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
}

func setupBenchmark(ctx context.Context, cfg config.Config) error {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		return benchmarkwiring.SetupTerminalBench(ctx, cfg)
	case "deepresearchbench":
		return benchmarkwiring.SetupDeepResearchBench(ctx, cfg)
	case "sweatlasqa":
		return benchmarkwiring.SetupSWEAtlas(ctx, cfg)
	case "swebenchpro":
		return benchmarkwiring.SetupSWEbenchPro(ctx, cfg)
	default:
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}

func loadPreparationTasks(ctx context.Context, cfg config.Config, taskIDs []string, lookup func(string) ([]byte, bool)) ([]core.Task, error) {
	benchmark, err := benchmarkForTasks(cfg, cfg.OutputDir, taskIDs, nil, lookup)
	if err != nil {
		return nil, fmt.Errorf("validate %s profile: %w", cfg.Benchmark.Type, err)
	}
	tasks, err := benchmark.Tasks(ctx)
	if err != nil {
		return nil, fmt.Errorf("load %s tasks: %w", cfg.Benchmark.Type, err)
	}
	return tasks, nil
}

func validateDeployment(cfg *config.Config) error {
	if err := cfg.NormalizeDeployment(); err != nil {
		return err
	}
	for _, component := range []struct {
		path      string
		placement config.DeploymentConfig
	}{
		{"harness.deployment", cfg.Harness.Deployment}, {"sandbox.deployment", cfg.Sandbox.Deployment},
	} {
		switch component.placement.Backend {
		case "docker":
		case "kubernetes":
			return fmt.Errorf("%s: Kubernetes deployment is not implemented", component.path)
		default:
			return fmt.Errorf("%s: unsupported backend %q", component.path, component.placement.Backend)
		}
	}
	return nil
}

func newDeployment(cfg config.DeploymentConfig, logger *logrus.Logger) (deployment.Deployment, error) {
	switch cfg.Backend {
	case "docker":
		return deploymentwiring.NewDocker(cfg, logger)
	case "kubernetes":
		return nil, errors.New("Kubernetes deployment is not implemented")
	default:
		return nil, fmt.Errorf("unsupported deployment backend %q", cfg.Backend)
	}
}

func pullImages(ctx context.Context, cfg config.Config, images []string) error {
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	// The supported embedded topology requires one daemon for both components.
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		return deploymentwiring.PullDockerImages(ctx, cfg.Sandbox.Deployment, images)
	default:
		return fmt.Errorf("unsupported image preparation backend %q", cfg.Sandbox.Deployment.Backend)
	}
}
