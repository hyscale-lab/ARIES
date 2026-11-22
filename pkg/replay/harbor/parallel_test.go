package harbor

import (
	"context"
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
