package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

type probedSandbox struct {
	stubSandbox
	workdir string
}

func (s *probedSandbox) Workdir() string { return s.workdir }

type probingToolSandbox struct {
	stubToolSandbox
	mu       sync.Mutex
	requests []core.SandboxRequest
	stopped  int
}

func (p *probingToolSandbox) Start(_ context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if request.Environment.Image == "broken" {
		return nil, errors.New("pull failed")
	}
	return &probedSandbox{workdir: "/from/" + request.Environment.Image}, nil
}

func (p *probingToolSandbox) Stop(context.Context, runner.Sandbox) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped++
	return nil
}

// Each image that names no working directory is probed once; an image whose
// probe fails is left out, so its tasks fall back to a later harness start.
func TestResolveWorkdirsProbesEachImageOnce(t *testing.T) {
	probe := &probingToolSandbox{}
	wiring := Wiring{NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
		return SandboxInstance{Sandbox: probe, Resources: &stubResources{}}, nil
	}}
	tasks := []core.Task{
		{ID: "a", Environment: core.Environment{Image: "python"}},
		{ID: "b", Environment: core.Environment{Image: "python"}},
		{ID: "c", Environment: core.Environment{Image: "python", ExecUser: "1000:1000"}},
		{ID: "d", Environment: core.Environment{Image: "named", Workdir: "/app"}},
		{ID: "e", Environment: core.Environment{Image: "broken"}},
	}
	workdirs := resolveWorkdirs(context.Background(), config.Config{}, "run", t.TempDir(), tasks, wiring, logrus.New())
	want := map[runner.WorkdirKey]string{{Image: "python"}: "/from/python", {Image: "python", ExecUser: "1000:1000"}: "/from/python"}
	if len(workdirs) != len(want) || workdirs[runner.WorkdirKey{Image: "python"}] != "/from/python" || workdirs[runner.WorkdirKey{Image: "python", ExecUser: "1000:1000"}] != "/from/python" {
		t.Fatalf("workdirs = %v", workdirs)
	}
	if len(probe.requests) != 3 || probe.stopped != 2 {
		t.Fatalf("probes = %d, stopped = %d", len(probe.requests), probe.stopped)
	}
	for _, request := range probe.requests {
		if request.Environment.Workdir != "" || request.Environment.Image == "named" {
			t.Fatalf("probe request = %#v", request)
		}
	}
}
