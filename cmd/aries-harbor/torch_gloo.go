package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment/vhive"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

const torchGlooTrial = "e047ddea-bb8c-43c0-b71e-e625a611565f"
const torchGlooSpawnTrial = "ce62b3a5-602c-44c7-b13e-6537dca35d1d"

//go:embed torch_gloo.py
var torchGlooProbe string

func readTorchGloo(ctx context.Context, manager *vhive.Manager, id, trial, phase, action string, log *json.Encoder) error {
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	python, wheel, importRoot := "/usr/bin/python3", "2.5.1+cpu", "/tmp/parallel-linear-deps"
	if trial == torchGlooSpawnTrial {
		wheel, importRoot = "2.13.0+cpu", ""
		if phase == "after_source" {
			python = "/tmp/parallel-linear-venv/bin/python"
		}
	}
	result, err := manager.Exec(ctx, id, core.Command{Path: python,
		Args: []string{"-B", "-c", torchGlooProbe, action, wheel, importRoot}, Dir: "/app"})
	if err != nil {
		return err
	}
	end, err := harbor.Clock()
	if err != nil {
		return err
	}
	if result.ExitCode != 0 || !json.Valid([]byte(result.Stdout)) {
		return fmt.Errorf("Torch hostname %s at %s failed: exit %d, stderr %s", action, phase, result.ExitCode, result.Stderr)
	}
	return log.Encode(map[string]any{"event": "torch_gloo_network_readback", "phase": phase, "action": action,
		"begin_monotonic_ns": begin, "end_monotonic_ns": end, "state": json.RawMessage(result.Stdout),
		"fallback_warnings": strings.Count(result.Stderr, "Using the loopback address as fallback")})
}
