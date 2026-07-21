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
	return buildExperiment(cfg)
}

func buildExperiment(cfg config.Config) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate ARIES executable: %w", err)
	}
	binDir := filepath.Dir(executable)
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		if _, err := terminalbench.New(terminalbench.Options{
			Root:      cfg.Benchmark.Root,
			TaskIDs:   cfg.Benchmark.Tasks,
			OutputDir: cfg.OutputDir,
		}); err != nil {
			return fmt.Errorf("construct terminalbench2 benchmark: %w", err)
		}
	default:
		return fmt.Errorf("unsupported benchmark type %q", cfg.Benchmark.Type)
	}
	switch cfg.Harness.Type {
	case "openclaw":
		// Wired when the concrete M5 adapter exists.
	default:
		return fmt.Errorf("unsupported harness type %q", cfg.Harness.Type)
	}
	switch cfg.Sandbox.Type {
	case "docker":
		if _, err := dockersandbox.New(dockersandbox.Options{
			OutputDir:      cfg.OutputDir,
			ExecHelperPath: filepath.Join(binDir, "aries-exec-helper"),
		}); err != nil {
			return fmt.Errorf("construct Docker sandbox: %w", err)
		}
	default:
		return fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
		if _, err := openclawssh.New(openclawssh.Options{
			OutputDir:  cfg.OutputDir,
			ClientPath: filepath.Join(binDir, "aries-ssh"),
			ServerPath: filepath.Join(binDir, "aries-ssh-server"),
		}); err != nil {
			return fmt.Errorf("construct OpenClaw SSH bridge: %w", err)
		}
	default:
		return fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	return errors.New("benchmark, Docker sandbox, and OpenClaw SSH bridge are valid, but the concrete M5 harness is not implemented yet")
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
