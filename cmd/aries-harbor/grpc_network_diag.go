package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
)

const grpcLoopbackTrial = "69ba9b75-cf5b-4722-94f4-7ce3ea31bbb4"

//go:embed grpc_route_probe.py
var grpcRouteProbe string

// The private guest OS changes only its own loopback address. This reads that
// state in the task's network namespace, outside the recorded tool intervals.
func readGRPCLoopback(ctx context.Context, guest groupCommand, phase string, log *json.Encoder, ipv4Addrs, diagnostic bool) error {
	profile := "--require-omitted"
	if ipv4Addrs {
		profile = "--require-ipv4-addresses"
	}
	out, err := guest(ctx, core.Command{Path: "/usr/bin/env", Args: []string{"python3", "-I", "-S", "-c", grpcRouteProbe, profile}})
	if err != nil {
		return err
	}
	var readback map[string]any
	if err := json.Unmarshal([]byte(out.Stdout), &readback); err != nil {
		return fmt.Errorf("decode loopback readback: %w", err)
	}
	event := "source_grpc_network"
	if diagnostic {
		event = "diagnostic_grpc_network"
	}
	if err := log.Encode(map[string]any{"event": event, "phase": phase, "readback": readback, "exit_code": out.ExitCode}); err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return errors.New("guest loopback diagnostic preconditions differ")
	}
	return nil
}
