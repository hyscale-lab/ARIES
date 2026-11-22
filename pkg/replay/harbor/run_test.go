package harbor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestReplyRelativeScheduleAndRetainedState(t *testing.T) {
	dir := t.TempDir()
	r := &Recording{Plan: replay.Plan{Calls: []replay.Call{
		{Sequence: 1, Script: "printf preserved > value; exit 1", ExitCode: 1},
		{Sequence: 2, Script: "cat value", ResponseStdout: []byte("preserved")},
	}}, Events: []Event{{Kind: "exec_command", Step: 1, Sequence: 1}, {Kind: "function_call_output", Step: 1}, {Kind: "exec_command", Wait: time.Millisecond * 3, Step: 2, Sequence: 2}}}
	var order []string
	var records []Receipt
	h := Hooks{
		Boundary: func(_ context.Context, phase string, _ int) error { order = append(order, phase); return nil },
		Record:   func(r Receipt) error { records = append(records, r); return nil },
		Exec: func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
			order = append(order, "exec")
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", c.Script)
			cmd.Dir = dir
			output, err := cmd.Output()
			var failed *exec.ExitError
			if errors.As(err, &failed) {
				err = nil
			}
			return core.CommandResult{Stdout: string(output), ExitCode: cmd.ProcessState.ExitCode()}, err
		},
	}
	if err := Run(t.Context(), r, h); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"session-start", "pre-tool", "exec", "post-tool", "pre-tool", "exec", "post-tool", "session-end"}) {
		t.Fatal(order)
	}
	var firstReply, secondDispatch int64
	for _, v := range records {
		if v.Event == "tool_reply" && v.Sequence == 1 {
			firstReply = v.Time
		}
		if v.Event == "tool_dispatch" && v.Sequence == 2 {
			secondDispatch = v.Time
		}
		if v.Event == "tool_reply" && v.Sequence == 2 && (v.Stdout != "preserved" || v.OutputComparison != "matched_ignoring_trailing_newlines") {
			t.Fatal(v)
		}
	}
	if firstReply == 0 || secondDispatch-firstReply < int64(3*time.Millisecond) {
		t.Fatal(records)
	}
	if records[0].Event != "session_start" || records[len(records)-1].Event != "session_end" {
		t.Fatal(records)
	}
}

func TestRejectedSourceAndObserverCleanup(t *testing.T) {
	if _, err := Load([]byte("arbitrary"), nil); err == nil {
		t.Fatal("accepted unreviewed recording")
	}
	var phases []string
	err := Run(t.Context(), &Recording{}, Hooks{
		Record: func(Receipt) error { return nil },
		Exec: func(context.Context, replay.Call) (core.CommandResult, error) {
			t.Fatal("unexpected dispatch")
			return core.CommandResult{}, nil
		},
		Boundary: func(_ context.Context, phase string, _ int) error {
			phases = append(phases, phase)
			if phase == "session-start" {
				return os.ErrPermission
			}
			return nil
		},
	})
	if !errors.Is(err, os.ErrPermission) || !reflect.DeepEqual(phases, []string{"session-start", "session-end"}) {
		t.Fatal(err, phases)
	}
}
