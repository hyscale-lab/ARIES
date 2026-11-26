package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

//go:embed crab/*.py
var crabScripts embed.FS

var crabRunName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,80}$`)

func crabCalendar(ctx context.Context, trace, arm, run, profile string) error {
	if !crabRunName.MatchString(run) || (arm != "checkpoint" && arm != "control") || (profile != "off" && profile != "on") {
		return errors.New("Crab requires a new -crab-run name, -crab-arm checkpoint|control, and -crab-profile off|on")
	}
	data, err := os.ReadFile(trace)
	if err != nil {
		return err
	}
	recording, err := harbor.LoadWildClawCalendar(data)
	if err != nil {
		return err
	}

	dir, err := os.MkdirTemp("", "aries-crab-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	for _, name := range []string{"host.py", "action.py", "inner.py", "profile.py"} {
		source, err := crabScripts.ReadFile("crab/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), source, 0600); err != nil {
			return err
		}
	}
	host := exec.Command("python3", filepath.Join(dir, "host.py"), arm, run, profile)
	host.Stderr = os.Stderr
	ready, err := host.StdoutPipe()
	if err != nil {
		return err
	}
	done, err := host.StdinPipe()
	if err != nil {
		return err
	}
	if err := host.Start(); err != nil {
		return err
	}
	var state struct{ Sandbox string }
	err = json.NewDecoder(ready).Decode(&state)
	if err == nil && !crabRunName.MatchString(state.Sandbox) {
		err = errors.New("invalid sandbox identity from Crab")
	}
	if err == nil {
		var log *os.File
		log, err = os.OpenFile(filepath.Join(run, "replay.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			err = crabReplay(ctx, recording, state.Sandbox, log)
			err = errors.Join(err, log.Close())
		}
	}
	if err == nil {
		_, err = io.WriteString(done, "ok\n")
	}
	_ = done.Close()
	return errors.Join(err, host.Wait())
}

// Execute each live call through the stock Crab SDK; harbor.Run owns the replay clock and dispatch.
func crabReplay(ctx context.Context, recording *harbor.Recording, sandbox string, log io.Writer) error {
	script, err := crabScripts.ReadFile("crab/action.py")
	if err != nil {
		return err
	}
	enc := json.NewEncoder(log)
	return harbor.Run(ctx, recording, harbor.Hooks{
		Boundary: func(context.Context, string, int) error { return nil },
		Record:   func(r harbor.Receipt) error { return enc.Encode(r) },
		Exec: func(ctx context.Context, call replay.Call) (core.CommandResult, error) {
			payload := call.Script
			if call.Operation == "ReadFile" {
				payload = call.Path
			} else if call.Operation != "Exec" {
				return core.CommandResult{}, fmt.Errorf("unsupported Crab operation %q", call.Operation)
			}
			if call.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, call.Timeout)
				defer cancel()
			}
			args := []string{"sudo", "/opt/crab/venv/bin/python", "-", "exec", sandbox, call.Workdir, call.Operation, payload}
			for i, s := range args {
				args[i] = "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
			}
			cmd := exec.CommandContext(ctx, "./guest", strings.Join(args, " "))
			cmd.Stdin = bytes.NewReader(script)
			output, err := cmd.CombinedOutput()
			rc := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) && ctx.Err() == nil {
				rc = exit.ExitCode()
				err = nil
			}
			return core.CommandResult{ExitCode: rc, Stdout: string(output)}, err
		},
	})
}
