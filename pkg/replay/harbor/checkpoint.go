package harbor

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

// Checkpoint returns after all snapshot/pack work has finished. It must call
// suspended synchronously once when the provider confirms the VMM has stopped.
// Restore returns when the replacement guest can accept the next dispatch.
type CheckpointLifecycle struct {
	Checkpoint func(ctx context.Context, step int, suspended func() error) error
	Restore    func(ctx context.Context, step int) error
}

type checkpointState struct {
	handles map[int]bool
	pending *pendingCheckpoint
}

type pendingCheckpoint struct {
	done      chan struct{}
	cancel    context.CancelFunc
	suspended int64
	err       error
}

func (h Hooks) observeHandle(call replay.Call, out core.CommandResult, err error) {
	if h.lifecycle == nil || err != nil {
		return
	}
	id := call.SessionID
	if call.Operation == "Exec" {
		id = diagnosticProcessID(call)
	}
	if id == 0 {
		return
	}
	if out.ExitCode == ProcessRunning {
		h.lifecycle.handles[id] = true
	} else {
		delete(h.lifecycle.handles, id)
	}
}

func (h Hooks) startCheckpoint(ctx context.Context, step int, following []Event) error {
	if h.lifecycle == nil {
		return nil
	}
	for id := range h.lifecycle.handles {
		if id < 0 && h.UnnamedFinished != nil && h.UnnamedFinished(id) {
			delete(h.lifecycle.handles, id)
		}
	}
	if len(h.lifecycle.handles) != 0 {
		return nil
	}
	// No transition to thinking remains after the last reply in some recordings.
	for _, e := range following {
		if e.Wait > 0 || (e.Sequence != 0 && !e.isToolReply()) {
			return h.checkpoint(ctx, step)
		}
	}
	return nil
}

func (h Hooks) checkpoint(ctx context.Context, step int) error {
	start, err := Clock()
	if err != nil {
		return err
	}
	if err := h.Record(Receipt{Event: "checkpoint_start", Step: step, Time: start}); err != nil {
		return err
	}
	// Stock pause owns the VMM exclusively; it does not expose the instant the
	// guest stops running inside the operation. Bound any unobservable timer.
	var nextTrace int64
	if h.Periodic != nil {
		nextTrace = h.clock.wallDeadline(h.clock.next)
	}
	work, cancel := context.WithCancel(ctx)
	p := &pendingCheckpoint{done: make(chan struct{}), cancel: cancel}
	h.lifecycle.pending = p
	go func() {
		defer close(p.done)
		defer cancel()
		suspended := func() error {
			if p.suspended != 0 {
				return errors.New("duplicate checkpoint suspension")
			}
			at, err := Clock()
			if err != nil {
				return err
			}
			p.suspended = at
			return errors.Join(h.Record(Receipt{Event: "checkpoint_suspended", Step: step, Time: at}),
				h.unknownTimer("checkpoint", step, start, at, nextTrace))
		}
		p.err = h.Lifecycle.Checkpoint(work, step, suspended)
		if p.err == nil && p.suspended == 0 {
			p.err = errors.New("checkpoint completed without confirmed suspension")
		}
		p.err = errors.Join(p.err, h.lifecycleEnd("checkpoint_end", step, p.err))
	}()
	return nil
}

func (h Hooks) unknownTimer(phase string, step int, start, end, deadline int64) error {
	if deadline == 0 || end < deadline {
		return nil
	}
	return h.Record(Receipt{Event: "trace_timer_coverage_unknown", Phase: phase, Step: step,
		Time: max(start, deadline), CaptureEndNS: end, Basis: "guest_runnable_state_unobserved_during_native_operation"})
}

func (h Hooks) lifecycleEnd(event string, step int, err error) error {
	receipt := Receipt{Event: event, Step: step}
	if err != nil {
		receipt.NativeError = err.Error()
	}
	return emit(h, receipt)
}

func (h Hooks) prepareTools(ctx context.Context, requests []Receipt) error {
	ready, err := Clock()
	if err != nil {
		return err
	}
	step := requests[0].Step
	if h.RecordToolRequests {
		// All calls in the assistant turn arrive together, regardless of their dispatch schedule.
		for _, r := range requests {
			r.Event, r.Time = "tool_request", ready
			if err := h.Record(r); err != nil {
				return err
			}
		}
	}
	if h.lifecycle != nil && h.lifecycle.pending != nil {
		p := h.lifecycle.pending
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
		}
		if p.err != nil {
			return p.err
		}
		start, err := Clock()
		if err != nil {
			return err
		}
		if err := h.Record(Receipt{Event: "restore_start", Time: start, Step: step}); err != nil {
			return err
		}
		err = h.Lifecycle.Restore(ctx, step)
		if err == nil {
			h.lifecycle.pending = nil
		}
		if err := errors.Join(err, h.lifecycleEnd("restore_end", step, err)); err != nil {
			return err
		}
		ready, err = Clock()
		if err != nil {
			return err
		}
		if h.Periodic != nil {
			// The thinking clock advances throughout. Only the trace timer stops during suspension.
			h.clock.next += start - p.suspended
			if err := h.unknownTimer("restore", step, start, ready, h.clock.wallDeadline(h.clock.next)); err != nil {
				return err
			}
			// A delayed stock pause/resume cannot establish that the timer really
			// expired. The mandatory pre-tool trace now starts a fresh period.
			h.clock.next = 0
		}
	}
	return h.boundary(ctx, "pre-tool", step, ready)
}

func (h Hooks) finishSuspended(abort bool) error {
	p := h.lifecycle.pending
	if abort {
		p.cancel()
	}
	<-p.done
	if p.err != nil {
		return fmt.Errorf("finishing checkpoint: %w", p.err)
	}
	if abort {
		return nil
	}
	return emit(h, Receipt{Event: "session_end_suspended", CaptureIndex: len(h.clock.windows)})
}
