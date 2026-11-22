package harbor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

// IntraToolOptions schedules a finite number of additional diagnostic captures
// inside one actual call. The interval elapses anew after each capture finishes.
type IntraToolOptions struct {
	Step     int
	Interval time.Duration
	Windows  int
}

func (o *IntraToolOptions) Validate(r *Recording) error {
	if o == nil {
		return nil
	}
	if o.Step < 1 || o.Interval <= 0 || o.Windows < 1 {
		return errors.New("intra-tool diagnostics require a step, positive interval, and finite window count")
	}
	for _, event := range r.Events {
		if event.Step == o.Step && event.Kind == "exec_command" {
			return nil
		}
	}
	return fmt.Errorf("intra-tool step %d is not a recorded exec command", o.Step)
}

type toolCompletion struct {
	output core.CommandResult
	time   int64
	err    error
}

func (h Hooks) executeTool(ctx context.Context, call replay.Call, receipt Receipt) (core.CommandResult, int64, error) {
	if h.IntraTool == nil || receipt.Step != h.IntraTool.Step {
		output, err := h.Exec(ctx, call)
		finished, clockErr := Clock()
		return output, finished, errors.Join(err, clockErr)
	}

	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan toolCompletion, 1)
	go func() {
		output, err := h.Exec(callCtx, call)
		finished, clockErr := Clock()
		completed <- toolCompletion{output, finished, errors.Join(err, clockErr)}
	}()

	timer := time.NewTimer(h.IntraTool.Interval)
	defer timer.Stop()
	var diagnosticErr error
	for n := 0; n < h.IntraTool.Windows; n++ {
		select {
		case result := <-completed:
			return result.output, result.time, result.err
		case <-timer.C:
		case <-ctx.Done():
		}
		// Take a queued live reply before starting a capture. A reply arriving
		// during a capture retains its timestamp from the execution goroutine.
		select {
		case result := <-completed:
			return result.output, result.time, result.err
		default:
		}
		if ctx.Err() != nil {
			break
		}
		diagnosticErr = h.captureIntraTool(ctx, receipt, n+1)
		if diagnosticErr != nil {
			cancel()
			break
		}
		if n+1 < h.IntraTool.Windows {
			timer.Reset(h.IntraTool.Interval)
		}
	}
	result := <-completed
	return result.output, result.time, errors.Join(result.err, diagnosticErr)
}

func (h Hooks) captureIntraTool(ctx context.Context, receipt Receipt, index int) error {
	begin, err := Clock()
	if err != nil {
		return err
	}
	captureErr := h.Boundary(ctx, "intra-tool", receipt.Step)
	end, clockErr := Clock()
	return errors.Join(captureErr, clockErr, h.Record(Receipt{
		Event: "intra_tool_capture", Time: begin, Step: receipt.Step,
		Line: receipt.Line, Sequence: receipt.Sequence,
		CaptureEndNS: end, CaptureIndex: index,
	}))
}
