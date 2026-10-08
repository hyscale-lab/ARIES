package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

// SharedBridge is run-owned. Runner receives a fresh session for each sandbox.
type SharedBridge interface {
	Start(context.Context) error
	NewSession(string) (runner.ToolBridge, error)
	Stop(context.Context) error
	RuntimeID() string
	RuntimeName() string
}

type SandboxFactory func(config.Config, string, string, string, []int, *logrus.Logger) (SandboxInstance, error)

// RunInfrastructure contains explicit run-owned dependencies and task factories.
// Construction makes no allocations; Close releases construction transports.
type RunInfrastructure struct {
	Environment   deployment.RunEnvironment
	NewBridge     func(core.RuntimePlacement) (SharedBridge, error)
	NewSandbox    SandboxFactory
	Resources     monitor.ResourceSource
	Close         func() error
	NetworkPolicy string
}

// runWithInfrastructure finalizes every run resource before executeAndRecord
// persists the result, including failures before any task is admitted.
func runWithInfrastructure(ctx context.Context, cfg config.Config, runID, outputRoot string, logger *logrus.Logger, wiring Wiring, execute func(context.Context, Wiring) (core.RunResult, error)) (result core.RunResult, returnErr error) {
	result = core.RunResult{Name: cfg.Name, RunID: runID}
	report := &core.InfrastructureResult{Observer: core.ObserverResult{Status: core.StatusNotStarted}, Cleanup: core.CleanupResult{Status: core.StatusNotStarted}}
	result.Infrastructure = report
	started := time.Now()
	if wiring.NewInfrastructure == nil {
		report.Error = "run infrastructure wiring is required"
		return result, errors.New(report.Error)
	}
	infra, err := wiring.NewInfrastructure(cfg, runID, outputRoot, logger)
	if err != nil {
		report.Error = err.Error()
		return result, err
	}
	var service SharedBridge
	var recorder *monitor.Recorder
	var observerStarted bool
	var recorderOwnsSource, executionStarted bool
	defer func() {
		if report.StartupDuration == 0 {
			report.StartupDuration = time.Since(started)
		}
		cleanupStarted := time.Now()
		var cleanupErrors []error
		// Each owner gets an independent deadline so one failure does not prevent the next cleanup.
		cleanup := func(name string, stop func(context.Context) error) {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := stop(cleanupCtx); err != nil {
				cleanupErrors = append(cleanupErrors, fmt.Errorf("%s: %w", name, err))
			}
		}
		if service != nil {
			report.BridgeRuntimeID = service.RuntimeID()
			report.BridgeRuntimeName = service.RuntimeName()
			cleanup("stop shared bridge", service.Stop)
		}
		if observerStarted {
			cleanup("finalize shared bridge observation", func(stopCtx context.Context) error {
				reports, err := recorder.Stop(stopCtx)
				if r, ok := reports[""]; ok {
					report.Observer = r
				}
				if err != nil {
					report.Observer.Status = core.StatusFailed
					report.Observer.Error = errors.Join(errors.New(report.Observer.Error), err).Error()
				}
				return err
			})
		} else if !recorderOwnsSource && infra.Resources != nil {
			if err := infra.Resources.Close(); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		if infra.Environment != nil {
			report.NetworkID = infra.Environment.NetworkID()
			report.NetworkName = infra.Environment.NetworkName()
			cleanup("remove run environment", infra.Environment.Stop)
		}
		if infra.Close != nil {
			if err := infra.Close(); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		report.Cleanup.Status = core.StatusSucceeded
		infrastructureErr := errors.Join(cleanupErrors...)
		if err := infrastructureErr; err != nil {
			report.Cleanup.Status = core.StatusFailed
			report.Cleanup.Error = err.Error()
		}
		if report.Observer.Status == core.StatusFailed {
			infrastructureErr = errors.Join(infrastructureErr, fmt.Errorf("shared bridge observer: %s", report.Observer.Error))
		}
		if !executionStarted {
			infrastructureErr = errors.Join(returnErr, infrastructureErr)
		}
		if infrastructureErr != nil {
			report.Error = infrastructureErr.Error()
		}
		returnErr = errors.Join(returnErr, infrastructureErr)
		report.CleanupDuration = time.Since(cleanupStarted)
		result.Infrastructure = report
	}()
	report.NetworkPolicy = infra.NetworkPolicy
	if infra.Environment == nil || infra.NewBridge == nil || infra.NewSandbox == nil || infra.Resources == nil {
		return result, errors.New("run infrastructure is incomplete")
	}
	placement, err := infra.Environment.Start(ctx)
	if err != nil {
		return result, fmt.Errorf("start run environment: %w", err)
	}
	service, err = infra.NewBridge(placement)
	if err != nil {
		return result, fmt.Errorf("construct shared bridge: %w", err)
	}
	recorder, err = monitor.New(monitor.Options{Scope: "run", RunID: runID, OutputDir: filepath.Join(outputRoot, "infrastructure", "bridge"), Source: infra.Resources, Logger: logger})
	if err == nil {
		recorderOwnsSource = true
		err = recorder.Start(ctx)
		observerStarted = err == nil
	}
	if err != nil {
		report.Observer.Status = core.StatusFailed
		report.Observer.Error = err.Error()
	}
	if err := service.Start(ctx); err != nil {
		return result, fmt.Errorf("start shared bridge: %w", err)
	}
	report.StartupDuration = time.Since(started)
	wiring.NewSandbox = infra.NewSandbox
	wiring.NewBridge = func(_ config.Config, root string, _ *logrus.Logger) (runner.ToolBridge, error) {
		return service.NewSession(root)
	}
	executionStarted = true
	result, returnErr = execute(ctx, wiring)
	return result, returnErr
}
