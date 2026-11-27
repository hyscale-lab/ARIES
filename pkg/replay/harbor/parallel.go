package harbor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
)

func validateParallelGroups(r *Recording) error {
	for i := 0; i < len(r.Events); i++ {
		e := r.Events[i]
		n := e.ParallelSize
		if n < 2 {
			continue
		}
		if i+2*n > len(r.Events) {
			return errors.New("incomplete ATIF parallel batch")
		}
		seen := map[uint64]bool{}
		sessions := map[int]bool{}
		for j, x := range r.Events[i : i+2*n] {
			if x.Step != e.Step || x.ParallelSize != n || x.Sequence == 0 {
				return fmt.Errorf("interleaved batch at step %d", e.Step)
			}
			if j < n {
				if x.isToolReply() || seen[x.Sequence] {
					return errors.New("batch requires a reply before all dispatches")
				}
				seen[x.Sequence] = true
				id := r.Plan.Calls[x.Sequence-1].SessionID
				if r.OpenClawTrace == nil && id != 0 && sessions[id] {
					return errors.New("concurrent operations on same source handle")
				}
				sessions[id] = true
			} else {
				if !x.isToolReply() || !seen[x.Sequence] {
					return errors.New("wrong parallel reply binding")
				}
				delete(seen, x.Sequence)
			}
		}
		i += 2*n - 1
	}
	return nil
}

type parallelReply struct {
	request Event
	receipt Receipt
	output  core.CommandResult
	mono    int64
	err     error
}

func runToolGroup(ctx context.Context, h Hooks, r *Recording, events []Event, first Receipt) (runErr error) {
	n := len(events) / 2
	if h.IntraTool != nil && h.IntraTool.Step == first.Step {
		return errors.New("intra-tool sampling requires an individual, foreground source call")
	}
	requests := []Receipt{first}
	if h.RecordToolRequests {
		requests[0] = toolReceipt(h, first, r.Plan.Calls[events[0].Sequence-1])
		for _, e := range events[1:n] {
			requests = append(requests, toolReceipt(h, sourceReceipt(e), r.Plan.Calls[e.Sequence-1]))
		}
	}
	if err := h.prepareTools(ctx, requests); err != nil {
		return err
	}
	work, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	var lastReply int64
	var postCaptured bool
	capturePostTool := func() error {
		postCaptured = true
		if h.clock != nil && lastReply != 0 {
			h.clock.cursor = h.clock.runnable(lastReply)
			h.clock.step = first.Step
		}
		return h.boundary(ctx, "post-tool", first.Step, lastReply)
	}
	defer func() {
		cancel()
		wg.Wait()
		if !postCaptured {
			runErr = errors.Join(runErr, capturePostTool())
		}
	}()
	var processJoins []func() error
	if h.FinishObservedProcess != nil {
		h.deferProcessJoin = func(join func() error) { processJoins = append(processJoins, join) }
	}
	sourceReplies := map[uint64]Event{}
	for _, e := range events[n:] {
		sourceReplies[e.Sequence] = e
	}
	done := make(chan parallelReply, n)
	serial := r.OpenClawTrace != nil
	phase := "parallel-tool"
	if serial {
		phase = "tool"
	}
	start := time.Now()
	next, finished := 0, 0
	var batchStart int64
	dispatch := func(e Event, receipt Receipt) error {
		call := r.Plan.Calls[e.Sequence-1]
		receipt = toolReceipt(h, receipt, call)
		if err := h.dispatchReceipt(receipt, phase); err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := h.Exec(work, call)
			at, clockErr := Clock()
			done <- parallelReply{e, receipt, out, at, errors.Join(err, clockErr)}
		}()
		return nil
	}
	handleReply := func(outcome parallelReply) error {
		e := outcome.request
		call := r.Plan.Calls[e.Sequence-1]
		lastReply = max(lastReply, outcome.mono)
		if err := recordReply(ctx, h, call, outcome.receipt, outcome.output, outcome.mono, outcome.err); err != nil {
			return err
		}
		if _, err := recordSourceEvent(ctx, h, sourceReplies[e.Sequence]); err != nil {
			return err
		}
		finished++
		return nil
	}
	for finished < n {
		var timer *time.Timer
		var tick <-chan time.Time
		var periodic bool
		// OpenClaw awaits each native tool reply, even when it returns a running handle.
		canDispatch := next < n && (!serial || next == finished)
		cursor := batchStart
		if h.clock == nil {
			if canDispatch {
				delay := events[next].SourceTime.Sub(events[0].SourceTime) - time.Since(start)
				timer = time.NewTimer(max(0, delay))
				tick = timer.C
			}
		} else {
			due := h.clock.next
			periodic = true
			if canDispatch {
				cursor = batchStart + int64(events[next].SourceTime.Sub(events[0].SourceTime))
				if next == 0 {
					cursor = h.clock.cursor
				} else if serial {
					cursor = h.clock.runnable(lastReply)
				}
				if cursor <= due {
					due = cursor
					periodic = false
				}
			}
			at, err := Clock()
			if err != nil {
				return err
			}
			timer = time.NewTimer(max(0, time.Duration(due-h.clock.runnable(at))))
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-tick:
			if periodic {
				// Consume a ready reply first so the final batch reply can coalesce with the timer.
				select {
				case outcome := <-done:
					if err := handleReply(outcome); err != nil {
						return err
					}
					continue
				default:
				}
				at, err := Clock()
				if err != nil {
					return err
				}
				if err := h.capture(ctx, phase, first.Step, at, true); err != nil {
					return err
				}
				continue
			}
			e := events[next]
			receipt := first
			if next > 0 {
				receipt = sourceReceipt(e)
				if err := emit(h, receipt); err != nil {
					return err
				}
			}
			if h.clock != nil {
				h.clock.cursor = cursor
			}
			if err := dispatch(e, receipt); err != nil {
				return err
			}
			if h.clock != nil && next == 0 {
				batchStart = h.clock.lastDispatch
			}
			next++
		case outcome := <-done:
			if timer != nil {
				timer.Stop()
			}
			if err := handleReply(outcome); err != nil {
				return err
			}
		}
	}
	// Observe the transition at the last original reply before any diagnostic
	// follow-up. Every original sibling has returned before this capture.
	if err := capturePostTool(); err != nil {
		return err
	}
	for _, join := range processJoins {
		if err := join(); err != nil {
			return err
		}
	}
	return nil
}
