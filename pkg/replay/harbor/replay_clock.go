package harbor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const tracePeriod = 10 * time.Second

// Freeze uses CLOCK_MONOTONIC nanoseconds, from pause acknowledgement to resume request.
type Freeze struct {
	PauseAckNS      int64
	ResumeRequestNS int64
}

type traceWindow struct {
	begin, end int64
	freeze     Freeze
}

type replayClock struct {
	origin, cursor, next int64
	interval             time.Duration
	lastTraceEnd         int64
	lastDispatch         int64
	step                 int
	windows              []traceWindow
}

func newReplayClock(begin int64) *replayClock {
	return &replayClock{origin: begin, interval: tracePeriod}
}

func (c *replayClock) runnable(wall int64) int64 {
	elapsed := wall - c.origin
	for _, window := range c.windows {
		freeze := window.freeze
		elapsed -= max(0, min(wall, freeze.ResumeRequestNS)-freeze.PauseAckNS)
	}
	return elapsed
}

func (c *replayClock) wallDeadline(runnable int64) int64 {
	wall := c.origin + runnable
	for _, window := range c.windows {
		freeze := window.freeze
		if freeze.PauseAckNS <= wall {
			wall += freeze.ResumeRequestNS - freeze.PauseAckNS
		}
	}
	return wall
}

func (h Hooks) boundary(ctx context.Context, phase string, step int, transitionTime int64) error {
	if h.Periodic == nil {
		return h.Boundary(ctx, phase, step)
	}
	return h.capture(ctx, phase, step, transitionTime, false)
}

func (h Hooks) capture(ctx context.Context, phase string, step int, transitionTime int64, periodic bool) error {
	begin, err := Clock()
	if err != nil {
		return err
	}
	c := h.clock
	var triggers []string
	if !periodic {
		triggers = append(triggers, phase)
	}
	if periodic || (c.next > 0 && c.runnable(transitionTime) >= c.next) {
		triggers = append(triggers, "timer")
	}
	c.next = c.runnable(begin) + int64(c.interval)
	freeze, captureErr := h.Periodic(ctx, phase, step, triggers)
	end, clockErr := Clock()
	if freeze != (Freeze{}) && (freeze.PauseAckNS < begin || freeze.ResumeRequestNS < freeze.PauseAckNS || freeze.ResumeRequestNS > end) {
		return errors.Join(captureErr, clockErr, fmt.Errorf("invalid freeze for %s step %d: %v outside [%d, %d]", phase, step, freeze, begin, end))
	}
	c.windows = append(c.windows, traceWindow{begin, end, freeze})
	recordErr := h.Record(Receipt{
		Event: "trace_capture", Time: begin, Step: step,
		Phase: phase, Triggers: triggers, CaptureIndex: len(c.windows), CaptureEndNS: end,
		PauseAckNS: freeze.PauseAckNS, ResumeRequestNS: freeze.ResumeRequestNS,
	})
	return errors.Join(captureErr, clockErr, recordErr)
}

func (h Hooks) markTraceBlocked(ready, actual int64, phase string, step int) error {
	c := h.clock
	wallReady := c.wallDeadline(ready)
	for _, capture := range c.windows {
		begin := max(capture.begin, wallReady, c.lastTraceEnd)
		end := min(capture.end, actual)
		// Freeze time has its own receipt. Report only additional non-runnable-schedule delay here.
		for _, span := range [][2]int64{
			{begin, min(end, capture.freeze.PauseAckNS)},
			{max(begin, capture.freeze.ResumeRequestNS), end},
		} {
			if span[0] < span[1] {
				if err := h.Record(Receipt{Event: "trace_blocked", Time: span[0], Step: step, Phase: phase, CaptureEndNS: span[1]}); err != nil {
					return err
				}
			}
		}
	}
	c.lastTraceEnd = actual
	return nil
}

func (h Hooks) waitRunnable(ctx context.Context, step int, deadline int64) (int64, error) {
	c := h.clock
	var completed <-chan struct{}
	if h.lifecycle != nil && h.lifecycle.pending != nil {
		completed = h.lifecycle.pending.done
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		wall, err := Clock()
		if err != nil {
			return 0, err
		}
		now := c.runnable(wall)
		if now >= deadline {
			return wall, nil
		}
		next := deadline
		// Native checkpoint and restore own the guest. Never freeze it concurrently.
		if h.lifecycle == nil || h.lifecycle.pending == nil {
			next = min(next, c.next)
		}
		if now >= next {
			if err := h.capture(ctx, "wait", step, wall, true); err != nil {
				return 0, err
			}
			continue
		}
		timer := time.NewTimer(time.Duration(next - now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-completed:
			timer.Stop()
			if err := h.lifecycle.pending.err; err != nil {
				return 0, err
			}
			completed = nil
		case <-timer.C:
		}
	}
}
