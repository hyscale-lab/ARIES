package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func (g *guest) nativeResult(ctx context.Context, c replay.Call, diagnostic bool) (core.CommandResult, error) {
	var stdout, stderr bytes.Buffer
	ctx, timings := core.WithCommandTimings(ctx)
	client, err := g.streamMode(ctx, c, &stdout, &stderr, diagnostic)
	if err != nil {
		return client, err
	}
	if client.ExitCode != 0 {
		return client, fmt.Errorf("native OpenClaw tool client failed: %s", stderr.String())
	}
	var wire struct {
		Result         *struct{ Content, Details json.RawMessage }
		TransportError string
	}
	if err = json.Unmarshal(stdout.Bytes(), &wire); err != nil {
		return client, err
	}
	if wire.TransportError != "" {
		return client, errors.New(wire.TransportError)
	}
	if wire.Result == nil {
		return client, errors.New("missing native OpenClaw reply")
	}
	result, err := harbor.DecodeOpenClawResult(c.NativeTool, wire.Result.Content, wire.Result.Details)
	t := timings.Snapshot()
	result.Timing = &t
	return result, err
}
