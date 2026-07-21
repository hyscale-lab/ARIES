package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/hyscale-lab/aries/pkg/config"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("aries failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: aries EXPERIMENT.json")
	}
	cfg, err := config.Load(args[0])
	if err != nil {
		return err
	}
	return buildExperiment(cfg)
}

func buildExperiment(cfg config.Config) error {
	switch cfg.Benchmark.Type {
	case "terminalbench2":
		// Wired when the concrete M2 adapter exists.
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
		// Wired when the concrete M3 adapter exists.
	default:
		return fmt.Errorf("unsupported sandbox type %q", cfg.Sandbox.Type)
	}
	switch cfg.Bridge.Type {
	case "openclaw-ssh":
		// Wired when the concrete M4 adapter exists.
	default:
		return fmt.Errorf("unsupported bridge type %q", cfg.Bridge.Type)
	}
	return errors.New("experiment types are valid, but concrete M2-M5 components are not implemented yet")
}
