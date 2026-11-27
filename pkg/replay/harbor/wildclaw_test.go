package harbor

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestWildClawCalendarRejectsUnreviewedSource(t *testing.T) {
	if _, err := LoadWildClawCalendar([]byte("{}\n")); err == nil {
		t.Fatal("accepted unreviewed OpenClaw bytes")
	}
}

// The imported trace paces for about 103 seconds. Supply the publisher's file
// explicitly so routine checks need neither a download nor that wall time.
func TestWildClawCalendarReplay(t *testing.T) {
	path := os.Getenv("WILDCLAW_CALENDAR_TRACE")
	if path == "" {
		t.Skip("set WILDCLAW_CALENDAR_TRACE to the pinned publisher JSONL")
	}
	trace, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := LoadWildClawCalendar(trace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWildClawCalendar(append(trace, '\n')); err == nil {
		t.Fatal("accepted a changed trace")
	}
	wantIDs := []string{"call_dePeyokUlznMXFdZgUsFKjrF", "call_yGkhfg3USIunm1umrKT5nmtH", "call_wfJ5ClD9CX6924JLOtF7cVqJ", "call_WgGja4BfmbEmdZBArDZ3XLaw", "call_k2Z3tsRAluekbrpOJFH4b99S"}
	wantSourceOffsets := []time.Duration{6068 * time.Millisecond, 6068 * time.Millisecond, 6068 * time.Millisecond, 95385 * time.Millisecond, 98500 * time.Millisecond}
	wantExit := []int32{0, 0, 0, 128, 0}
	for i, call := range r.Plan.Calls {
		if call.ToolCallID != wantIDs[i] || call.StartOffset != wantSourceOffsets[i] || call.ExitCode != wantExit[i] {
			t.Fatalf("unexpected call %d identity, timing or status", i)
		}
	}
	read := r.Plan.Calls[0]
	if read.Operation != "ReadFile" || read.Path != "/root/USER.md" || read.Script != "" || r.Plan.Calls[1].Workdir != "/root" {
		t.Fatal("unexpected read or home shell projection")
	}
	if r.Plan.Calls[3].Timeout != time.Minute || r.Plan.Calls[3].YieldTime != 10*time.Second || r.OpenClawTrace.End.Sub(r.Plan.StartedAt) != 102981*time.Millisecond || r.RolloutHash != "" || r.TrajectoryHash != "" {
		t.Fatal("unexpected source metadata or exec arguments")
	}
	if r.Plan.FinalWait != 4360*time.Millisecond {
		t.Fatal(r.Plan.FinalWait)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var receipts []Receipt
	h := Hooks{
		Periodic: func(context.Context, string, int, []string) (Freeze, error) { return Freeze{}, nil },
		Record:   func(r Receipt) error { receipts = append(receipts, r); return nil },
		Exec: func(_ context.Context, call replay.Call) (core.CommandResult, error) {
			out := core.CommandResult{Stdout: string(call.ResponseStdout), ExitCode: int(call.ExitCode)}
			if call.Sequence == 3 {
				out.Stdout = "test backend live reply"
			}
			return out, nil
		},
	}
	if err := Run(ctx, r, h); err != nil {
		t.Fatal(err)
	}
	var waits []int64
	var dispatches, replies [5]int64
	var replyCount, timerCount int
	for _, receipt := range receipts {
		switch receipt.Event {
		case "wait_end":
			waits = append(waits, receipt.RequestedNS)
		case "tool_dispatch":
			dispatches[receipt.Sequence-1] = receipt.Time
			if receipt.SourceCallID != wantIDs[receipt.Sequence-1] || receipt.SourceRecordID == "" {
				t.Fatal(receipt)
			}
		case "tool_reply":
			replies[receipt.Sequence-1] = receipt.Time
			if receipt.Sequence == 1 && receipt.Expected != nil || receipt.Sequence == 3 && (receipt.Stdout != "test backend live reply" || receipt.OutputComparison != "differed") {
				t.Fatal(receipt)
			}
		case "source_event":
			if receipt.SourceKind == "toolResult" {
				replyCount++
				if receipt.Basis != "replaced_by_live_guest_reply" || receipt.RequestedNS != 0 || receipt.SourceRecordID == "" {
					t.Fatal(receipt)
				}
			}
		case "trace_capture":
			if reflect.DeepEqual(receipt.Triggers, []string{"timer"}) {
				timerCount++
			}
		}
	}
	wantWaits := []int64{6068_000_000, 89202_000_000, 2873_000_000, 4360_000_000}
	if !reflect.DeepEqual(waits, wantWaits) || replyCount != 5 || timerCount < 8 || receipts[len(receipts)-1].Event != "session_end" {
		t.Fatalf("waits %v, replies %d, timer captures %d", waits, replyCount, timerCount)
	}
	for i := 1; i < 3; i++ {
		if replies[i-1] == 0 || dispatches[i] < replies[i-1] {
			t.Fatalf("overlapping siblings: dispatch %v, first reply %v", dispatches, replies)
		}
	}
	gaps := []int64{dispatches[0] - receipts[0].Time, dispatches[3] - replies[2], dispatches[4] - replies[3], receipts[len(receipts)-1].Time - replies[4]}
	for i, gap := range gaps {
		if gap < wantWaits[i] {
			t.Fatalf("gap %d was %s, expected at least %s", i, time.Duration(gap), time.Duration(wantWaits[i]))
		}
	}
	t.Logf("test Hooks: source gaps %v ns, live gaps %v ns, %d timer captures, %d bound replies", waits, gaps, timerCount, replyCount)
}
