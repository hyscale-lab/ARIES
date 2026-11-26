package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

//go:embed codex_group.py
var codexGroupProgram string

const codexGroupPath = "/usr/local/libexec/aries-codex-group.py"

type shellGroups struct {
	prefix            string
	onlyInterruptible bool
	interruptible     map[int]bool
	lock              sync.Mutex
	released          map[string]func() error
	timings           []map[string]any
}

func newShellGroups() (*shellGroups, error) {
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return nil, err
	}
	return &shellGroups{prefix: hex.EncodeToString(key[:]), released: make(map[string]func() error)}, nil
}

func (g *shellGroups) token(c replay.Call) string {
	if c.Operation == "Exec" {
		return fmt.Sprintf("%s-e%d", g.prefix, c.Sequence)
	}
	return fmt.Sprintf("%s-p%d", g.prefix, c.SessionID)
}

func (g *shellGroups) start(command core.Command, call replay.Call) core.Command {
	if !g.owns(call) {
		return command
	}
	mode := "start"
	if g.interruptible[call.SessionID] {
		mode = "start-interruptible"
	}
	args := []string{"python3", "-I", "-S", codexGroupPath, mode, g.token(call), command.Path}
	command.Path = "/usr/bin/env"
	command.Args = append(args, command.Args...)
	return command
}

func (g *shellGroups) owns(c replay.Call) bool {
	return g != nil && (!g.onlyInterruptible || g.interruptible[c.SessionID])
}

type groupCommand func(context.Context, core.Command) (core.CommandResult, error)
type shellCall func(context.Context, replay.Call) (core.CommandResult, error)

func (g *shellGroups) interrupt(ctx context.Context, guest groupCommand, c replay.Call) error {
	if g == nil || !g.interruptible[c.SessionID] {
		return fmt.Errorf("no native interrupt group for source handle %d", c.SessionID)
	}
	result, err := guest(ctx, core.Command{Path: "/usr/bin/env", Args: []string{"python3", "-I", "-S", codexGroupPath, "interrupt", g.token(c)}})
	if err != nil {
		return fmt.Errorf("interrupt guest process group: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("interrupt guest process group exited %d: %s", result.ExitCode, result.Stderr)
	}
	return nil
}

func installCodexGroups(ctx context.Context, guest groupCommand) (string, error) {
	const script = "import hashlib,pathlib,sys; p=pathlib.Path(sys.argv[1]); p.parent.mkdir(exist_ok=True); f=p.open('xb'); f.write(sys.argv[2].encode()); f.close(); print(hashlib.sha256(p.read_bytes()).hexdigest())"
	out, err := guest(ctx, core.Command{Path: "/usr/bin/env", Args: []string{"python3", "-I", "-S", "-c", script, codexGroupPath, codexGroupProgram}})
	if err != nil {
		return "", err
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(codexGroupProgram)))
	if out.ExitCode != 0 || strings.TrimSpace(out.Stdout) != sum {
		return "", fmt.Errorf("guest process-group helper readback failed: %s", out.Stderr)
	}
	return sum, nil
}

// The real completion of a handle releases its original Linux process group.
// A running poll retains the group. A diagnostic may later read a saved exit.
func (g *shellGroups) releaseOnExit(run shellCall, guest groupCommand) shellCall {
	return func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
		out, err := run(ctx, c)
		if err != nil || out.ExitCode == harbor.ProcessRunning || !g.owns(c) || (c.Operation != "Exec" && c.Operation != "StartProcess" && c.Operation != "PollProcess") {
			return out, err
		}
		token := g.token(c)
		g.lock.Lock()
		release, ok := g.released[token]
		if !ok {
			release = sync.OnceValue(func() error {
				started := time.Now()
				result, err := guest(ctx, core.Command{Path: "/usr/bin/env", Args: []string{"python3", "-I", "-S", codexGroupPath, "release", token}})
				g.lock.Lock()
				g.timings = append(g.timings, map[string]any{"sequence": c.Sequence, "source_call_id": c.ToolCallID, "cleanup_ns": time.Since(started).Nanoseconds(), "exit_code": result.ExitCode})
				g.lock.Unlock()
				if err != nil {
					return fmt.Errorf("release guest process group: %w", err)
				}
				if result.ExitCode != 0 {
					return fmt.Errorf("release guest process group exited %d: %s", result.ExitCode, result.Stderr)
				}
				return nil
			})
			g.released[token] = release
		}
		g.lock.Unlock()
		return out, release()
	}
}
