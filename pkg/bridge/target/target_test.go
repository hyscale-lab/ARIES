package target

import (
	"context"
	"errors"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"io"
	"testing"
)

type backendStub struct {
	command         core.Command
	id              string
	validationErr   error
	calls           int
	snapshotErr     error
	revokeErr       error
	revokeCalls     int
	revokedBaseline []deployment.ProcessIdentity
}

func (b *backendStub) ValidateBridgeTarget(context.Context, core.BridgeTarget) error {
	return b.validationErr
}
func (b *backendStub) ExecStream(_ context.Context, id string, c core.Command, in io.Reader, out, stderr io.Writer) (core.CommandResult, error) {
	b.calls++
	b.id = id
	b.command = c
	if in != nil {
		_, _ = io.Copy(out, in)
	}
	return core.CommandResult{ExitCode: 7}, nil
}
func validDescriptor() core.BridgeTarget {
	return core.BridgeTarget{Version: 1, RunID: "run", TaskID: "task", OccurrenceID: "occurrence", Backend: "docker", RuntimeID: "immutable-id", RuntimeName: "runtime", Workdir: "/app", ExecUser: "1000:1000", MaxInputBytes: 16 << 20, MaxOutputBytes: 1 << 30, ExpectedLabels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run", "aries.task": "task"}}
}
func TestBorrowedPreservesDefaultsAndRejectsInvalidCommands(t *testing.T) {
	backend := &backendStub{}
	b, err := New(context.Background(), validDescriptor(), backend)
	if err != nil {
		t.Fatal(err)
	}
	result, err := b.ExecStream(context.Background(), core.Command{Path: "/bin/true", Args: []string{"a b", ""}}, nil, io.Discard, io.Discard)
	if err != nil || result.ExitCode != 7 || backend.id != "immutable-id" || backend.command.Dir != "/app" || backend.command.User != "1000:1000" || len(backend.command.Args) != 2 {
		t.Fatalf("lost defaults or argv: %+v %+v %v", result, backend.command, err)
	}
	_, err = b.ExecStream(context.Background(), core.Command{Path: "relative"}, nil, io.Discard, io.Discard)
	if err == nil || backend.calls != 1 {
		t.Fatalf("invalid command reached backend: %v", err)
	}
	_, err = b.ExecStream(context.Background(), core.Command{Path: "/bin/true", Dir: "/", User: "0:0"}, nil, io.Discard, io.Discard)
	if err != nil || backend.command.Dir != "/" || backend.command.User != "0:0" {
		t.Fatalf("lost overrides: %+v %v", backend.command, err)
	}
}
func TestBorrowedRejectsMismatchedIdentityAndOwnership(t *testing.T) {
	d := validDescriptor()
	d.ExpectedLabels["aries.task"] = "other"
	if _, err := New(context.Background(), d, &backendStub{}); err == nil {
		t.Fatal("cross-task descriptor accepted")
	}
	d = validDescriptor()
	if _, err := New(context.Background(), d, &backendStub{validationErr: errors.New("wrong immutable runtime")}); err == nil {
		t.Fatal("failed backend validation accepted")
	}
	d = validDescriptor()
	d.Backend = "unsupported"
	if err := Validate(d); err == nil {
		t.Fatal("unsupported backend accepted")
	}
}

func (b *backendStub) SnapshotBridgeProcesses(context.Context, string) ([]deployment.ProcessIdentity, error) {
	return []deployment.ProcessIdentity{{PID: 1, StartTime: 2}}, b.snapshotErr
}
func (b *backendStub) RevokeBridgeProcesses(_ context.Context, _ string, baseline []deployment.ProcessIdentity) error {
	b.revokeCalls++
	b.revokedBaseline = append([]deployment.ProcessIdentity(nil), baseline...)
	return b.revokeErr
}

func TestBorrowedRequiresBaselineAndRetriesOriginalRevocation(t *testing.T) {
	backend := &backendStub{snapshotErr: errors.New("snapshot uncertain")}
	if _, err := New(context.Background(), validDescriptor(), backend); err == nil {
		t.Fatal("admission without process baseline accepted")
	}
	backend.snapshotErr = nil
	borrowed, err := New(context.Background(), validDescriptor(), backend)
	if err != nil {
		t.Fatal(err)
	}
	backend.revokeErr = errors.New("backend unavailable")
	if err := borrowed.Revoke(context.Background()); err == nil {
		t.Fatal("uncertain process sweep accepted")
	}
	if _, err := borrowed.ExecStream(context.Background(), core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("command admitted after revocation began")
	}
	backend.revokeErr = nil
	if err := borrowed.Revoke(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.revokeCalls != 2 || len(backend.revokedBaseline) != 1 || backend.revokedBaseline[0].StartTime != 2 {
		t.Fatal("retry replaced baseline")
	}
	if err := borrowed.Revoke(context.Background()); err != nil || backend.revokeCalls != 2 {
		t.Fatal("terminal revocation repeated backend mutation")
	}
}
