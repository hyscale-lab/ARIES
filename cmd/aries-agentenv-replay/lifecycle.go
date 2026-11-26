package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/hyscale-lab/aries/pkg/replay/harbor"
)

var (
	checkpointHelper = flag.String("checkpoint-helper", "", "stock/checkpoint.py on the worker")
	stockServiceDir  = flag.String("stock-service-dir", "", "private stock service directory")
	checkpointDir    = flag.String("checkpoint-dir", "", "pilot output containing bindings.json")
	initialSnapshot  = flag.String("initial-snapshot", "", "published snapshot used for the initial restore")
)

type stockLifecycle struct {
	g        *guest
	snapshot string
	pending  bool
}

func newLifecycle(g *guest) (*stockLifecycle, error) {
	if *checkpointHelper == "" && *stockServiceDir == "" && *checkpointDir == "" && *initialSnapshot == "" {
		return nil, nil
	}
	if *checkpointHelper == "" || *stockServiceDir == "" || *checkpointDir == "" || *initialSnapshot == "" {
		return nil, errors.New("checkpoint replay requires helper, stock service directory, checkpoint directory, and initial snapshot")
	}
	return &stockLifecycle{g: g, snapshot: *initialSnapshot}, nil
}

// Checkpoint sends the exit confirmation before waiting for manifest publication.
// Only the private stdout pipe carries the replacement envd access token.
func (l *stockLifecycle) invoke(ctx context.Context, phase string, step int, suspended func() error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", *checkpointHelper, phase, *stockServiceDir, *checkpointDir,
		l.g.url, l.g.sandbox, l.snapshot, strconv.Itoa(step))
	cmd.Stderr = os.Stderr
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	dec := json.NewDecoder(output)
	if phase == "checkpoint" {
		var msg struct{ Suspended bool }
		err = dec.Decode(&msg)
		if err == nil && !msg.Suspended {
			err = errors.New("missing source VMM exit confirmation")
		}
		if err == nil {
			err = suspended()
		}
	}
	var done struct{ Snapshot, Sandbox, Token string }
	if err == nil {
		err = dec.Decode(&done)
	}
	if err == nil {
		var extra json.RawMessage
		if end := dec.Decode(&extra); end != io.EOF {
			err = errors.New("unexpected stock lifecycle helper output")
		}
	}
	if err != nil {
		cancel()
	}
	if err = errors.Join(err, cmd.Wait()); err != nil {
		return fmt.Errorf("stock %s: %w", phase, err)
	}
	switch phase {
	case "checkpoint":
		if done.Snapshot == "" || done.Snapshot == l.snapshot {
			return errors.New("expected a newly published snapshot")
		}
		l.snapshot, l.pending = done.Snapshot, true
	case "restore", "inspection":
		if done.Snapshot != l.snapshot || done.Sandbox == "" || done.Sandbox == l.g.sandbox || done.Token == "" {
			return errors.New("incomplete replacement sandbox identity")
		}
		l.g.sandbox, l.g.token, l.pending = done.Sandbox, done.Token, false
	}
	return nil
}

func (l *stockLifecycle) hooks() *harbor.CheckpointLifecycle {
	if l == nil {
		return nil
	}
	return &harbor.CheckpointLifecycle{
		Checkpoint: func(ctx context.Context, step int, suspended func() error) error {
			return l.invoke(ctx, "checkpoint", step, suspended)
		},
		Restore: func(ctx context.Context, step int) error { return l.invoke(ctx, "restore", step, nil) },
	}
}

func (l *stockLifecycle) finish(ctx context.Context, periodic func(context.Context, string, int, []string) (harbor.Freeze, error)) error {
	if l == nil {
		return nil
	}
	if l.pending {
		if err := l.invoke(ctx, "inspection", 0, nil); err != nil {
			return err
		}
		if periodic != nil {
			if _, err := periodic(ctx, "session-end", 0, []string{"session-end"}); err != nil {
				return err
			}
		}
	}
	if err := l.invoke(ctx, "audit", 0, nil); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*checkpointDir, "final-sandbox-id"), []byte(l.g.sandbox+"\n"), 0600)
}
