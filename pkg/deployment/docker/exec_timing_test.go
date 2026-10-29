package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

type timingDocker struct {
	*fakeDocker
	attachErr, inspectExecErr error
	reportRunning             bool
	beforeInspect             func()
}

func (f *timingDocker) ExecCreate(ctx context.Context, id string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	time.Sleep(2 * time.Millisecond)
	return f.fakeDocker.ExecCreate(ctx, id, options)
}
func (f *timingDocker) ExecAttach(ctx context.Context, id string, options client.ExecAttachOptions) (client.ExecAttachResult, error) {
	time.Sleep(2 * time.Millisecond)
	if f.attachErr != nil {
		return client.ExecAttachResult{}, f.attachErr
	}
	return f.fakeDocker.ExecAttach(ctx, id, options)
}
func (f *timingDocker) ExecInspect(ctx context.Context, id string, options client.ExecInspectOptions) (client.ExecInspectResult, error) {
	if f.beforeInspect != nil {
		f.beforeInspect()
	}
	time.Sleep(2 * time.Millisecond)
	if f.inspectExecErr != nil {
		return client.ExecInspectResult{}, f.inspectExecErr
	}
	r, err := f.fakeDocker.ExecInspect(ctx, id, options)
	r.Running = f.reportRunning
	return r, err
}
func newTimingDocker() *timingDocker {
	f := newFakeDocker()
	f.container = container.InspectResponse{ID: "sandbox", State: &container.State{Running: true}}
	f.execDelay = 3 * time.Millisecond
	return &timingDocker{fakeDocker: f}
}

func TestExecTimingSeparatesTransportSetupFromDrainedAndInspectedCompletion(t *testing.T) {
	ctx, timings := core.WithCommandTimings(t.Context())
	f := newTimingDocker()
	f.beforeInspect = func() {
		snapshot := timings.Snapshot()
		for _, name := range []string{"command.exited", "command.completed"} {
			if _, ok := snapshot.Milestones[name]; ok {
				t.Errorf("%s invented before inspection", name)
			}
		}
		found := false
		for _, stage := range snapshot.Stages {
			if stage.Name == "command.output_drain" {
				found = true
			}
		}
		if !found {
			t.Error("inspection began before output drain was measured")
		}
	}
	result, err := (&execution{client: f, containerID: "sandbox"}).Exec(ctx, core.Command{Path: "/launcher"})
	if err != nil || result.ExitCode != 0 || !strings.Contains(result.Stdout, "task complete") || result.Stderr != "agent diagnostic\n" {
		t.Fatalf("changed command result: %#v %v", result, err)
	}
	snapshot := timings.Snapshot()
	for _, name := range []string{"command.total", "command.create_exec", "command.attach_exec", "command.output_drain", "command.inspect_exec"} {
		found := false
		for _, stage := range snapshot.Stages {
			if stage.Name == name {
				found = true
				if stage.DurationMS < 1 {
					t.Errorf("native delay missing from %s: %+v", name, stage)
				}
			}
		}
		if !found {
			t.Errorf("missing %s", name)
		}
	}
	for _, name := range []string{"command.started", "command.exited", "command.completed"} {
		if _, ok := snapshot.Milestones[name]; !ok {
			t.Errorf("missing %s", name)
		}
	}
	if snapshot.Milestones["command.completed"].OffsetMS < snapshot.Milestones["command.started"].OffsetMS {
		t.Error("completion preceded attachment")
	}
}

func TestExecTimingDoesNotInventExitForFailedOrStillRunningExec(t *testing.T) {
	for _, test := range []struct {
		name                  string
		attachErr, inspectErr error
		running               bool
	}{
		{name: "attach failure", attachErr: errors.New("attach unavailable")},
		{name: "inspection failure", inspectErr: errors.New("inspection unavailable")},
		{name: "streams closed but process running", running: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, timings := core.WithCommandTimings(t.Context())
			f := newTimingDocker()
			f.attachErr = test.attachErr
			f.inspectExecErr = test.inspectErr
			f.reportRunning = test.running
			result, err := (&execution{client: f, containerID: "sandbox"}).Exec(ctx, core.Command{Path: "/launcher"})
			expectedErr := test.attachErr
			if expectedErr == nil {
				expectedErr = test.inspectErr
			}
			if expectedErr != nil && !errors.Is(err, expectedErr) {
				t.Fatalf("lost native error: %v", err)
			}
			if expectedErr == nil && (err != nil || result.ExitCode != 0) {
				t.Fatalf("changed running inspection semantics: %#v %v", result, err)
			}
			snapshot := timings.Snapshot()
			for _, name := range []string{"command.exited", "command.completed"} {
				if _, ok := snapshot.Milestones[name]; ok {
					t.Errorf("invented %s: %+v", name, snapshot)
				}
			}
			if test.attachErr != nil {
				if _, ok := snapshot.Milestones["command.started"]; ok {
					t.Error("failed attachment marked command started")
				}
			}
			stages := map[string]bool{}
			for _, s := range snapshot.Stages {
				stages[s.Name] = true
			}
			if !stages["command.total"] || !stages["command.attach_exec"] {
				t.Errorf("lost partial timings: %+v", snapshot.Stages)
			}
			if test.inspectErr != nil && !stages["command.inspect_exec"] {
				t.Error("failed inspection has no timing")
			}
		})
	}
}
