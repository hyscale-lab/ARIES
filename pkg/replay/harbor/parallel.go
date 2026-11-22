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
				if x.Kind == "function_call_output" || seen[x.Sequence] {
					return errors.New("batch requires a reply before all dispatches")
				}
				seen[x.Sequence] = true
				id := r.Plan.Calls[x.Sequence-1].SessionID
				if id != 0 && sessions[id] {
					return errors.New("concurrent operations on same source handle")
				}
				sessions[id] = true
			} else {
				if x.Kind != "function_call_output" || !seen[x.Sequence] {
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

func runParallel(ctx context.Context, h Hooks, r *Recording, events []Event, first Receipt) (runErr error) {
	n := len(events) / 2
	if h.IntraTool != nil && h.IntraTool.Step == first.Step {
		return errors.New("intra-tool sampling requires an individual, foreground source call")
	}
	if err := h.Boundary(ctx, "pre-tool", first.Step); err != nil {
		return err
	}
	work, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
		runErr = errors.Join(runErr, h.Boundary(ctx, "post-tool", first.Step))
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
	start := time.Now()
	next, finished := 0, 0
	dispatch := func(e Event, receipt Receipt) error {
		call := r.Plan.Calls[e.Sequence-1]
		receipt = toolReceipt(h, receipt, call)
		receipt.Event = "tool_dispatch"
		if err := emit(h, receipt); err != nil {
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
	for finished < n {
		var timer *time.Timer
		var dispatchTime <-chan time.Time
		if next < n {
			delay := events[next].SourceTime.Sub(events[0].SourceTime) - time.Since(start)
			timer = time.NewTimer(max(0, delay))
			dispatchTime = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return ctx.Err()
		case <-dispatchTime:
			e := events[next]
			receipt := first
			if next > 0 {
				receipt = Receipt{Event: "source_event", SourceKind: e.Kind, SourceCallID: e.CallID, Step: e.Step, Line: e.Line, Sequence: e.Sequence, ParallelSize: n, Basis: e.Basis, RequestedNS: int64(e.Wait)}
				if err := emit(h, receipt); err != nil {
					return err
				}
			}
			if err := dispatch(e, receipt); err != nil {
				return err
			}
			next++
		case outcome := <-done:
			if timer != nil {
				timer.Stop()
			}
			e := outcome.request
			call := r.Plan.Calls[e.Sequence-1]
			receipt := outcome.receipt
			if err := recordReply(ctx, h, call, receipt, outcome.output, outcome.mono, outcome.err); err != nil {
				return err
			}
			reply := sourceReplies[e.Sequence]
			if _, err := recordSourceEvent(ctx, h, reply); err != nil {
				return err
			}
			finished++
		}
	}
	// A late terminal poll in this diagnostic can add a join. Preserve the
	// recorded dispatch schedule of every original sibling before joining.
	for _, join := range processJoins {
		if err := join(); err != nil {
			return err
		}
	}
	return nil
}
