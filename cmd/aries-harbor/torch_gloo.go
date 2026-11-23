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

//go:embed torch_gloo.py
var torchGlooProbe string

func readTorchGloo(ctx context.Context, manager *vhive.Manager, id, phase, action string, log *json.Encoder) error {
	begin, err := harbor.Clock()
	if err != nil {
		return err
	}
	result, err := manager.Exec(ctx, id, core.Command{Path: "/usr/bin/python3",
		Args: []string{"-c", torchGlooProbe, action}, Dir: "/app"})
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
