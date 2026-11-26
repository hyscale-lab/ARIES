package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hyscale-lab/aries/pkg/core"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

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
	case "deltabox-lite", "deltabox-lite-session":
		r, err := harbor.LoadWildClawCalendar(data)
		if err != nil {
			return err
		}
		if mode == "deltabox-lite-session" {
			return harbor.Run(ctx, r, harbor.Hooks{
				Boundary: func(context.Context, string, int) error { return nil },
				Record:   func(receipt harbor.Receipt) error { return encoder.Encode(receipt) },
				Exec:     liteTool,
			})
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		trace, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "./run-arm.sh", os.Getenv("ARIES_LITE_RUN"), os.Getenv("ARIES_LITE_PROFILE"), os.Getenv("ARIES_LITE_ARM"))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
		cmd.Env = append(os.Environ(), "ARIES_HARBOR="+binary, "ARIES_LITE_TRACE="+trace)
		cmd.Stdout, cmd.Stderr = out, os.Stderr
		return cmd.Run()
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
		return fmt.Errorf("unknown -wildclaw-mode %q (choose inspect, timeline, live-plan, or deltabox-lite)", mode)
	}
}

func liteTool(ctx context.Context, c replay.Call) (core.CommandResult, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	payload := c.Script
	switch c.Operation {
	case "ReadFile":
		payload = c.Path
	case "Exec":
	default:
		return core.CommandResult{}, fmt.Errorf("unsupported Lite operation %q", c.Operation)
	}
	cmd := exec.CommandContext(ctx, "./exec-call", c.Workdir, c.Operation, payload)
	cmd.WaitDelay = 2 * time.Second
	output, err := cmd.CombinedOutput()
	rc := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		rc = exit.ExitCode()
		if rc < 0 {
			return core.CommandResult{}, err
		}
		err = nil
	}
	return core.CommandResult{Stdout: string(output), ExitCode: rc}, err
}
