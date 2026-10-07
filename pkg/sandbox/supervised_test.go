package sandbox

import (
	"context"
	"io"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

type supervisedFakeDeployment struct {
	*fakeDeployment
	configuredUser string
	request        deployment.Request
	command        core.Command
	calls          int
}

func (f *supervisedFakeDeployment) ExecSupervisedStream(_ context.Context, _ string, request deployment.Request, command core.Command, _ io.Reader, _, _ io.Writer) (core.CommandResult, error) {
	f.calls++
	f.request, f.command = request, command
	return core.CommandResult{ExitCode: 0}, nil
}

func (f *supervisedFakeDeployment) ConfiguredUser(_ context.Context, _ string, request deployment.Request) (string, error) {
	f.calls++
	f.request = request
	return f.configuredUser, nil
}

func startSupervisedSandbox(t *testing.T, execUser string) (*Sandbox, *supervisedFakeDeployment) {
	t.Helper()
	base := &fakeDeployment{}
	f := &supervisedFakeDeployment{fakeDeployment: base}
	m, err := New(Options{Deployment: f, NewEnvironment: func() deployment.TaskEnvironment { return &fakeEnvironment{f: base} }, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	request.Environment.ExecUser = execUser
	live, err := m.Start(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return live.(*Sandbox), f
}

func TestSupervisedExecutionValidatesAgainstTheSandboxRequest(t *testing.T) {
	s, f := startSupervisedSandbox(t, "65532:65532")
	if _, err := s.ExecSupervisedStream(context.Background(), core.Command{Path: "/supervisor"}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if f.command.Dir != "/work" || f.command.User != "65532:65532" {
		t.Fatalf("command defaults = %+v", f.command)
	}
	if f.request.Labels["aries.kind"] != "task-container" || f.request.Labels["aries.task"] != "task" || !f.request.NoNewPrivileges || f.request.Workdir != "/work" {
		t.Fatalf("supervised request = %+v", f.request)
	}
}

func TestSupervisedCapabilitiesRequireOwnedLiveSandbox(t *testing.T) {
	for _, invalid := range []string{"unowned", "stopped", "stopping", "invalid command", "unsupported deployment"} {
		t.Run(invalid, func(t *testing.T) {
			s, f := startSupervisedSandbox(t, "")
			command := core.Command{Path: "/supervisor"}
			switch invalid {
			case "unowned":
				s.containerOwned = false
			case "stopped":
				s.stopped = true
			case "stopping":
				s.stopping = true
			case "invalid command":
				command.Args = []string{"has\x00nul"}
			case "unsupported deployment":
				s.deployment = f.fakeDeployment
			}
			if _, err := s.ExecSupervisedStream(context.Background(), command, nil, nil, nil); err == nil {
				t.Fatal("invalid supervised execution accepted")
			}
			if invalid != "invalid command" {
				if _, err := s.TaskUser(context.Background()); err == nil {
					t.Fatal("invalid task-user ownership accepted")
				}
			}
			if f.calls != 0 {
				t.Fatal("invalid supervised operation reached the deployment")
			}
		})
	}
}

func TestTaskUserResolvesDeclaredAndImageUsers(t *testing.T) {
	for _, test := range []struct{ name, declared, image, want string }{
		{"default root", "", "", "0:0"},
		{"image numeric", "", "1000:1001", "1000:1001"},
		{"image named", "", "app:appgroup", "app:appgroup"},
		{"declared overrides image", "65532:65532", "app", "65532:65532"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, f := startSupervisedSandbox(t, test.declared)
			f.configuredUser = test.image
			got, err := s.TaskUser(context.Background())
			if err != nil || got != test.want {
				t.Fatalf("TaskUser = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
