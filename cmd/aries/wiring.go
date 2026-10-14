package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"

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
		PrepareBackend:     prepareBackend,
		PrepareBridge:      prepareBridge,
		ValidateComponents: validateComponents,
		SetupBenchmark:     setupBenchmark,
		LoadBenchmark:      loadBenchmark,
		PullImages:         pullImages,
		BuildHarnessImage:  buildHarnessImage,
		NewHarness:         newHarness,
		NewInfrastructure:  newInfrastructure,
	}
}

func validateComponents(cfg config.Config) error {
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	// Every runtime of an Apptainer run shares one loopback, so two tasks'
	// fixed service ports would collide.
	if cfg.Sandbox.Deployment.Backend == "apptainer" && cfg.Execution.Concurrency > 1 {
		return errors.New("an Apptainer run executes one task at a time; set execution.concurrency to 1")
	}
	// Apptainer runs every command as the instance's root, and SWE-bench Pro
	// runs the agent's commands as UID 65532.
	if cfg.Sandbox.Deployment.Backend == "apptainer" && cfg.Benchmark.Type == "swebenchpro" {
		return errors.New("SWE-bench Pro runs commands as a non-root user, which the Apptainer backend cannot; use Docker or Kubernetes")
	}
	switch cfg.Benchmark.Type {
	case "terminalbench2":
	case "deepresearchbench":
	case "sweatlasqa":
	case "swebenchpro":
	case "toolathlon":
		if err := benchmarkwiring.ValidateToolathlon(cfg); err != nil {
			return err
		}
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

// preparedTaskBenchmark is a concrete adapter capability used only by command wiring.
// Runner continues to consume the four standard roles.
type preparedTaskBenchmark interface {
	runner.Benchmark
	ForTask(string, string) (runner.Benchmark, error)
}

func benchmarkForTasks(cfg config.Config, outputRoot string, taskIDs, executionIDs []string, lookup func(string) ([]byte, bool)) (preparedTaskBenchmark, error) {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		return benchmarkwiring.NewTerminalBench(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "swebenchpro":
		return benchmarkwiring.NewSWEbenchPro(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "deepresearchbench":
		return benchmarkwiring.NewDeepResearchBench(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "sweatlasqa":
		return benchmarkwiring.NewSWEAtlas(cfg, outputRoot, taskIDs, executionIDs, lookup)
	case "toolathlon":
		return benchmarkwiring.NewToolathlon(cfg, outputRoot, taskIDs, executionIDs, lookup)
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
	transport, err := newHarnessDeployment(cfg, logger)
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
	case "kubernetes":
		transport, resources, err := deploymentwiring.NewKubernetesSandbox(cfg.Sandbox.Deployment, runID, occurrenceID, logger)
		if err != nil {
			return app.SandboxInstance{}, err
		}
		return sandboxwiring.New(outputRoot, occurrenceID, gpuIndices, logger, transport, environment.NewTaskEnvironment, resources)
	case "apptainer":
		transport, resources, err := deploymentwiring.NewApptainerSandbox(cfg.Sandbox.Deployment, logger)
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
	if cfg.Bridge.Deployment.Backend == "apptainer" {
		return newApptainerBridge(cfg, runID, outputRoot, placement)
	}
	runtime, err := newDeployment(cfg.Bridge.Deployment, logger)
	if err != nil {
		return nil, fmt.Errorf("construct bridge deployment: %w", err)
	}
	launch := bridgewiring.Launch(cfg.Versions.Bridge.Image)
	launch.RuntimeBackend = cfg.Bridge.Deployment.Backend
	launch.ResourceMetrics = "unsupported"
	switch cfg.Bridge.Deployment.Backend {
	case "docker":
		launch.ResourceMetrics = "docker-stats"
	case "kubernetes":
		launch.ResourceMetrics = "unsupported"
		if cfg.Sandbox.Deployment.Kubernetes.PrometheusURL != "" {
			launch.ResourceMetrics = "prometheus"
		}
	}
	// Sandbox execution access is selected independently of bridge placement.
	launch.Config.Backend = cfg.Sandbox.Deployment.Backend
	switch cfg.Sandbox.Deployment.Backend {
	case "docker":
		launch.Config.BackendEndpoint, launch.Request.Mounts = deploymentwiring.DockerExecutionAccess(cfg.Sandbox.Deployment.Docker.Socket)
	case "kubernetes":
		// The bridge pod reaches sandboxes through its service account.
		launch.Config.BackendEndpoint = cfg.Sandbox.Deployment.Kubernetes.Namespace
	default:
		return nil, errors.Join(fmt.Errorf("unsupported bridge execution backend %q", cfg.Sandbox.Deployment.Backend), runtime.Close())
	}
	return bridgewiring.NewService(cfg, runID, filepath.Join(outputRoot, "infrastructure", "bridge"), placement, runtime, launch)
}

// newApptainerBridge runs the bridge as a process in the run's namespace: from
// inside an instance it could not execute in the sandboxes. Its paths are host
// paths, and its ports are on the run's private loopback.
func newApptainerBridge(cfg config.Config, runID, outputRoot string, placement core.RuntimePlacement) (app.SharedBridge, error) {
	runtime, workdir, endpoint, err := deploymentwiring.ApptainerBridge(cfg.Bridge.Deployment, runID)
	if err != nil {
		return nil, fmt.Errorf("construct bridge deployment: %w", err)
	}
	launch := bridgewiring.Launch(cfg.Versions.Bridge.Image)
	launch.RuntimeBackend = "apptainer"
	launch.ResourceMetrics = "unsupported"
	launch.Request.Workdir = workdir
	launch.Config.OutputDir = filepath.Join(workdir, "evidence")
	launch.Config.ControlAddress = "127.0.0.1:" + strconv.Itoa(launch.Request.ServicePort)
	launch.Config.Listen.BindHost = "127.0.0.1"
	launch.Config.Backend, launch.Config.BackendEndpoint = "apptainer", endpoint
	return bridgewiring.NewService(cfg, runID, filepath.Join(outputRoot, "infrastructure", "bridge"), placement, runtime, launch)
}

func prepareBridge(ctx context.Context, cfg config.Config) error {
	if err := validateDeployment(&cfg); err != nil {
		return err
	}
	switch cfg.Bridge.Deployment.Backend {
	case "docker":
		return deploymentwiring.PrepareDockerBridge(ctx, cfg)
	case "kubernetes":
		// The cluster pulls the bridge image; ARIES cannot build into it.
		return nil
	case "apptainer":
		return deploymentwiring.PrepareApptainerBridge()
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
	case "toolathlon":
		return benchmarkwiring.SetupToolathlon(ctx, cfg)
	default:
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}

func loadBenchmark(ctx context.Context, cfg config.Config, outputRoot string, taskIDs []string, lookup func(string) ([]byte, bool)) (app.PreparedBenchmark, error) {
	benchmark, err := benchmarkForTasks(cfg, outputRoot, taskIDs, nil, lookup)
	if err != nil {
		return app.PreparedBenchmark{}, fmt.Errorf("validate %s profile: %w", cfg.Benchmark.Type, err)
	}
	tasks, err := benchmark.Tasks(ctx)
	if err != nil {
		return app.PreparedBenchmark{}, fmt.Errorf("load %s tasks: %w", cfg.Benchmark.Type, err)
	}
	return app.PreparedBenchmark{Tasks: tasks, NewOccurrence: benchmark.ForTask}, nil
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
		case "docker", "kubernetes", "apptainer":
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
		return deploymentwiring.NewKubernetes(cfg, logger)
	case "apptainer":
		return deploymentwiring.NewApptainer(cfg, logger)
	default:
		return nil, fmt.Errorf("unsupported deployment backend %q", cfg.Backend)
	}
}

func newHarnessDeployment(cfg config.Config, logger *logrus.Logger) (deployment.Deployment, error) {
	switch cfg.Harness.Deployment.Backend {
	case "kubernetes":
		return deploymentwiring.NewKubernetesHarness(cfg.Harness.Deployment, cfg.Harness.Type, logger)
	default:
		return newDeployment(cfg.Harness.Deployment, logger)
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
	case "kubernetes":
		// Each node's kubelet pulls images when it first schedules a pod.
		return nil
	case "apptainer":
		return deploymentwiring.PullApptainerImages(ctx, cfg.Sandbox.Deployment, apptainerImages(cfg, images))
	default:
		return fmt.Errorf("unsupported image preparation backend %q", cfg.Sandbox.Deployment.Backend)
	}
}

// apptainerImages leaves out Hermes's base image. It is pulled only for the
// Docker build that derives the image Hermes runs; Apptainer cannot perform
// that build and unpacks the derived image from the SIF mapped to it instead.
// Unpacking it anyway would add minutes to every setup.
func apptainerImages(cfg config.Config, images []string) []string {
	if cfg.Harness.Type != "hermes" {
		return images
	}
	kept := make([]string, 0, len(images))
	for _, image := range images {
		if image != cfg.Versions.Hermes.Image {
			kept = append(kept, image)
		}
	}
	return kept
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
	case "kubernetes":
		// The derived Hermes image must already be in a registry the cluster
		// pulls from; harness.deployment.kubernetes.images names it.
		return nil
	case "apptainer":
		// Apptainer cannot run the Docker build; the derived image is built
		// elsewhere and mapped to its SIF.
		return harnesswiring.PrepareHermesImage(ctx, cfg, func(ctx context.Context, image, _ string, _ map[string]string) error {
			return deploymentwiring.PrepareApptainerImage(ctx, cfg.Sandbox.Deployment, image)
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
	case "kubernetes":
		owner, err := deploymentwiring.NewKubernetes(cfg.Sandbox.Deployment, logger)
		if err != nil {
			return nil, err
		}
		environment := owner.NewRunEnvironment(runID)
		resources, err := deploymentwiring.NewKubernetesRunResources(cfg.Sandbox.Deployment, runID)
		if err != nil {
			return nil, errors.Join(err, owner.Close())
		}
		return &app.RunInfrastructure{
			Environment: environment, Resources: resources, Close: owner.Close, NetworkPolicy: "per-sandbox-policy",
			NewSandbox: func(cfg config.Config, root, run, occurrence string, gpus []int, log *logrus.Logger) (app.SandboxInstance, error) {
				return newSandbox(cfg, root, run, occurrence, gpus, log, environment)
			},
			NewBridge: func(placement core.RuntimePlacement) (app.SharedBridge, error) {
				return newSharedBridge(cfg, runID, outputRoot, placement, logger)
			},
		}, nil
	case "apptainer":
		owner, err := deploymentwiring.NewApptainer(cfg.Sandbox.Deployment, logger)
		if err != nil {
			return nil, err
		}
		environment := owner.NewRunEnvironment(runID)
		return &app.RunInfrastructure{
			Environment: environment, Resources: deploymentwiring.NewApptainerRunResources(), Close: owner.Close, NetworkPolicy: deploymentwiring.ApptainerNetworkPolicy,
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
