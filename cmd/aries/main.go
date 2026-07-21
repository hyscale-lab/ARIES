package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hyscale-lab/aries/pkg/benchmark/terminalbench"
	"github.com/hyscale-lab/aries/pkg/bridge/openclawssh"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	openclawharness "github.com/hyscale-lab/aries/pkg/harness/openclaw"
	"github.com/hyscale-lab/aries/pkg/runner"
	dockersandbox "github.com/hyscale-lab/aries/pkg/sandbox/docker"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("aries failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 2 && args[0] == "setup" {
		return setup(context.Background(), args[1])
	}
	if len(args) != 1 {
		return errors.New("usage: aries EXPERIMENT.json | aries setup terminalbench2")
	}
	cfg, err := config.Load(args[0])
	if err != nil {
		return err
	}
	if _, err := buildExperiment(cfg); err != nil {
		return err
	}
	return errors.New("the M5 runtime is valid, but M6 end-to-end evaluation execution is not enabled yet")
}

func buildExperiment(cfg config.Config) (*runner.Runner, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate ARIES executable: %w", err)
	}
	binDir := filepath.Dir(executable)
	var benchmark runner.Benchmark
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		benchmark, err = terminalbench.New(terminalbench.Options{
			Root:      cfg.Benchmark.Root,
			TaskIDs:   cfg.Benchmark.Tasks,
			OutputDir: cfg.OutputDir,
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
		harness, err = openclawharness.New(openclawharness.Options{Image: cfg.Harness.Image, OutputDir: cfg.OutputDir})
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
			OutputDir:      cfg.OutputDir,
			ExecHelperPath: filepath.Join(binDir, "aries-exec-helper"),
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
			OutputDir:  cfg.OutputDir,
			ClientPath: filepath.Join(binDir, "aries-ssh"),
			ServerPath: filepath.Join(binDir, "aries-ssh-server"),
		})
		if err != nil {
			return nil, fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	return runner.New(benchmark, harness, sandbox, bridge, runner.Options{
		Name: cfg.Name, RunID: "composition-check", OutputDir: cfg.OutputDir,
		Model: core.ModelConfig{BaseURL: cfg.Model.BaseURL, Model: cfg.Model.Model, APIKeyEnv: cfg.Model.APIKeyEnv},
	})
}

func setup(ctx context.Context, component string) error {
	switch component {
	case "terminalbench2":
		if err := terminalbench.Setup(ctx, terminalbench.DefaultRoot); err != nil {
			return fmt.Errorf("setup terminalbench2: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("unsupported setup component %q", component)
	}
}
