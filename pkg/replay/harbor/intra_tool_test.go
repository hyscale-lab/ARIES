package harbor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestIntraToolKeepsEarlyLiveReply(t *testing.T) {
	h := Hooks{
		IntraTool: &IntraToolOptions{Step: 12, Interval: time.Hour, Windows: 3},
		Exec: func(context.Context, replay.Call) (core.CommandResult, error) {
			return core.CommandResult{Stdout: "done"}, nil
		},
		Boundary: func(context.Context, string, int) error {
			return fmt.Errorf("unexpected capture after a live reply")
		},
	}
	result, replied, err := h.executeTool(context.Background(), replay.Call{}, Receipt{Step: 12})
	if err != nil || result.Stdout != "done" || replied <= 0 {
		t.Fatalf("unexpected result %v reply %d error %v", result, replied, err)
	}
}

func TestIntraToolRearmsOneActiveCallWithinBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	allowReply := make(chan struct{})
	var captures []Receipt
	h := Hooks{
		IntraTool: &IntraToolOptions{Step: 12, Interval: time.Millisecond, Windows: 2},
		Exec: func(ctx context.Context, _ replay.Call) (core.CommandResult, error) {
			select {
			case <-allowReply:
				return core.CommandResult{Stdout: "done"}, nil
			case <-ctx.Done():
				return core.CommandResult{}, ctx.Err()
			}
		},
		Boundary: func(_ context.Context, phase string, step int) error {
			if phase != "intra-tool" || step != 12 {
				return fmt.Errorf("unexpected diagnostic boundary %s/%d", phase, step)
			}
			return nil
		},
		Record: func(event Receipt) error {
			captures = append(captures, event)
			if len(captures) == 2 {
				close(allowReply)
			}
			return nil
		},
	}
	result, replied, err := h.executeTool(ctx, replay.Call{}, Receipt{Step: 12})
	if err != nil || result.Stdout != "done" || len(captures) != 2 {
		t.Fatalf("unexpected result %v captures %v error %v", result, captures, err)
	}
	for i, capture := range captures {
		if capture.Event != "intra_tool_capture" || capture.CaptureIndex != i+1 || capture.Time > capture.CaptureEndNS || capture.CaptureEndNS > replied {
			t.Fatalf("unexpected capture timing %#v at reply %d", capture, replied)
		}
	}
}
