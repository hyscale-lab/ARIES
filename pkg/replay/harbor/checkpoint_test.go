package harbor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestCheckpointOverlapsThinkingAndChargesRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	// The second action is a batch. Keep pack work in flight until the host
	// actually records both requests, even if the test machine stalls.
	r := &Recording{Plan: replay.Plan{Calls: []replay.Call{
		{Sequence: 1}, {Sequence: 2}, {Sequence: 3},
	}}, Events: []Event{
		{Step: 1, Sequence: 1},
		{Step: 2, Sequence: 2, Wait: 5 * time.Millisecond, ParallelSize: 2},
		{Step: 2, Sequence: 3, ParallelSize: 2},
		{Step: 2, Sequence: 2, Kind: "function_call_output", ParallelSize: 2},
		{Step: 2, Sequence: 3, Kind: "function_call_output", ParallelSize: 2},
		{Step: 3, Wait: 5 * time.Millisecond},
	}}
	requested := make(chan struct{})
	stamps := make(map[string]int64)
	var restores int
	h := Hooks{
		Record: func(r Receipt) error {
			stamps[fmt.Sprintf("%s/%d/%d", r.Event, r.Step, r.Sequence)] = r.Time
			if r.Event == "tool_request" && r.Sequence == 3 {
				close(requested)
			}
			return nil
		},
		Periodic: func(context.Context, string, int, []string) (Freeze, error) { return Freeze{}, nil },
		Exec:     func(context.Context, replay.Call) (core.CommandResult, error) { return core.CommandResult{}, nil },
		Lifecycle: &CheckpointLifecycle{
			Checkpoint: func(ctx context.Context, step int, suspended func() error) error {
				if err := suspended(); err != nil {
					return err
				}
				if step == 1 {
					select {
					case <-requested:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			},
			Restore: func(context.Context, int) error { restores++; return nil },
		},
	}
	if err := Run(ctx, r, h); err != nil {
		t.Fatal(err)
	}
	req := stamps["tool_request/2/2"]
	if req == 0 || req != stamps["tool_request/2/3"] || restores != 1 || stamps["session_end_suspended/0/0"] == 0 {
		t.Fatal(restores, stamps)
	}
	if begin := stamps["tool_reply/1/1"]; begin == 0 || req-begin < int64(5*time.Millisecond) {
		t.Fatalf("shortened thinking deadline: %v", stamps)
	}
	for _, sequence := range []int{2, 3} {
		path := []int64{stamps["checkpoint_start/1/0"], req, stamps["checkpoint_end/1/0"], stamps["restore_start/2/0"], stamps["restore_end/2/0"],
			stamps[fmt.Sprintf("tool_dispatch/2/%d", sequence)], stamps[fmt.Sprintf("tool_reply/2/%d", sequence)]}
		for i := 1; i < len(path); i++ {
			if path[i-1] <= 0 || path[i] < path[i-1] {
				t.Fatalf("incorrect latency boundaries: %v", path)
			}
		}
	}
}

func TestCheckpointWaitsForAllBatchHandlesAndBoundedPoll(t *testing.T) {
	for _, finish := range []int{0, ProcessRunning} {
		t.Run(fmt.Sprint(finish), func(t *testing.T) {
			r := &Recording{Plan: replay.Plan{Calls: []replay.Call{
				{Sequence: 1, Operation: "StartProcess", SessionID: 51, ExitCode: ProcessRunning},
				{Sequence: 2, Operation: "StartProcess", SessionID: 52, ExitCode: ProcessRunning},
				{Sequence: 3, Operation: "PollProcess", SessionID: 52},
				{Sequence: 4, Operation: "PollProcess", SessionID: 51},
				{Sequence: 5, Operation: "Exec"},
			}}, Events: []Event{
				{Step: 1, Sequence: 1, ParallelSize: 2}, {Step: 1, Sequence: 2, ParallelSize: 2},
				{Step: 1, Sequence: 1, Kind: "function_call_output", ParallelSize: 2},
				{Step: 1, Sequence: 2, Kind: "function_call_output", ParallelSize: 2},
				{Step: 2, Sequence: 3, Wait: time.Millisecond}, {Step: 3, Sequence: 4, Wait: time.Millisecond},
				{Step: 4, Sequence: 5, Wait: time.Millisecond},
			}}
			var checkpoints []int
			h := Hooks{
				Boundary: func(context.Context, string, int) error { return nil },
				Record:   func(Receipt) error { return nil },
				Exec: func(_ context.Context, c replay.Call) (core.CommandResult, error) {
					if c.Sequence == 4 {
						return core.CommandResult{ExitCode: ProcessRunning}, nil
					}
					return core.CommandResult{ExitCode: int(c.ExitCode)}, nil
				},
				KeepBoundedPollResult: true,
				FinishObservedProcess: func(context.Context, replay.Call) (core.CommandResult, error) {
					return core.CommandResult{ExitCode: finish}, nil
				},
				Lifecycle: &CheckpointLifecycle{
					Checkpoint: func(_ context.Context, step int, suspended func() error) error {
						checkpoints = append(checkpoints, step)
						return suspended()
					},
					Restore: func(context.Context, int) error { return nil },
				},
			}
			if err := Run(t.Context(), r, h); err != nil {
				t.Fatal(err)
			}
			if finish == ProcessRunning && len(checkpoints) != 0 || finish == 0 && (len(checkpoints) != 1 || checkpoints[0] != 3) {
				t.Fatal(checkpoints)
			}
		})
	}
}

func TestTraceTimerStopsDuringSuspension(t *testing.T) {
	begin, _ := Clock()
	c := newReplayClock(begin)
	c.interval = time.Millisecond
	var waits int
	h := Hooks{
		clock: c, lifecycle: &checkpointState{}, Record: func(Receipt) error { return nil },
		Periodic: func(_ context.Context, phase string, _ int, triggers []string) (Freeze, error) {
			if phase == "wait" {
				waits++
			}
			return Freeze{}, nil
		},
		Lifecycle: &CheckpointLifecycle{
			Checkpoint: func(_ context.Context, _ int, suspended func() error) error { return suspended() },
			Restore:    func(context.Context, int) error { return nil },
		},
	}
	if err := h.boundary(t.Context(), "post-tool", 1, begin); err != nil {
		t.Fatal(err)
	}
	if err := h.checkpoint(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	<-h.lifecycle.pending.done
	target := c.runnable(h.lifecycle.pending.suspended) + int64(5*c.interval)
	if _, err := h.waitRunnable(t.Context(), 1, target); err != nil || waits != 0 {
		t.Fatal(err, waits)
	}
	if err := h.prepareTools(t.Context(), []Receipt{{Step: 2}}); err != nil {
		t.Fatal(err)
	}
	target = c.next + int64(3*c.interval)
	if _, err := h.waitRunnable(t.Context(), 2, target); err != nil || waits == 0 {
		t.Fatal(err, waits)
	}
}

func TestCheckpointErrorInterruptsThinking(t *testing.T) {
	want := errors.New("checkpoint failed")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	r := &Recording{Plan: replay.Plan{Calls: []replay.Call{{Sequence: 1}}},
		Events: []Event{{Step: 1, Sequence: 1}, {Step: 2, Wait: time.Hour}}}
	h := Hooks{
		Exec:     func(context.Context, replay.Call) (core.CommandResult, error) { return core.CommandResult{}, nil },
		Record:   func(Receipt) error { return nil },
		Boundary: func(context.Context, string, int) error { return nil },
		Lifecycle: &CheckpointLifecycle{
			Checkpoint: func(context.Context, int, func() error) error { return want },
			Restore:    func(context.Context, int) error { return nil },
		},
	}
	if err := Run(ctx, r, h); !errors.Is(err, want) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestCheckpointReleasesOnlyFinishedUnnamedStream(t *testing.T) {
	s := NewProcessSessions(t.Context(), nil)
	ended := make(chan struct{})
	close(ended)
	s.active[-1] = &guestProcess{done: ended}
	s.active[-2] = &guestProcess{done: make(chan struct{})}
	s.active[-3] = &guestProcess{done: ended, err: errors.New("lost stream")}
	s.active[7] = &guestProcess{done: ended}
	h := Hooks{lifecycle: &checkpointState{handles: map[int]bool{-1: true, -2: true, -3: true, 7: true}}, UnnamedFinished: s.UnnamedFinished}
	if err := h.startCheckpoint(t.Context(), 1, nil); err != nil {
		t.Fatal(err)
	}
	handles := h.lifecycle.handles
	if len(handles) != 3 || !handles[-2] || !handles[-3] || !handles[7] {
		t.Fatal(handles)
	}
}
