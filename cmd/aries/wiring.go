package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/hyscale-lab/aries/internal/app"
	benchmarkwiring "github.com/hyscale-lab/aries/internal/app/wiring/benchmark"
	bridgewiring "github.com/hyscale-lab/aries/internal/app/wiring/bridge"
	deploymentwiring "github.com/hyscale-lab/aries/internal/app/wiring/deployment"
	harnesswiring "github.com/hyscale-lab/aries/internal/app/wiring/harness"
	runtimewiring "github.com/hyscale-lab/aries/internal/app/wiring/runtime"
	sandboxwiring "github.com/hyscale-lab/aries/internal/app/wiring/sandbox"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

func commandWiring() app.Wiring {
	return app.Wiring{
		PrepareBackend:       prepareBackend,
		PrepareBridge:        prepareBridge,
		ValidateComponents:   validateComponents,
		SetupBenchmark:       setupBenchmark,
		LoadPreparationTasks: loadPreparationTasks,
		PullImages:           pullImages,
		BuildHarnessImage:    buildHarnessImage,
		NewBenchmark:         newBenchmark,
		NewHarness:           newHarness,
		NewInfrastructure:    newInfrastructure,
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
	transport, err := newDeployment(cfg.Harness.Deployment, cfg.Versions, logger)
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

func newSandbox(cfg config.Config, outputRoot, runID, occurrenceID string, gpuIndices []int, logger *logrus.Logger, environment deployment.RunEnvironment) (app.SandboxInstance, error) {
	if err := validateDeployment(&cfg); err != nil {
		return app.SandboxInstance{}, err
	}
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		transport, resources, err := deploymentwiring.NewDockerSandbox(cfg.Sandbox.Deployment, runID, occurrenceID, logger)
		if err != nil {
			return app.SandboxInstance{}, err
		}
		return sandboxwiring.New(outputRoot, occurrenceID, gpuIndices, logger, transport, environment.NewTaskEnvironment, resources)
	default:
		return app.SandboxInstance{}, fmt.Errorf("unsupported sandbox.deployment.backend %q", cfg.Sandbox.Deployment.Backend)
	}
}

func newSharedBridge(cfg config.Config, runID, outputRoot string, placement core.RuntimePlacement, logger *logrus.Logger) (app.SharedBridge, error) {
	if err := validateDeployment(&cfg); err != nil {
		return nil, err
	}
	switch cfg.Bridge.Type {
	case "openclaw-ssh", "hermes-ssh":
	default:
		return nil, fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	runtime, err := newDeployment(cfg.Bridge.Deployment, cfg.Versions, logger)
	if err != nil {
		return nil, fmt.Errorf("construct bridge deployment: %w", err)
	}
	launch := bridgewiring.Launch(cfg.Versions.Bridge.Image)
	launch.RuntimeBackend = cfg.Bridge.Deployment.Backend
	launch.ResourceMetrics = "unsupported"
	if cfg.Bridge.Deployment.Backend == "docker" {
		launch.ResourceMetrics = "docker-stats"
	}
	// Sandbox execution access is selected independently of bridge placement.
	launch.Config.Backend = cfg.Sandbox.Deployment.Backend
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		launch.Config.BackendEndpoint, launch.Request.Mounts = deploymentwiring.DockerExecutionAccess(cfg.Sandbox.Deployment.Docker.Socket)
	default:
		return nil, errors.Join(fmt.Errorf("unsupported bridge execution backend %q", cfg.Sandbox.Deployment.Backend), runtime.Close())
	}
	return bridgewiring.NewService(cfg, runID, filepath.Join(outputRoot, "infrastructure", "bridge"), placement, runtime, launch)
}

func prepareBridge(ctx context.Context, cfg config.Config) error {
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	switch cfg.Bridge.Deployment.Backend {
	case "docker":
		return deploymentwiring.PrepareDockerBridge(ctx, cfg)
	default:
		return fmt.Errorf("unsupported bridge preparation backend %q", cfg.Bridge.Deployment.Backend)
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
	if cfg.Harness.Deployment.Backend != cfg.Sandbox.Deployment.Backend {
		return errors.New("harness and sandbox must use the same deployment backend")
	}

	if err := containerimage.ValidatePinnedTagOnly(cfg.Versions.Bridge.Image); err != nil {
		return fmt.Errorf("bridge deployment requires pinned versions.bridge.image: %w", err)
	}
	for _, component := range []struct {
		path      string
		placement config.DeploymentConfig
	}{
		{"harness.deployment", cfg.Harness.Deployment}, {"sandbox.deployment", cfg.Sandbox.Deployment},
	} {
		switch component.placement.Backend {
		case "docker":
		default:
			return fmt.Errorf("%s: unsupported backend %q", component.path, component.placement.Backend)
		}
	}
	return nil
}

func newDeployment(cfg config.DeploymentConfig, versions config.Versions, logger *logrus.Logger) (deployment.Deployment, error) {
	switch cfg.Backend {
	case "docker":
		return deploymentwiring.NewDocker(cfg, logger)
	default:
		return nil, fmt.Errorf("unsupported deployment backend %q", cfg.Backend)
	}
}

func pullImages(ctx context.Context, cfg config.Config, images []string) error {
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	// Docker task components share one selected daemon.
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		return deploymentwiring.PullDockerImages(ctx, cfg.Sandbox.Deployment, images)
	default:
		return fmt.Errorf("unsupported image preparation backend %q", cfg.Sandbox.Deployment.Backend)
	}
}

// buildHarnessImage builds the image the harness runs when it is derived from
// the pinned one: Hermes runs its pinned image plus hermes-otel. It uses the
// daemon pullImages prepared the base on.
func buildHarnessImage(ctx context.Context, cfg config.Config) error {
	if cfg.Harness.Type != "hermes" {
		return nil
	}
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		return harnesswiring.PrepareHermesImage(ctx, cfg, func(ctx context.Context, image, dockerfile string, buildArgs map[string]string) error {
			return deploymentwiring.BuildDockerImage(ctx, cfg.Sandbox.Deployment, image, dockerfile, buildArgs)
		})
	default:
		return fmt.Errorf("unsupported image preparation backend %q", cfg.Sandbox.Deployment.Backend)
	}
}

func newInfrastructure(cfg config.Config, runID, outputRoot string, logger *logrus.Logger) (*app.RunInfrastructure, error) {
	if err := validateDeployment(&cfg); err != nil {
		return nil, err
	}
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		owner, err := deploymentwiring.NewDocker(cfg.Sandbox.Deployment, logger)
		if err != nil {
			return nil, err
		}
		environment := owner.NewRunEnvironment(runID)
		resources, err := deploymentwiring.NewDockerRunResources(cfg.Bridge.Deployment, runID)
		if err != nil {
			return nil, errors.Join(err, owner.Close())
		}
		return &app.RunInfrastructure{
			Environment: environment, Resources: resources, Close: owner.Close, NetworkPolicy: "shared-egress",
			NewSandbox: func(cfg config.Config, root, run, occurrence string, gpus []int, log *logrus.Logger) (app.SandboxInstance, error) {
				return newSandbox(cfg, root, run, occurrence, gpus, log, environment)
			},
			NewBridge: func(placement core.RuntimePlacement) (app.SharedBridge, error) {
				return newSharedBridge(cfg, runID, outputRoot, placement, logger)
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported run environment backend %q", cfg.Sandbox.Deployment.Backend)
	}
}
