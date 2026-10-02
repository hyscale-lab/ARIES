// Package sandbox assembles the tool sandbox from selected infrastructure.
package sandbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/monitor"
	nvidiamonitor "github.com/hyscale-lab/aries/pkg/monitor/nvidia"
	tasksandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/sirupsen/logrus"
)

// New takes ownership of transport and resources, closing both on construction
// failure. On success the sandbox owns transport and the observer owns Resources.
func New(outputRoot, occurrenceID string, gpuIndices []int, logger *logrus.Logger, transport deployment.Deployment, newEnvironment func() deployment.TaskEnvironment, resources monitor.ResourceSource) (app.SandboxInstance, error) {
	manager, err := tasksandbox.New(tasksandbox.Options{Deployment: transport, NewEnvironment: newEnvironment, OutputDir: outputRoot, Logger: logger})
	if err != nil {
		return app.SandboxInstance{}, errors.Join(fmt.Errorf("construct tool sandbox: %w", err), resources.Close(), transport.Close())
	}
	if len(gpuIndices) != 0 {
		gpuSource, err := nvidiamonitor.NewSource(nvidiamonitor.Options{TaskID: occurrenceID, GPUIndices: gpuIndices})
		if err != nil {
			return app.SandboxInstance{}, errors.Join(fmt.Errorf("construct NVIDIA resource source: %w", err), resources.Close(), manager.Close())
		}
		resources = &combinedResourceSource{container: resources, gpu: gpuSource}
	}
	return app.SandboxInstance{Sandbox: manager, Resources: resources, Close: manager.Close, BridgeListen: manager.BridgeListen}, nil
}

type combinedResourceSource struct {
	container monitor.ResourceSource
	gpu       monitor.ResourceSource
}

func (source *combinedResourceSource) Sample(ctx context.Context) ([]core.ResourceReading, error) {
	containerReadings, err := source.container.Sample(ctx)
	if err != nil {
		return nil, err
	}
	gpuReadings, err := source.gpu.Sample(ctx)
	if err != nil {
		return nil, err
	}
	return append(containerReadings, gpuReadings...), nil
}
func (source *combinedResourceSource) Close() error {
	return errors.Join(source.gpu.Close(), source.container.Close())
}
