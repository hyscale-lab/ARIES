package harbor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestReplayReadOnlyPaymentsVerification(t *testing.T) {
	const interval = 8 * time.Millisecond
	recording := &Recording{
		Plan: replay.Plan{Calls: []replay.Call{{
			Operation: VerifyPatch, Sequence: 1, ToolCallID: "patch",
			ResponseStdout: []byte(duplicatePaymentsTarget),
		}}},
		Events: []Event{
			{Kind: "exec_command", Step: 15, CallID: "patch", Sequence: 1, VerificationInterval: interval},
			{Kind: "function_call_output", Step: 15, CallID: "patch", Sequence: 1, VerificationInterval: interval},
		},
	}
	for _, tc := range []struct {
		name, reply string
		wantError   bool
	}{
		{"duplicate matches", duplicatePaymentsTarget + "\n", false},
		{"missing file", "apply_patch verification failed: failed to read worker.py\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var begin, end int64
			calls := 0
			err := Run(context.Background(), recording, Hooks{
				Periodic: func(context.Context, string, int, []string) (Freeze, error) { return Freeze{}, nil },
				Exec: func(_ context.Context, call replay.Call) (core.CommandResult, error) {
					calls++
					if call.Operation != VerifyPatch {
						t.Fatalf("verification dispatched as %s", call.Operation)
					}
					return core.CommandResult{Stdout: tc.reply}, nil
				},
				Record: func(receipt Receipt) error {
					switch receipt.Event {
					case "tool_dispatch":
						begin = receipt.Time
					case "tool_reply":
						end = receipt.Time
						if receipt.SourceVerificationNS != int64(interval) {
							t.Fatalf("source interval lost: %+v", receipt)
						}
					}
					return nil
				},
			})
			if calls != 1 || time.Duration(end-begin) < interval {
				t.Fatalf("calls=%d reply interval=%v", calls, time.Duration(end-begin))
			}
			if tc.wantError {
				if err == nil || !strings.Contains(err.Error(), "differs from the recorded refusal") {
					t.Fatalf("wrong verifier failure: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
