package harbor

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestParallelStepStartsBothLiveOperationsBeforeDependentStep(t *testing.T) {
	t0 := time.Now()
	r := &Recording{Plan: replay.Plan{Calls: []replay.Call{{Sequence: 1}, {Sequence: 2}, {Sequence: 3}}}}
	r.Events = []Event{
		{Kind: "exec_command", Step: 5, ParallelSize: 2, Sequence: 1, SourceTime: t0},
		{Kind: "exec_command", Step: 5, ParallelSize: 2, Sequence: 2, SourceTime: t0.Add(time.Millisecond)},
		{Kind: "function_call_output", Step: 5, ParallelSize: 2, Sequence: 1},
		{Kind: "function_call_output", Step: 5, ParallelSize: 2, Sequence: 2},
		{Kind: "exec_command", Step: 6, Sequence: 3},
	}
	if err := validateParallelGroups(r); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var lock sync.Mutex
	started, returned := 0, 0
	both := make(chan struct{})
	var captures []string
	h := Hooks{
		Boundary: func(_ context.Context, phase string, _ int) error { captures = append(captures, phase); return nil },
		Record:   func(Receipt) error { return nil },
		Exec: func(ctx context.Context, c replay.Call) (core.CommandResult, error) {
			if c.Sequence == 3 {
				lock.Lock()
				defer lock.Unlock()
				if returned != 2 {
					t.Errorf("dependent call preceded live replies")
				}
				return core.CommandResult{}, nil
			}
			lock.Lock()
			started++
			if started == 2 {
				close(both)
			}
			lock.Unlock()
			select {
			case <-both:
			case <-ctx.Done():
				return core.CommandResult{}, ctx.Err()
			}
			lock.Lock()
			returned++
			lock.Unlock()
			return core.CommandResult{}, nil
		},
	}
	if err := Run(ctx, r, h); err != nil {
		t.Fatal(err)
	}
	if len(captures) != 6 {
		t.Fatal(captures)
	}
}

func TestWildClawSerializesFirstRepliesWithinTurn(t *testing.T) {
	r := &Recording{OpenClawTrace: &OpenClawTrace{}, Plan: replay.Plan{Calls: []replay.Call{
		{Sequence: 1, Operation: "StartProcess", SessionID: 51, ExitCode: ProcessRunning},
		{Sequence: 2, Operation: "PollProcess", SessionID: 51, ExitCode: ProcessRunning},
		{Sequence: 3, Operation: "PollProcess", SessionID: 51},
		{Sequence: 4, Operation: "ReadFile"},
	}}, Events: []Event{
		{Step: 1, Sequence: 1, ParallelSize: 2}, {Step: 1, Sequence: 2, ParallelSize: 2},
		{Step: 1, Sequence: 1, Kind: "toolResult", ParallelSize: 2}, {Step: 1, Sequence: 2, Kind: "toolResult", ParallelSize: 2},
		{Step: 2, Sequence: 3, ParallelSize: 2}, {Step: 2, Sequence: 4, ParallelSize: 2},
		{Step: 2, Sequence: 3, Kind: "toolResult", ParallelSize: 2}, {Step: 2, Sequence: 4, Kind: "toolResult", ParallelSize: 2},
		{Step: 3, Wait: time.Millisecond},
	}}
	if err := validateParallelGroups(r); err != nil {
		t.Fatal(err)
	}
	for _, checkpoint := range []bool{false, true} {
		t.Run(fmt.Sprintf("checkpoint=%t", checkpoint), func(t *testing.T) {
			var invocations []string
			var requests, replies [4]int64
			var checkpoints, postTools []int
			h := Hooks{
				RecordToolRequests: true,
				Record: func(e Receipt) error {
					switch e.Event {
					case "session_start":
						if !e.SerialSiblings {
							t.Error("missing sibling schedule")
						}
					case "tool_request":
						requests[e.Sequence-1] = e.Time
					case "tool_dispatch", "tool_reply":
						invocations = append(invocations, fmt.Sprintf("%s/%d", e.Event, e.Sequence))
						if e.Event == "tool_reply" {
							replies[e.Sequence-1] = e.Time
						} else if e.Sequence%2 == 0 && e.Time < replies[e.Sequence-2] {
							t.Errorf("sibling %d dispatched before first reply", e.Sequence)
						}
					case "checkpoint_start":
						checkpoints = append(checkpoints, e.Step)
						if replies[3] == 0 || e.Time < replies[3] {
							t.Error("checkpoint before final sibling reply")
						}
					}
					return nil
				},
				Boundary: func(_ context.Context, phase string, step int) error {
					if phase == "post-tool" {
						postTools = append(postTools, step)
					}
					return nil
				},
				Exec: func(_ context.Context, c replay.Call) (core.CommandResult, error) {
					return core.CommandResult{ExitCode: int(c.ExitCode)}, nil
				},
			}
			if checkpoint {
				h.Lifecycle = &CheckpointLifecycle{
					Checkpoint: func(_ context.Context, _ int, suspended func() error) error { return suspended() },
					Restore:    func(context.Context, int) error { return nil },
				}
			}
			if err := Run(t.Context(), r, h); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(invocations, []string{"tool_dispatch/1", "tool_reply/1", "tool_dispatch/2", "tool_reply/2",
				"tool_dispatch/3", "tool_reply/3", "tool_dispatch/4", "tool_reply/4"}) {
				t.Fatal(invocations)
			}
			if requests[0] == 0 || requests[0] != requests[1] || requests[2] < replies[1] || requests[2] != requests[3] {
				t.Fatalf("group requests %v, first replies %v", requests, replies)
			}
			if !slices.Equal(postTools, []int{1, 2}) || checkpoint && !slices.Equal(checkpoints, []int{2}) {
				t.Fatalf("post-tool steps %v, checkpoint steps %v", postTools, checkpoints)
			}
		})
	}
}
