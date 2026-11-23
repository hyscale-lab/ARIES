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

func TestPrewarmSandboxesSendsEachTasksSandboxRequest(t *testing.T) {
	cpu, memory := 2.0, 4096
	var cfg config.Config
	cfg.Overrides.AgentSandboxResources = config.ResourceOverrides{CPU: &cpu, MemoryMB: &memory}
	tasks := []core.Task{
		{ID: "a", Environment: core.Environment{Image: "img-a"}},
		{ID: "b", Environment: core.Environment{Image: "img-b", Workdir: "/own"}},
	}
	var got []core.SandboxRequest
	wiring := Wiring{
		Workdirs: map[runner.WorkdirKey]string{{Image: "img-a"}: "/resolved"},
		Prewarm: func(_ context.Context, requests []core.SandboxRequest) error {
			got = requests
			return nil
		},
	}
	if err := prewarmSandboxes(context.Background(), cfg, "run-1", tasks, wiring, logrus.New()); err != nil {
		t.Fatal(err)
	}
	// Each request is the one the task's runner will make.
	for i, task := range tasks {
		want := runner.SandboxRequest("run-1", task, runtimeOverrides(cfg), wiring.Workdirs)
		if got[i].RunID != want.RunID || got[i].TaskID != want.TaskID || got[i].Environment.Workdir != want.Environment.Workdir ||
			got[i].Environment.CPU != 2 || got[i].Environment.MemoryMB != 4096 {
			t.Fatalf("request %d = %+v, want %+v", i, got[i], want)
		}
	}
	if got[0].Environment.Workdir != "/resolved" || got[1].Environment.Workdir != "/own" {
		t.Fatalf("workdirs = %q, %q", got[0].Environment.Workdir, got[1].Environment.Workdir)
	}

	wiring.Prewarm = func(context.Context, []core.SandboxRequest) error { return errors.New("pool not ready") }
	if err := prewarmSandboxes(context.Background(), cfg, "run-1", tasks, wiring, logrus.New()); err == nil {
		t.Fatal("a failed prewarm was not reported")
	}
}
