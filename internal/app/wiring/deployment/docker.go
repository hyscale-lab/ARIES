// Package deployment constructs the selected deployment infrastructure.
package deployment

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/config"
	dockerdeployment "github.com/hyscale-lab/aries/pkg/deployment/docker"
	"github.com/hyscale-lab/aries/pkg/monitor"
	"github.com/sirupsen/logrus"
)

func NewDocker(cfg config.DeploymentConfig, logger *logrus.Logger) (*dockerdeployment.Manager, error) {
	return dockerdeployment.New(dockerdeployment.Options{Socket: cfg.Docker.Socket, Logger: logger})
}

// NewDockerSandbox constructs the transport and occurrence-scoped monitoring
// source. The caller owns both on success; partial construction closes transport.
func NewDockerSandbox(cfg config.DeploymentConfig, runID, occurrenceID string, logger *logrus.Logger) (*dockerdeployment.Manager, monitor.ResourceSource, error) {
	transport, err := NewDocker(cfg, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("construct sandbox deployment: %w", err)
	}
	source, err := dockerdeployment.NewResourceSource(dockerdeployment.ResourceOptions{DockerSocket: cfg.Docker.Socket, RunID: runID, TaskIDs: []string{occurrenceID}})
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("construct Docker resource source: %w", err), transport.Close())
	}
	return transport, source, nil
}

func PullDockerImages(ctx context.Context, cfg config.DeploymentConfig, images []string) error {
	return dockerdeployment.PullImages(ctx, cfg.Docker.Socket, images)
}
