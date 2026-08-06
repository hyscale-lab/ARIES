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
	"github.com/hyscale-lab/aries/pkg/bridge/hermesssh"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	hermesharness "github.com/hyscale-lab/aries/pkg/harness/hermes"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"github.com/hyscale-lab/aries/pkg/monitor"
	nvidiamonitor "github.com/hyscale-lab/aries/pkg/monitor/nvidia"
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
	case "hermes":
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
	model := cfg.CoreModel()
	switch cfg.Runtime.Backend {
	case "deepseek":
		if cfg.Runtime.Mode != "external" {
			return app.PreparedBackend{}, errors.New("DeepSeek runtime must be external")
		}
		return app.PreparedBackend{Model: model}, nil
	case "sglang":
		native, err := runtimesglang.LoadNativeConfig(cfg.Runtime.Config.ResolvedFile, cfg.Model.ID, cfg.Model.BaseURL)
		if err != nil {
			return app.PreparedBackend{}, err
		}
		switch cfg.Runtime.Mode {
		case "external":
			return app.PreparedBackend{Model: model}, nil
		case "managed":
			gpuIndices, err := native.ResolveGPUIndices(cfg.Runtime.Config.GPUIndices)
			if err != nil {
				return app.PreparedBackend{}, fmt.Errorf("resolve managed SGLang GPUs: %w", err)
			}
			runtime, err := runtimesglang.New(runtimesglang.Options{Executable: cfg.Runtime.Config.Executable, ConfigPath: cfg.Runtime.Config.ResolvedFile, OutputDir: outputDir, BaseURL: cfg.Model.BaseURL, CredentialEnv: cfg.Model.APIKeyEnv, GPUIndices: append([]int(nil), gpuIndices...)})
			if err != nil {
				return app.PreparedBackend{}, err
			}
			return app.PreparedBackend{Model: model, Runtime: runtime, EffectiveGPUIndices: append([]int(nil), gpuIndices...)}, nil
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
		realtime := openclawharness.RealtimeOptions{
			AgentQuestionTemplate: cfg.Harness.Realtime.AgentQuestionTemplate,
			TTS: openclawharness.RealtimeTTSOptions{
				Provider: cfg.Harness.Realtime.TTS.Provider, BaseURL: cfg.Harness.Realtime.TTS.BaseURL,
				APIKeyEnv: cfg.Harness.Realtime.TTS.APIKeyEnv, Model: cfg.Harness.Realtime.TTS.Model,
				Voice: cfg.Harness.Realtime.TTS.Voice, Instructions: cfg.Harness.Realtime.TTS.Instructions,
				Speed: cfg.Harness.Realtime.TTS.Speed, Timeout: cfg.Harness.Realtime.TTS.Timeout,
			},
			ChunkDuration:         cfg.Harness.Realtime.ChunkDuration,
			ListenDuration:        cfg.Harness.Realtime.ListenDuration,
			QuietDuration:         cfg.Harness.Realtime.QuietDuration,
			AgentWaitDuration:     cfg.Harness.Realtime.AgentWaitDuration,
			ToolCallTimeout:       cfg.Harness.Realtime.ToolCallTimeout,
			TrailingSilenceMillis: cfg.Harness.Realtime.TrailingSilenceMillis,
			Provider:              cfg.Harness.Realtime.Provider,
			Model:                 cfg.Harness.Realtime.Model,
			Voice:                 cfg.Harness.Realtime.Voice,
			ReasoningEffort:       cfg.Harness.Realtime.ReasoningEffort,
			IncludeEvents:         cfg.Harness.Realtime.IncludeEvents,
		}
		manager, err := openclawharness.New(openclawharness.Options{Image: cfg.Versions.OpenClaw.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger, Mode: cfg.Harness.Mode, Realtime: realtime})
		if err != nil {
			return app.HarnessInstance{}, fmt.Errorf("construct OpenClaw harness: %w", err)
		}
		return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
	case "hermes":
		manager, err := hermesharness.New(hermesharness.Options{Image: cfg.Versions.Hermes.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger})
		if err != nil {
			return app.HarnessInstance{}, fmt.Errorf("construct Hermes harness: %w", err)
		}
		return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
	default:
		return app.HarnessInstance{}, fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
}

func newSandbox(cfg config.Config, outputRoot, runID, occurrenceID string, gpuIndices []int, logger *logrus.Logger) (app.SandboxInstance, error) {
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
		var resources monitor.ResourceSource = source
		if len(gpuIndices) != 0 {
			gpuSource, err := nvidiamonitor.NewSource(nvidiamonitor.Options{TaskID: occurrenceID, GPUIndices: gpuIndices})
			if err != nil {
				return app.SandboxInstance{}, errors.Join(fmt.Errorf("construct NVIDIA resource source: %w", err), source.Close(), manager.Close())
			}
			resources = &combinedResourceSource{container: source, gpu: gpuSource}
		}
		return app.SandboxInstance{Sandbox: manager, Resources: resources, Close: manager.Close}, nil
	default:
		return app.SandboxInstance{}, fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
}

type combinedResourceSource struct {
	container monitor.ResourceSource
	gpu       monitor.ResourceSource
}

func (source *combinedResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	containerReadings, err := source.container.Sample(ctx)
	if err != nil {
		return nil, err
	}
	gpuReadings, err := source.gpu.Sample(ctx)
	if err != nil {
		return nil, err
	}
	return append(containerReadings, gpuReadings...), nil
}

func (source *combinedResourceSource) Close() error {
	return errors.Join(source.gpu.Close(), source.container.Close())
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
	case "hermes-ssh":
		// Hermes runs OpenSSH itself, so this bridge stages no client helper
		// and needs no path to the ARIES executable.
		bridge, err := hermesssh.New(hermesssh.Options{OutputDir: outputRoot, Logger: logger})
		if err != nil {
			return nil, fmt.Errorf("construct Hermes SSH bridge: %w", err)
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
