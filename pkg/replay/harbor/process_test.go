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
