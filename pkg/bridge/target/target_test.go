package target

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
)

type backendStub struct {
	command       core.Command
	id            string
	validationErr error
	validated     []core.BridgeTarget
	calls         int
}

func (b *backendStub) ValidateBridgeTarget(_ context.Context, d core.BridgeTarget) error {
	b.validated = append(b.validated, d)
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
	for _, backendName := range []string{"docker", "remote-exec"} {
		d := validDescriptor()
		d.Backend = backendName
		d.ExpectedLabels["aries.task"] = "other"
		backend := &backendStub{}
		if _, err := New(context.Background(), d, backend); err == nil || len(backend.validated) != 0 {
			t.Fatalf("cross-task descriptor reached %s backend: %v", backendName, err)
		}
	}
	d := validDescriptor()
	if _, err := New(context.Background(), d, &backendStub{validationErr: errors.New("wrong immutable runtime")}); err == nil {
		t.Fatal("failed backend validation accepted")
	}
}

func TestBorrowedDelegatesBackendSupportAndResourceIdentity(t *testing.T) {
	for _, validationErr := range []error{nil, errors.New("resource belongs to a different task")} {
		name := "accepted"
		if validationErr != nil {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			d := validDescriptor()
			d.Backend = "remote-exec"
			backend := &backendStub{validationErr: validationErr}
			borrowed, err := New(context.Background(), d, backend)
			if len(backend.validated) != 1 || !reflect.DeepEqual(backend.validated[0], d) {
				t.Fatalf("provider did not receive the immutable grant: %+v", backend.validated)
			}
			if validationErr != nil {
				if !errors.Is(err, validationErr) || borrowed != nil {
					t.Fatalf("failed provider validation admitted target: borrowed=%v err=%v", borrowed, err)
				}
				return
			}
			if err != nil || borrowed == nil {
				t.Fatalf("valid alternative backend denied: %v", err)
			}
		})
	}
}

func TestBorrowedRejectsMalformedBackendBeforeProviderAdmission(t *testing.T) {
	for _, name := range []string{"", "../remote", "remote exec", "remote\nexec", strings.Repeat("x", 129)} {
		t.Run(name, func(t *testing.T) {
			d := validDescriptor()
			d.Backend = name
			backend := &backendStub{}
			if _, err := New(context.Background(), d, backend); err == nil || len(backend.validated) != 0 {
				t.Fatalf("invalid backend reached provider: validations=%d err=%v", len(backend.validated), err)
			}
		})
	}
}

func TestBorrowedRevocationClosesAdmissionWithoutBackendOperations(t *testing.T) {
	backend := &backendStub{}
	borrowed, err := New(context.Background(), validDescriptor(), backend)
	if err != nil {
		t.Fatal(err)
	}
	// Closing bridge access does not depend on sandbox availability or mutate it.
	backend.validationErr = errors.New("backend unavailable")
	borrowed.Revoke()
	if _, err := borrowed.ExecStream(context.Background(), core.Command{Path: "/bin/true"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("command admitted after revocation")
	}
	borrowed.Revoke()
	if backend.calls != 0 || len(backend.validated) != 1 {
		t.Fatalf("revocation reached sandbox backend: executions=%d validations=%d", backend.calls, len(backend.validated))
	}
}
