package deployment

import (
	"context"
	"errors"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

type processExecutorStub struct {
	result  core.CommandResult
	err     error
	command core.Command
	id      string
	calls   int
}

func (s *processExecutorStub) Exec(_ context.Context, id string, c core.Command) (core.CommandResult, error) {
	s.id = id
	s.command = c
	s.calls++
	return s.result, s.err
}
func TestProcessSnapshotRequiresInitAndUniqueStartTimeIdentities(t *testing.T) {
	for _, output := range []string{"", "2 9\n", "1 5\n1 6\n", "1 5\n2 shell-injection\n"} {
		s := &processExecutorStub{result: core.CommandResult{Stdout: output}}
		if _, err := SnapshotBridgeProcesses(context.Background(), s, "runtime"); err == nil {
			t.Fatalf("accepted invalid snapshot %q", output)
		}
	}
	s := &processExecutorStub{result: core.CommandResult{Stdout: "1 9\n42 123\n"}}
	baseline, err := SnapshotBridgeProcesses(context.Background(), s, "immutable")
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 2 || baseline[1].PID != 42 || s.command.User != "0:0" || s.id != "immutable" {
		t.Fatal("baseline or infrastructure execution identity changed")
	}
}
func TestProcessRevokeRetainsFailureAndBoundsStructuredInput(t *testing.T) {
	s := &processExecutorStub{result: core.CommandResult{ExitCode: 125}}
	baseline := []ProcessIdentity{{1, 9}, {42, 123}}
	if err := RevokeBridgeProcesses(context.Background(), s, "runtime", baseline); err == nil {
		t.Fatal("failed drain accepted")
	}
	if string(s.command.Stdin) != "1 9\n42 123\n" || s.command.User != "0:0" {
		t.Fatal("baseline not passed as private numeric stdin")
	}
	s.err = context.DeadlineExceeded
	if err := RevokeBridgeProcesses(context.Background(), s, "runtime", baseline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("lost cancellation uncertainty")
	}
	before := s.calls
	if err := RevokeBridgeProcesses(context.Background(), s, "runtime", []ProcessIdentity{{42, 1}}); err == nil || s.calls != before {
		t.Fatal("baseline without init reached backend")
	}
}
