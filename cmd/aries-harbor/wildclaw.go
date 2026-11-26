package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func wildClawCommand(ctx context.Context, path, mode string, out io.Writer) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	switch mode {
	case "inspect", "timeline":
		r, err := harbor.LoadWildClawTimeline(data)
		if err != nil {
			return err
		}
		if mode == "inspect" {
			return encoder.Encode(r)
		}
		return harbor.Run(ctx, r, harbor.Hooks{
			Boundary: func(context.Context, string, int) error { return nil },
			Record:   func(receipt harbor.Receipt) error { return encoder.Encode(receipt) },
		})
	case "live-plan":
		r, err := harbor.LoadWildClawLive(data)
		if err != nil {
			return err
		}
		return encoder.Encode(struct {
			*harbor.Recording
			Plan replay.Plan `json:"plan"`
		}{r, r.Plan})
	default:
		return fmt.Errorf("unknown -wildclaw-mode %q (choose inspect, timeline, or live-plan)", mode)
	}
}
