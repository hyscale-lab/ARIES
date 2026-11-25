package harbor

import (
	"context"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestReplayClockCountsOneFreezeAndOnlyExposedExport(t *testing.T) {
	c := newReplayClock(100)
	c.windows = []traceWindow{{begin: 101, end: 115, freeze: Freeze{PauseAckNS: 103, ResumeRequestNS: 108}}}
	if at := c.runnable(105); at != 3 {
		t.Fatalf("clock advanced during freeze: %d", at)
	}
	if at := c.runnable(109); at != 4 {
		t.Fatalf("clock failed to resume: %d", at)
	}
	if wall := c.wallDeadline(7); wall != 112 {
		t.Fatalf("deadline did not preserve runnable wait: %d", wall)
	}
	var blocks []Receipt
	h := Hooks{clock: c, Record: func(event Receipt) error {
		blocks = append(blocks, event)
		return nil
	}}
	hidden := *c
	light := h
	light.clock = &hidden
	if err := light.markTraceBlocked(12, 117, "tool", 1); err != nil || len(blocks) != 0 {
		t.Fatalf("export fully hidden by the wait was deducted: %v, %v", blocks, err)
	}
	if err := h.markTraceBlocked(7, 115, "tool", 1); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Time != 112 || blocks[0].CaptureEndNS != 115 {
		t.Fatalf("blocked export differs from deadline overrun: %v", blocks)
	}
	if err := h.markTraceBlocked(10, 120, "tool", 2); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 {
		t.Fatalf("earlier blockage counted twice: %v", blocks)
	}
}

func TestPeriodicCapturesInToolAndWait(t *testing.T) {
	begin, err := Clock()
	if err != nil {
		t.Fatal(err)
	}
	var captures []Receipt
	c := newReplayClock(begin)
	c.interval = 40 * time.Millisecond
	h := Hooks{
		clock: c,
		Exec: func(ctx context.Context, _ replay.Call) (core.CommandResult, error) {
			timer := time.NewTimer(70 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return core.CommandResult{}, ctx.Err()
			case <-timer.C:
				return core.CommandResult{}, nil
			}
		},
		Periodic: func(context.Context, string, int, []string) (Freeze, error) {
			pause, _ := Clock()
			time.Sleep(10 * time.Millisecond)
			resume, _ := Clock()
			return Freeze{PauseAckNS: pause, ResumeRequestNS: resume}, nil
		},
		Record: func(e Receipt) error {
			captures = append(captures, e)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := h.boundary(ctx, "pre-tool", 1, begin); err != nil {
		t.Fatal(err)
	}
	_, reply, err := h.executeTool(ctx, replay.Call{}, Receipt{Step: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.boundary(ctx, "post-tool", 1, reply); err != nil {
		t.Fatal(err)
	}
	target := c.runnable(reply) + int64(70*time.Millisecond)
	end, err := h.waitRunnable(ctx, 1, target)
	if err != nil {
		t.Fatal(err)
	}
	if c.runnable(end) < target {
		t.Fatal("runnable wait ended early")
	}
	if c.runnable(end) > target+int64(30*time.Millisecond) {
		t.Fatal("runnable wait included extra idle time")
	}
	phases := map[string]int{}
	for _, e := range captures {
		phases[e.Phase]++
	}
	if phases["tool"] == 0 || phases["wait"] == 0 {
		t.Fatal(phases)
	}
	if err := h.boundary(ctx, "session-end", 0, c.wallDeadline(c.next)); err != nil {
		t.Fatal(err)
	}
	last := captures[len(captures)-1]
	if len(last.Triggers) != 2 || last.Triggers[0] != "session-end" || last.Triggers[1] != "timer" {
		t.Fatalf("coincident transition and timer used separate captures: %v", last)
	}
}

func TestParallelScheduleUsesRunnableSpacing(t *testing.T) {
	begin, err := Clock()
	if err != nil {
		t.Fatal(err)
	}
	spacing := 70 * time.Millisecond
	sourceStart := time.Now()
	r := &Recording{Plan: replay.Plan{Calls: []replay.Call{{Sequence: 1}, {Sequence: 2}}}}
	r.Events = []Event{
		{Kind: "exec_command", Step: 1, ParallelSize: 2, Sequence: 1, SourceTime: sourceStart},
		{Kind: "exec_command", Step: 1, ParallelSize: 2, Sequence: 2, SourceTime: sourceStart.Add(spacing)},
		{Kind: "function_call_output", Step: 1, ParallelSize: 2, Sequence: 1},
		{Kind: "function_call_output", Step: 1, ParallelSize: 2, Sequence: 2},
	}
	var dispatches []Receipt
	var periodic int
	c := newReplayClock(begin)
	c.interval = 40 * time.Millisecond
	h := Hooks{
		clock: c,
		Exec: func(ctx context.Context, _ replay.Call) (core.CommandResult, error) {
			timer := time.NewTimer(90 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return core.CommandResult{}, ctx.Err()
			case <-timer.C:
				return core.CommandResult{}, nil
			}
		},
		Periodic: func(_ context.Context, phase string, _ int, _ []string) (Freeze, error) {
			if phase != "parallel-tool" {
				return Freeze{}, nil
			}
			periodic++
			pause, _ := Clock()
			time.Sleep(20 * time.Millisecond)
			resume, _ := Clock()
			return Freeze{PauseAckNS: pause, ResumeRequestNS: resume}, nil
		},
		Record: func(event Receipt) error {
			if event.Event == "tool_dispatch" {
				dispatches = append(dispatches, event)
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := runParallel(ctx, h, r, r.Events, Receipt{Step: 1, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if periodic == 0 || len(dispatches) != 2 {
		t.Fatalf("periodic=%d dispatch count=%d", periodic, len(dispatches))
	}
	first, second := dispatches[0], dispatches[1]
	if modeled := *second.RunnableTargetNS - *first.RunnableActualNS; modeled != int64(spacing) {
		t.Fatalf("modeled sibling spacing = %s", time.Duration(modeled))
	}
	if wall := time.Duration(second.Time - first.Time); wall < spacing+20*time.Millisecond {
		t.Fatalf("wall sibling spacing = %s", wall)
	}
}
