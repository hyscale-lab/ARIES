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
