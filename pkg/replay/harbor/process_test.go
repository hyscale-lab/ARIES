package harbor

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/replay"
)

func TestProcessResumesAndReturnsEachStreamOnce(t *testing.T) {
	continueWork := make(chan struct{})
	wrote := make(chan struct{})
	sessions := NewProcessSessions(t.Context(), func(ctx context.Context, _ replay.Call, out, errOut io.Writer) (core.CommandResult, error) {
		fmt.Fprint(out, "first")
		close(wrote)
		select {
		case <-ctx.Done():
			return core.CommandResult{}, ctx.Err()
		case <-continueWork:
		}
		fmt.Fprint(out, "last")
		fmt.Fprint(errOut, "diagnostic")
		return core.CommandResult{ExitCode: 3}, nil
	})
	defer sessions.Close()
	result := make(chan core.CommandResult, 1)
	go func() {
		first, err := sessions.Execute(t.Context(), replay.Call{Operation: "StartProcess", SessionID: 7, YieldTime: 40 * time.Millisecond})
		if err != nil {
			t.Errorf("start: %v", err)
		}
		result <- first
	}()
	<-wrote
	first := <-result
	if first.ExitCode != ProcessRunning || first.Stdout != "first" {
		t.Fatal(first)
	}
	if states := sessions.EndingProcessStates([]int{7, 8}); states[7] != "running" || states[8] != "missing" {
		t.Fatalf("unexpected end-of-session observation: %v", states)
	}
	close(continueWork)
	poll := replay.Call{Operation: "PollProcess", SessionID: 7, YieldTime: time.Second}
	last, err := sessions.Execute(t.Context(), poll)
	if err != nil || last.ExitCode != 3 || last.Stdout != "last" || last.Stderr != "diagnostic" {
		t.Fatal(last, err)
	}
	if _, err := sessions.Execute(t.Context(), poll); err == nil {
		t.Fatal("consumed session still active")
	}
}

func TestDiagnosticPollRetainsEarlyCompletionAndConsumesOutputOnce(t *testing.T) {
	sessions := NewProcessSessions(t.Context(), func(_ context.Context, _ replay.Call, stdout, _ io.Writer) (core.CommandResult, error) {
		fmt.Fprint(stdout, "finished")
		return core.CommandResult{ExitCode: 0}, nil
	})
	defer sessions.Close()
	call := replay.Call{Operation: "StartProcess", SessionID: 12, YieldTime: time.Second, ExitCode: ProcessRunning}
	early, err := sessions.ObserveDrift(t.Context(), call)
	if err != nil || early.ExitCode != 0 || early.Stdout != "finished" || !sessions.WasRetained(12) {
		t.Fatal(early, err)
	}
	if state := sessions.EndingProcessStates([]int{12})[12]; state != "terminal_retained" {
		t.Fatalf("early terminal process was not retained: %s", state)
	}
	call.Operation = "PollProcess"
	again, err := sessions.ObserveDrift(t.Context(), call)
	if err != nil || again.ExitCode != 0 || again.Stdout != "" {
		t.Fatal(again, err)
	}
	call.ExitCode = 0
	last, err := sessions.ObserveDrift(t.Context(), call)
	if err != nil || last.ExitCode != 0 || sessions.WasRetained(12) {
		t.Fatal(last, err)
	}
}

func TestRelaxedUnnamedCommandKeepsOriginalProviderIdentity(t *testing.T) {
	called := make(chan replay.Call, 1)
	finish := make(chan struct{})
	sessions := NewProcessSessions(t.Context(), func(ctx context.Context, call replay.Call, stdout, _ io.Writer) (core.CommandResult, error) {
		called <- call
		select {
		case <-finish:
		case <-ctx.Done():
			return core.CommandResult{}, ctx.Err()
		}
		fmt.Fprint(stdout, "real late output")
		return core.CommandResult{ExitCode: 9}, nil
	})
	defer sessions.Close()
	command := replay.Call{Operation: "Exec", Sequence: 4, YieldTime: time.Millisecond}
	first, err := sessions.ObserveRelaxed(t.Context(), command)
	if err != nil || first.ExitCode != ProcessRunning {
		t.Fatal(first, err)
	}
	call := <-called
	if call.Operation != "Exec" || call.SessionID != 0 {
		t.Fatalf("provider received altered identity: %s, %d", call.Operation, call.SessionID)
	}
	close(finish)
	last, err := sessions.FinishRelaxed(t.Context(), command)
	if err != nil || last.ExitCode != 9 || last.Stdout != "real late output" {
		t.Fatal(last, err)
	}
}

func TestDiagnosticTerminalJoinReadsActualRemainingOutput(t *testing.T) {
	release := make(chan struct{})
	sessions := NewProcessSessions(t.Context(), func(ctx context.Context, _ replay.Call, stdout, _ io.Writer) (core.CommandResult, error) {
		select {
		case <-ctx.Done():
			return core.CommandResult{}, ctx.Err()
		case <-release:
		}
		fmt.Fprint(stdout, "last")
		return core.CommandResult{ExitCode: 4}, nil
	})
	defer sessions.Close()
	call := replay.Call{Operation: "StartProcess", SessionID: 13, ExitCode: ProcessRunning, YieldTime: time.Millisecond}
	if out, e := sessions.ObserveDrift(t.Context(), call); e != nil || out.ExitCode != ProcessRunning {
		t.Fatal(out, e)
	}
	call.Operation = "PollProcess"
	call.ExitCode = 4
	if out, e := sessions.ObserveDrift(t.Context(), call); e != nil || out.ExitCode != ProcessRunning {
		t.Fatal(out, e)
	}
	close(release)
	last, e := sessions.FinishObservedProcess(t.Context(), call)
	if e != nil || last.ExitCode != 4 || last.Stdout != "last" {
		t.Fatal(last, e)
	}
	if _, e := sessions.ObserveDrift(t.Context(), call); e == nil {
		t.Fatal("consumed handle still exists")
	}
}

func TestReplaySeparatesLateTerminalPoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	release := make(chan struct{})
	sessions := NewProcessSessions(ctx, func(ctx context.Context, _ replay.Call, stdout, _ io.Writer) (core.CommandResult, error) {
		select {
		case <-ctx.Done():
			return core.CommandResult{}, ctx.Err()
		case <-release:
		}
		fmt.Fprint(stdout, "terminal output")
		return core.CommandResult{ExitCode: 4}, nil
	})
	defer sessions.Close()
	calls := []replay.Call{
		{Sequence: 1, Operation: "StartProcess", SessionID: 13, ExitCode: ProcessRunning, YieldTime: 10 * time.Millisecond},
		{Sequence: 2, Operation: "PollProcess", SessionID: 13, ExitCode: ProcessRunning, YieldTime: 10 * time.Millisecond},
		{Sequence: 3, Operation: "PollProcess", SessionID: 13, ExitCode: 4, YieldTime: 40 * time.Millisecond},
	}
	r := &Recording{Plan: replay.Plan{Calls: calls}, Events: []Event{
		{Kind: "exec_command", Sequence: 1, CallID: "start"},
		{Kind: "write_stdin", Sequence: 2, CallID: "running"},
		{Kind: "write_stdin", Sequence: 3, CallID: "terminal"},
	}}
	var replies []Receipt
	err := Run(ctx, r, Hooks{
		Exec:                  sessions.ObserveDrift,
		FinishObservedProcess: sessions.PollObservedProcess,
		Boundary:              func(context.Context, string, int) error { return nil },
		Record: func(receipt Receipt) error {
			if receipt.Event == "diagnostic_process_dispatch" {
				close(release)
			}
			if receipt.Event == "tool_reply" || receipt.Event == "diagnostic_process_dispatch" || receipt.Event == "diagnostic_process_observation" {
				replies = append(replies, receipt)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 6 {
		t.Fatalf("got %d replies or diagnostics", len(replies))
	}
	for i, receipt := range replies[:3] {
		if receipt.Event != "tool_reply" || receipt.Exit == nil || *receipt.Exit != ProcessRunning || receipt.SessionID != 13 || receipt.Sequence != uint64(i+1) {
			t.Fatalf("first reply %d: %+v", i+1, receipt)
		}
	}
	first, dispatch, last := replies[2], replies[4], replies[5]
	if dispatch.Event != "diagnostic_process_dispatch" || dispatch.Time < first.Time ||
		last.Event != "diagnostic_process_observation" || last.Time < dispatch.Time || last.SourceCallID != "terminal" ||
		last.SessionID != 13 || last.Exit == nil || *last.Exit != 4 || last.Stdout != "terminal output" {
		t.Fatalf("diagnostic did not preserve the original handle and reply: %+v %+v %+v", first, dispatch, last)
	}
}

func TestCodexYieldBeginsAfterLaunchAndEarlyExitGrace(t *testing.T) {
	allowStart := make(chan struct{})
	started := make(chan struct{})
	finish := make(chan struct{})
	sessions := NewCodexProcessSessions(t.Context(), func(ctx context.Context, _ replay.Call, _, _ io.Writer) (core.CommandResult, error) {
		<-allowStart
		core.MarkCommandTiming(ctx, "command.started")
		close(started)
		<-finish
		return core.CommandResult{ExitCode: 3}, nil
	})
	defer sessions.Close()
	reply := make(chan core.CommandResult, 1)
	go func() {
		r, _ := sessions.Execute(t.Context(), replay.Call{Operation: "StartProcess", SessionID: 7, YieldTime: 10 * time.Millisecond})
		reply <- r
	}()
	select {
	case <-reply:
		t.Fatal("yield returned before start")
	case <-time.After(30 * time.Millisecond):
	}
	close(allowStart)
	<-started
	select {
	case <-reply:
		t.Fatal("yield returned inside the native early-exit window")
	case <-time.After(45 * time.Millisecond):
	}
	close(finish)
	if got := <-reply; got.ExitCode != 3 {
		t.Fatalf("early exit lost: %+v", got)
	}
}

func TestCodexLaunchFailureDoesNotWaitForStart(t *testing.T) {
	sessions := NewCodexProcessSessions(t.Context(), func(context.Context, replay.Call, io.Writer, io.Writer) (core.CommandResult, error) {
		return core.CommandResult{}, io.ErrUnexpectedEOF
	})
	defer sessions.Close()
	_, err := sessions.Execute(t.Context(), replay.Call{Operation: "StartProcess", SessionID: 8, YieldTime: time.Hour})
	if err != io.ErrUnexpectedEOF {
		t.Fatalf("launch failure = %v", err)
	}
}

func TestExpectedTerminalShellCanGenuinelyYield(t *testing.T) {
	finished := make(chan struct{})
	group := NewCodexProcessSessions(t.Context(), func(ctx context.Context, _ replay.Call, _, _ io.Writer) (core.CommandResult, error) {
		core.MarkCommandTiming(ctx, "command.started")
		<-finished
		return core.CommandResult{ExitCode: 0}, nil
	})
	defer group.Close()
	defer close(finished)
	before := time.Now()
	got, err := group.Execute(t.Context(), replay.Call{Operation: "Exec", ExitCode: 0, YieldTime: 10 * time.Millisecond})
	if err != nil || got.ExitCode != ProcessRunning || time.Since(before) < codexEarlyExitGrace+10*time.Millisecond {
		t.Fatalf("source-terminal live command = %+v elapsed=%s err=%v", got, time.Since(before), err)
	}
}

func TestNativeClockRejectsTransportWithoutAcknowledgement(t *testing.T) {
	s := NewCodexProcessSessions(t.Context(), func(context.Context, replay.Call, io.Writer, io.Writer) (core.CommandResult, error) {
		return core.CommandResult{}, nil
	})
	defer s.Close()
	if _, err := s.Execute(t.Context(), replay.Call{Operation: "Exec", YieldTime: time.Millisecond}); err == nil {
		t.Fatal("accepted a completed native command without an acknowledged start")
	}
}

func TestPipeInterruptDoesNotResendDuringDiagnosticPoll(t *testing.T) {
	finish := make(chan struct{})
	sessions := NewProcessSessions(t.Context(), func(ctx context.Context, _ replay.Call, _, _ io.Writer) (core.CommandResult, error) {
		select {
		case <-ctx.Done():
			return core.CommandResult{}, ctx.Err()
		case <-finish:
			return core.CommandResult{ExitCode: 130}, nil
		}
	})
	defer sessions.Close()
	interrupts := 0
	sessions.SendInterrupt = func(_ context.Context, call replay.Call) error {
		if call.SessionID != 37 {
			t.Fatalf("interrupt targeted %d", call.SessionID)
		}
		interrupts++
		return nil
	}
	start := replay.Call{Operation: "StartProcess", SessionID: 37, ExitCode: ProcessRunning, YieldTime: time.Millisecond}
	if got, err := sessions.Execute(t.Context(), start); err != nil || got.ExitCode != ProcessRunning {
		t.Fatalf("start = %+v, %v", got, err)
	}
	poll := replay.Call{Operation: "PollProcess", SessionID: 37, PipeInterrupt: true, ExitCode: 130, YieldTime: time.Millisecond}
	if got, err := sessions.ObserveRelaxed(t.Context(), poll); err != nil || got.ExitCode != ProcessRunning {
		t.Fatalf("interrupt poll = %+v, %v", got, err)
	}
	close(finish)
	poll.YieldTime = time.Second
	if got, err := sessions.PollObservedProcess(t.Context(), poll); err != nil || got.ExitCode != 130 || interrupts != 1 {
		t.Fatalf("diagnostic poll = %+v, %v, sent %d interrupts", got, err, interrupts)
	}
	if _, err := sessions.Execute(t.Context(), poll); err == nil || interrupts != 1 {
		t.Fatalf("closed handle returned %v after %d interrupts", err, interrupts)
	}
}

func TestBoundedDiagnosticStillRunning(t *testing.T) {
	call := replay.Call{Operation: "PollProcess", SessionID: 73, ExitCode: 0}
	var events []Receipt
	hooks := Hooks{
		KeepBoundedPollResult: true,
		Record: func(event Receipt) error {
			events = append(events, event)
			return nil
		},
		FinishObservedProcess: func(ctx context.Context, actual replay.Call) (core.CommandResult, error) {
			if actual.SessionID != call.SessionID {
				t.Fatalf("diagnostic used handle %d", actual.SessionID)
			}
			return core.CommandResult{ExitCode: ProcessRunning}, nil
		},
	}
	err := recordProcessDrift(t.Context(), hooks, call, Receipt{}, core.CommandResult{ExitCode: ProcessRunning})
	if err != nil || len(events) != 3 {
		t.Fatalf("observations %v, error %v", events, err)
	}
	if events[0].StatusBasis != "real_process_running_at_source_terminal_poll" || events[1].Event != "diagnostic_process_dispatch" || events[2].StatusBasis != "additional_join_after_source_poll" || *events[0].Exit != ProcessRunning || *events[2].Exit != ProcessRunning {
		t.Fatalf("first reply and diagnostic must retain their running observations: %v", events)
	}
}

func TestSourceTerminalExecYieldUsesSeparateDiagnosticIdentity(t *testing.T) {
	for _, finish := range []bool{false, true} {
		t.Run(fmt.Sprint(finish), func(t *testing.T) {
			terminal := make(chan struct{})
			sessions := NewProcessSessions(t.Context(), func(ctx context.Context, call replay.Call, stdout, stderr io.Writer) (core.CommandResult, error) {
				if call.Sequence == 1 {
					select {
					case <-terminal:
						_, _ = fmt.Fprint(stdout, "late output")
					case <-ctx.Done():
						return core.CommandResult{}, ctx.Err()
					}
				}
				return core.CommandResult{}, nil
			})
			defer sessions.Close()
			recording := &Recording{Plan: replay.Plan{Calls: []replay.Call{
				{Operation: "Exec", Sequence: 1, YieldTime: 10 * time.Millisecond},
				{Operation: "Exec", Sequence: 2, YieldTime: time.Second},
			}}, Events: []Event{
				{Kind: "exec_command", CallID: "late", Step: 1, Sequence: 1},
				{Kind: "function_call_output", CallID: "late", Step: 1, Sequence: 1},
				{Kind: "exec_command", CallID: "next", Step: 2, Sequence: 2},
				{Kind: "function_call_output", CallID: "next", Step: 2, Sequence: 2},
			}}
			var first, result Receipt
			var next bool
			h := Hooks{
				KeepBoundedPollResult: true,
				Exec:                  sessions.ObserveRelaxed,
				Boundary:              func(context.Context, string, int) error { return nil },
				FinishObservedProcess: sessions.PollObservedProcess,
				Record: func(r Receipt) error {
					if r.Sequence == 1 {
						switch r.Event {
						case "tool_reply":
							first = r
						case "diagnostic_process_dispatch":
							if r.DiagnosticID != -1 || r.SessionID != 0 {
								t.Fatalf("wrong follow-up identity: %+v", r)
							}
							if finish {
								close(terminal)
							}
						case "diagnostic_process_observation":
							if r.StatusBasis == "additional_join_after_source_reply" {
								result = r
							}
						}
					}
					if r.Event == "tool_reply" && r.Sequence == 2 {
						next = true
					}
					return nil
				},
			}
			if err := Run(t.Context(), recording, h); err != nil {
				t.Fatal(err)
			}
			want := ProcessRunning
			if finish {
				want = 0
			}
			if first.Exit == nil || *first.Exit != ProcessRunning || first.SessionID != 0 ||
				first.Expected == nil || *first.Expected != 0 || first.DiagnosticID != 0 {
				t.Fatalf("first reply changed: %+v", first)
			}
			if result.Exit == nil || *result.Exit != want || result.DiagnosticID != -1 || result.SessionID != 0 {
				t.Fatalf("wrong bounded observation: %+v", result)
			}
			if finish && result.Stdout != "late output" {
				t.Fatalf("bounded observation lost pending output: %+v", result)
			}
			if !next || len(sessions.EndingProcessStates(nil)) != 0 {
				t.Fatal("later source call or source handle state changed")
			}
		})
	}
}

func TestSourceOnlyPollRetainsFirstReplyWithoutAdditionalDispatch(t *testing.T) {
	var events []Receipt
	hooks := Hooks{KeepBoundedPollResult: true, Record: func(e Receipt) error {
		events = append(events, e)
		return nil
	}}
	call := replay.Call{Operation: "PollProcess", SessionID: 73, ExitCode: 0}
	first := core.CommandResult{ExitCode: ProcessRunning}
	if err := recordReply(t.Context(), hooks, call, Receipt{SourceCallID: "poll"}, first, 1, nil); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Event != "tool_reply" || *events[0].Exit != ProcessRunning ||
		events[1].StatusBasis != "real_process_running_at_source_terminal_poll" {
		t.Fatalf("unexpected source-only receipts: %+v", events)
	}
}
