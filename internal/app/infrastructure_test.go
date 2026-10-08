package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

type infrastructureEnvironment struct {
	events  *[]string
	stopErr error
}

func (e *infrastructureEnvironment) event(s string) {
	if e.events != nil {
		*e.events = append(*e.events, s)
	}
}
func (e *infrastructureEnvironment) Start(context.Context) (core.RuntimePlacement, error) {
	e.event("network-start")
	return core.RuntimePlacement{AttachmentID: "network"}, nil
}
func (e *infrastructureEnvironment) NewTaskEnvironment() deployment.TaskEnvironment { return nil }
func (e *infrastructureEnvironment) NetworkID() string                              { return "network-id" }
func (e *infrastructureEnvironment) NetworkName() string                            { return "network" }
func (e *infrastructureEnvironment) Stop(ctx context.Context) error {
	e.event("network-stop")
	return errors.Join(e.stopErr, ctx.Err())
}

type infrastructureBridge struct {
	events            *[]string
	newSession        func(string) (runner.ToolBridge, error)
	startErr, stopErr error
}

func (b *infrastructureBridge) event(s string) {
	if b.events != nil {
		*b.events = append(*b.events, s)
	}
}
func (b *infrastructureBridge) Start(context.Context) error {
	b.event("bridge-start")
	return b.startErr
}
func (b *infrastructureBridge) NewSession(root string) (runner.ToolBridge, error) {
	return b.newSession(root)
}
func (b *infrastructureBridge) Stop(ctx context.Context) error {
	b.event("bridge-stop")
	return errors.Join(b.stopErr, ctx.Err())
}
func (b *infrastructureBridge) RuntimeID() string   { return "bridge-id" }
func (b *infrastructureBridge) RuntimeName() string { return "bridge-name" }

// Existing CLI tests inject their task roles into a no-effect shared run owner.
func runCommandForTest(ctx context.Context, path string, out io.Writer, deps Dependencies) error {
	w := deps.Wiring
	if w.NewInfrastructure == nil {
		deps.Wiring.NewInfrastructure = func(cfg config.Config, runID, root string, log *logrus.Logger) (*RunInfrastructure, error) {
			sandboxFactory := w.NewSandbox
			if sandboxFactory == nil {
				sandboxFactory = func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
					return SandboxInstance{}, errors.New("sandbox fixture absent")
				}
			}
			return &RunInfrastructure{Environment: &infrastructureEnvironment{}, Resources: &stubResources{}, NewSandbox: sandboxFactory, NetworkPolicy: "test-shared", NewBridge: func(core.RuntimePlacement) (SharedBridge, error) {
				return &infrastructureBridge{newSession: func(root string) (runner.ToolBridge, error) {
					if w.NewBridge == nil {
						return nil, errors.New("bridge fixture absent")
					}
					return w.NewBridge(cfg, root, log)
				}}, nil
			}}, nil
		}
	}
	return Run(ctx, path, out, deps)
}

type infrastructureResources struct {
	events  *[]string
	failure error
	closes  int
}

func (s *infrastructureResources) Sample(context.Context) ([]core.ResourceReading, error) {
	return nil, s.failure
}
func (s *infrastructureResources) Close() error {
	s.closes++
	if s.events != nil {
		*s.events = append(*s.events, "observer-close")
	}
	return nil
}

func TestInfrastructureFinalizesBeforePersistenceAndPreservesFailures(t *testing.T) {
	for _, startupFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "task-cancellation", true: "startup-failure"}[startupFailure], func(t *testing.T) {
			events := []string{}
			env := &infrastructureEnvironment{events: &events, stopErr: errors.New("network removal failed")}
			bridge := &infrastructureBridge{events: &events, stopErr: errors.New("bridge removal failed")}
			if startupFailure {
				bridge.startErr = errors.New("bridge startup failed")
			}
			resources := &infrastructureResources{events: &events}
			root := t.TempDir()
			if err := os.Chmod(root, 0o700); err != nil {
				t.Fatal(err)
			}
			w := Wiring{NewInfrastructure: func(config.Config, string, string, *logrus.Logger) (*RunInfrastructure, error) {
				return &RunInfrastructure{Environment: env, Resources: resources, NetworkPolicy: "shared-egress", NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
					return SandboxInstance{}, nil
				}, NewBridge: func(core.RuntimePlacement) (SharedBridge, error) { return bridge, nil }, Close: func() error { events = append(events, "transport-close"); return nil }}, nil
			}}
			ctx, cancel := context.WithCancel(context.Background())
			err := executeAndRecord(ctx, func(ctx context.Context) (core.RunResult, error) {
				return runWithInfrastructure(ctx, config.Config{Name: "test"}, "run", root, logrus.New(), w, func(context.Context, Wiring) (core.RunResult, error) {
					events = append(events, "tasks")
					cancel()
					return core.RunResult{Name: "test", RunID: "run"}, context.Canceled
				})
			}, root, io.Discard)
			cancel()
			if err == nil || !strings.Contains(err.Error(), "bridge removal failed") || !strings.Contains(err.Error(), "network removal failed") {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(root, "run-result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result core.RunResult
			if err := json.Unmarshal(content, &result); err != nil {
				t.Fatal(err)
			}
			infra := result.Infrastructure
			if infra == nil || infra.Cleanup.Status != core.StatusFailed || infra.NetworkID != "network-id" || infra.BridgeRuntimeID != "bridge-id" || infra.Observer.Status != core.StatusSucceeded || result.Summary.Tasks != 0 {
				t.Fatalf("result=%+v infrastructure=%+v", result, infra)
			}
			joined := strings.Join(events, ",")
			if !strings.HasSuffix(joined, "bridge-stop,observer-close,network-stop,transport-close") {
				t.Fatal(events)
			}
			if startupFailure && strings.Contains(joined, "tasks") {
				t.Fatal("admitted tasks after startup failure")
			}
		})
	}
}

func TestInfrastructureObserverFailureDoesNotPreventTasks(t *testing.T) {
	ran := false
	source := &infrastructureResources{failure: errors.New("observer failed")}
	w := Wiring{NewInfrastructure: func(config.Config, string, string, *logrus.Logger) (*RunInfrastructure, error) {
		return &RunInfrastructure{Environment: &infrastructureEnvironment{}, Resources: source, NetworkPolicy: "shared-egress", NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{}, nil
		}, NewBridge: func(core.RuntimePlacement) (SharedBridge, error) { return &infrastructureBridge{}, nil }}, nil
	}}
	result, err := runWithInfrastructure(context.Background(), config.Config{Name: "test"}, "run", t.TempDir(), logrus.New(), w, func(context.Context, Wiring) (core.RunResult, error) {
		ran = true
		return core.RunResult{RunID: "run"}, nil
	})
	if !ran || err == nil || result.Infrastructure.Observer.Status != core.StatusFailed || !strings.Contains(result.Infrastructure.Error, "observer failed") {
		t.Fatal(ran, err, result.Infrastructure)
	}
	if source.closes != 1 {
		t.Fatalf("resource source closed %d times", source.closes)
	}
}

func TestTaskFailureDoesNotBecomeInfrastructureFailure(t *testing.T) {
	w := Wiring{NewInfrastructure: func(config.Config, string, string, *logrus.Logger) (*RunInfrastructure, error) {
		return &RunInfrastructure{Environment: &infrastructureEnvironment{}, Resources: &infrastructureResources{}, NetworkPolicy: "shared-egress", NewSandbox: func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error) {
			return SandboxInstance{}, nil
		}, NewBridge: func(core.RuntimePlacement) (SharedBridge, error) { return &infrastructureBridge{}, nil }}, nil
	}}
	taskErr := errors.New("task failed")
	result, err := runWithInfrastructure(context.Background(), config.Config{Name: "test"}, "run", t.TempDir(), logrus.New(), w, func(context.Context, Wiring) (core.RunResult, error) { return core.RunResult{RunID: "run"}, taskErr })
	if !errors.Is(err, taskErr) || result.Infrastructure.Error != "" || result.Infrastructure.Cleanup.Status != core.StatusSucceeded {
		t.Fatal(err, result.Infrastructure)
	}
}
