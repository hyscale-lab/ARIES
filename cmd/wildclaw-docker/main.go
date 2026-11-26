package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: wildclaw-docker TRACE.jsonl DISPOSABLE_CONTAINER")
	}
	src, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	recording, err := harbor.LoadWildClawLive(src)
	if err != nil {
		log.Fatal(err)
	}
	shell := "/bin/bash"
	if recording.Task == "01_Productivity_Flow_task_8_real_image_category" {
		// The recorded default shell rejects "set -o pipefail".
		shell = "sh"
	}
	container := os.Args[2]
	enc := json.NewEncoder(os.Stdout)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	err = harbor.Run(ctx, recording, harbor.Hooks{
		Boundary: func(context.Context, string, int) error { return nil },
		Record:   func(r harbor.Receipt) error { return enc.Encode(r) },
		Exec: func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
			if c.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, c.Timeout)
				defer cancel()
			}
			args := []string{"exec"}
			if c.Workdir != "" {
				args = append(args, "-w", c.Workdir)
			}
			args = append(args, container)
			switch c.Operation {
			case "ReadFile":
				args = append(args, "cat", c.Path)
			case "Exec":
				args = append(args, shell, "-c", c.Script)
			default:
				return core.CommandResult{}, fmt.Errorf("unsupported WildClaw operation %q", c.Operation)
			}
			output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
			rc := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) && ctx.Err() == nil {
				rc = exit.ExitCode()
				err = nil
			}
			return core.CommandResult{ExitCode: rc, Stdout: string(output)}, err
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
