package deployment

import (
	"context"
	"errors"

	"github.com/hyscale-lab/aries/internal/app"
	"github.com/hyscale-lab/aries/pkg/config"
	"github.com/hyscale-lab/aries/pkg/deployment/apptainer"
	"github.com/sirupsen/logrus"
)

// DockerLeftovers lists what a run left on the Docker daemon, containers before
// the networks they joined, each removed as the run itself would remove it.
func DockerLeftovers(ctx context.Context, cfg config.DeploymentConfig, runID string, logger *logrus.Logger) ([]app.Leftover, func() error, error) {
	manager, err := NewDocker(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	containers, networks, err := manager.RunLeftovers(ctx, runID)
	if err != nil {
		return nil, nil, errors.Join(err, manager.Close())
	}
	var leftovers []app.Leftover
	for _, container := range containers {
		id := container.ID
		leftovers = append(leftovers, app.Leftover{Kind: "container", Name: container.Name, Remove: func(ctx context.Context) error { return manager.Stop(ctx, id) }})
	}
	for _, network := range networks {
		id := network.ID
		leftovers = append(leftovers, app.Leftover{Kind: "network", Name: network.Name, Remove: func(ctx context.Context) error { return manager.StopNetwork(ctx, id) }})
	}
	return leftovers, manager.Close, nil
}

// ApptainerLeftovers lists what a run left in its state directory: instances,
// then the bridge, whose exit ends the run's network namespace.
func ApptainerLeftovers(cfg config.DeploymentConfig, runID string, logger *logrus.Logger) ([]app.Leftover, func() error, error) {
	manager, err := NewApptainer(cfg, logger)
	if err != nil {
		return nil, nil, err
	}
	instances, err := apptainer.RunInstances(cfg.Apptainer.StateDir, runID)
	if err != nil {
		return nil, nil, err
	}
	processes, err := apptainer.RunProcesses(cfg.Apptainer.StateDir, runID)
	if err != nil {
		return nil, nil, err
	}
	// A bridge is stopped from its record, so no program needs mapping.
	bridges, err := apptainer.NewProcessRuntime(cfg.Apptainer.StateDir, nil)
	if err != nil {
		return nil, nil, err
	}
	var leftovers []app.Leftover
	for _, name := range instances {
		leftovers = append(leftovers, app.Leftover{Kind: "instance", Name: name, Remove: func(ctx context.Context) error { return manager.Stop(ctx, name) }})
	}
	for _, name := range processes {
		leftovers = append(leftovers, app.Leftover{Kind: "bridge process", Name: name, Remove: func(ctx context.Context) error { return bridges.Stop(ctx, name) }})
	}
	return leftovers, func() error { return errors.Join(manager.Close(), bridges.Close()) }, nil
}
