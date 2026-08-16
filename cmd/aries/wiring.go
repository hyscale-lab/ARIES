package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/internal/app"
	runtimesglang "github.com/hyscale-lab/aries/internal/modelruntime/sglang"
	"github.com/hyscale-lab/aries/pkg/benchmark/deepresearchbench"
	"github.com/hyscale-lab/aries/pkg/benchmark/sweatlas"
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

// environmentAPIKeyLookup resolves the Deep Research Bench judge's API key
// from the process environment, mirroring internal/app's default lookup for
// the harness's own model.
func environmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil, false
	}
	return []byte(value), true
}

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
	case "terminalbench2", "deepresearchbench", "sweatlasqa":
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

func newBenchmark(cfg config.Config, outputRoot, logicalID, occurrenceID string, lookup func(string) ([]byte, bool)) (runner.Benchmark, error) {
	if lookup == nil {
		lookup = environmentAPIKeyLookup
	}
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
	case "deepresearchbench":
		var executionIDs []string
		if occurrenceID != logicalID {
			executionIDs = []string{occurrenceID}
		}
		judgeModel, factModel, jinaAPIKeyEnv, judgeDisabled := deepresearchbenchModels(cfg)
		benchmark, err := deepresearchbench.New(deepresearchbench.Options{
			Root: cfg.Benchmark.Root, TaskIDs: []string{logicalID}, ExecutionTaskIDs: executionIDs, OutputDir: outputRoot,
			Revision:      cfg.Versions.DeepResearchBench.Revision,
			Environment:   environmentFromConfig(cfg.Benchmark.Environment),
			Judge:         judgeModel,
			JudgeDisabled: judgeDisabled,
			FactJudge:     factModel,
			JinaAPIKeyEnv: jinaAPIKeyEnv,
			APIKeyLookup:  lookup,
		})
		if err != nil {
			return nil, fmt.Errorf("construct deepresearchbench benchmark: %w", err)
		}
		if reason := benchmark.FactSkipReason(); reason != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", reason)
		}
		return benchmark, nil
	case "sweatlasqa":
		var executionIDs []string
		if occurrenceID != logicalID {
			executionIDs = []string{occurrenceID}
		}
		// validateBenchmarkType already guarantees cfg.Benchmark.Judge is
		// non-nil for this type by the time wiring runs.
		benchmark, err := sweatlas.New(sweatlas.Options{
			Root: cfg.Benchmark.Root, TaskIDs: []string{logicalID}, ExecutionTaskIDs: executionIDs, OutputDir: outputRoot,
			Revision:     cfg.Versions.SWEAtlas.Revision,
			Judge:        cfg.Benchmark.Judge.CoreModel(),
			APIKeyLookup: lookup,
		})
		if err != nil {
			return nil, fmt.Errorf("construct sweatlasqa benchmark: %w", err)
		}
		return benchmark, nil
	default:
		return nil, fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}

// deepresearchbenchModels resolves the RACE judge and FACT judge model
// configs for a deepresearchbench profile. A nil benchmark.judge, or a
// benchmark.fact whose model fields are all empty, default to the profile's
// main model config rather than requiring it to be repeated. judgeDisabled
// reports whether benchmark.judge.enabled is explicitly false, which is a
// master switch disabling both RACE and FACT in deepresearchbench.New —
// fact/jinaAPIKeyEnv are still resolved unconditionally here regardless, so
// that New can report *why* FACT was skipped when a fact block is also
// present.
func deepresearchbenchModels(cfg config.Config) (judge, fact core.ModelConfig, jinaAPIKeyEnv string, judgeDisabled bool) {
	if judgeCfg := cfg.Benchmark.Judge; judgeCfg != nil && judgeCfg.Enabled != nil && !*judgeCfg.Enabled {
		judgeDisabled = true
	} else {
		judge = cfg.CoreModel()
		if judgeCfg != nil {
			judge = judgeCfg.CoreModel()
		}
	}
	fact = cfg.CoreModel()
	if factCfg := cfg.Benchmark.Fact; factCfg != nil {
		jinaAPIKeyEnv = factCfg.JinaAPIKeyEnv
		if factCfg.Provider != "" || factCfg.BaseURL != "" || factCfg.ID != "" || factCfg.APIKeyEnv != "" {
			fact = factCfg.CoreModel()
		}
	}
	return judge, fact, jinaAPIKeyEnv, judgeDisabled
}

// environmentFromConfig converts a profile's benchmark.environment block into
// the runner-neutral core.Environment. cfg is nil only when Config.validate
// hasn't run (e.g. ad-hoc construction); callers of newBenchmark and
// loadPreparationTasks always pass an already-validated config.
func environmentFromConfig(cfg *config.BenchmarkEnvironment) core.Environment {
	if cfg == nil {
		return core.Environment{}
	}
	return core.Environment{
		Image:     cfg.Image,
		Workdir:   cfg.Workdir,
		CPU:       cfg.CPU,
		MemoryMB:  cfg.MemoryMB,
		StorageMB: cfg.StorageMB,
		Env:       cfg.Env,
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
		manager, err := openclawharness.New(openclawharness.Options{
			Image: cfg.Versions.OpenClaw.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger,
			Mode: cfg.Harness.Mode, Realtime: realtime, WebSearchEnabled: cfg.Harness.WebSearch.Enabled,
			ExtractAPIKeyEnv:       cfg.Harness.WebSearch.ExtractAPIKeyEnv,
			SubagentsEnabled:       cfg.Harness.Subagents.Enabled != nil && *cfg.Harness.Subagents.Enabled,
			MaxConcurrentSubagents: cfg.Harness.Subagents.MaxConcurrent,
		})
		if err != nil {
			return app.HarnessInstance{}, fmt.Errorf("construct OpenClaw harness: %w", err)
		}
		return app.HarnessInstance{Harness: manager, Close: manager.Close}, nil
	case "hermes":
		manager, err := hermesharness.New(hermesharness.Options{
			Image: cfg.Versions.Hermes.Image, OutputDir: outputRoot, APIKeyLookup: lookup, Logger: logger,
			WebSearchEnabled: cfg.Harness.WebSearch.Enabled, ExtractAPIKeyEnv: cfg.Harness.WebSearch.ExtractAPIKeyEnv,
			SubagentsEnabled:       cfg.Harness.Subagents.Enabled != nil && *cfg.Harness.Subagents.Enabled,
			MaxConcurrentSubagents: cfg.Harness.Subagents.MaxConcurrent,
		})
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
		bridge, err := openclawssh.New(openclawssh.Options{OutputDir: outputRoot, ClientPath: filepath.Join(filepath.Dir(executable), "aries-ssh"), Logger: logger, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()})
		if err != nil {
			return nil, fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
		}
		return bridge, nil
	case "hermes-ssh":
		// Hermes runs OpenSSH itself, so this bridge stages no client helper
		// and needs no path to the ARIES executable.
		bridge, err := hermesssh.New(hermesssh.Options{OutputDir: outputRoot, Logger: logger, OmitRawLog: !cfg.Bridge.RetainBridgeRawLog()})
		if err != nil {
			return nil, fmt.Errorf("construct Hermes SSH bridge: %w", err)
		}
		return bridge, nil
	default:
		return nil, fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
}

func setupBenchmark(ctx context.Context, cfg config.Config) error {
	switch cfg.Benchmark.Type {
	case "deepresearchbench":
		return deepresearchbench.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.DeepResearchBench.RepositoryURL, cfg.Versions.DeepResearchBench.Revision)
	case "sweatlasqa":
		return sweatlas.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.SWEAtlas.RepositoryURL, cfg.Versions.SWEAtlas.Revision)
	case "terminalbench2":
		return terminalbench.Setup(ctx, cfg.Benchmark.Root, cfg.Versions.TerminalBench2.RepositoryURL, cfg.Versions.TerminalBench2.Revision)
	default:
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}

func loadPreparationTasks(ctx context.Context, cfg config.Config, taskIDs []string, lookup func(string) ([]byte, bool)) ([]core.Task, error) {
	if lookup == nil {
		lookup = environmentAPIKeyLookup
	}
	switch cfg.Benchmark.Type {
	case "deepresearchbench":
		judgeModel, factModel, jinaAPIKeyEnv, judgeDisabled := deepresearchbenchModels(cfg)
		benchmark, err := deepresearchbench.New(deepresearchbench.Options{
			Root: cfg.Benchmark.Root, TaskIDs: taskIDs, OutputDir: cfg.OutputDir,
			Revision:      cfg.Versions.DeepResearchBench.Revision,
			Environment:   environmentFromConfig(cfg.Benchmark.Environment),
			Judge:         judgeModel,
			JudgeDisabled: judgeDisabled,
			FactJudge:     factModel,
			JinaAPIKeyEnv: jinaAPIKeyEnv,
			APIKeyLookup:  lookup,
		})
		if err != nil {
			return nil, fmt.Errorf("validate deepresearchbench profile: %w", err)
		}
		if reason := benchmark.FactSkipReason(); reason != "" {
			fmt.Fprintf(os.Stderr, "warning: %s\n", reason)
		}
		tasks, err := benchmark.Tasks(ctx)
		if err != nil {
			return nil, fmt.Errorf("load deepresearchbench tasks: %w", err)
		}
		return tasks, nil
	case "sweatlasqa":
		// validateBenchmarkType already guarantees cfg.Benchmark.Judge is
		// non-nil for this type by the time wiring runs.
		benchmark, err := sweatlas.New(sweatlas.Options{
			Root: cfg.Benchmark.Root, TaskIDs: taskIDs, OutputDir: cfg.OutputDir,
			Revision:     cfg.Versions.SWEAtlas.Revision,
			Judge:        cfg.Benchmark.Judge.CoreModel(),
			APIKeyLookup: lookup,
		})
		if err != nil {
			return nil, fmt.Errorf("validate sweatlasqa profile: %w", err)
		}
		tasks, err := benchmark.Tasks(ctx)
		if err != nil {
			return nil, fmt.Errorf("load sweatlasqa tasks: %w", err)
		}
		return tasks, nil
	case "terminalbench2":
		benchmark, err := terminalbench.New(terminalbench.Options{Root: cfg.Benchmark.Root, TaskIDs: taskIDs, OutputDir: cfg.OutputDir, Revision: cfg.Versions.TerminalBench2.Revision})
		if err != nil {
			return nil, fmt.Errorf("validate terminalbench2 profile: %w", err)
		}
		tasks, err := benchmark.Tasks(ctx)
		if err != nil {
			return nil, fmt.Errorf("load terminalbench2 tasks: %w", err)
		}
		return tasks, nil
	default:
		return nil, fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
}
