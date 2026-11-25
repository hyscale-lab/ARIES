package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func nativePeriodic(path, runtime, api string) func(context.Context, string, int, []string) (harbor.Freeze, error) {
	return func(ctx context.Context, phase string, step int, triggers []string) (harbor.Freeze, error) {
		var span harbor.Freeze
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
		if err != nil {
			return span, err
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		if err := json.NewEncoder(conn).Encode([]any{phase, runtime, step, api, triggers}); err != nil {
			return span, err
		}
		var bounds struct {
			Pause  int64 `json:"pause_ack_ns"`
			Resume int64 `json:"resume_request_ns"`
		}
		if err := json.NewDecoder(conn).Decode(&bounds); err != nil {
			return span, err
		}
		if bounds.Pause <= 0 || bounds.Resume < bounds.Pause {
			return span, errors.New("invalid collector freeze bounds")
		}
		return harbor.Freeze{PauseAckNS: bounds.Pause, ResumeRequestNS: bounds.Resume}, nil
	}
}
