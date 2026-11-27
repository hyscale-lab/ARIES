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

// Docker process limits are fixed isolation properties, not per-task knobs.
// Agents run whatever a repository offers, and test runners size their worker
// pools from the host's CPU count rather than the cgroup's, so one task can
// otherwise hold thousands of processes at its memory cap and push the node's
// load into the thousands (observed with jest at 6600 processes). The sandbox
// limit is generous for real builds and test suites. The agent's tools run in
// the sandbox, so a harness needs only the agent process, its subprocesses and
// its bridge clients.
const (
	sandboxPidsLimit int64 = 4096
	harnessPidsLimit int64 = 1024
)

// NewDocker constructs a transport whose runtimes have no process limit, for
// the run network and the bridge.
func NewDocker(cfg config.DeploymentConfig, logger *logrus.Logger) (*dockerdeployment.Manager, error) {
	return dockerdeployment.New(dockerdeployment.Options{Socket: cfg.Docker.Socket, Logger: logger})
}

// NewDockerHarness constructs the transport for harness containers.
func NewDockerHarness(cfg config.DeploymentConfig, logger *logrus.Logger) (*dockerdeployment.Manager, error) {
	return dockerdeployment.New(dockerdeployment.Options{Socket: cfg.Docker.Socket, Logger: logger, PidsLimit: harnessPidsLimit})
}

// NewDockerSandbox constructs the transport and occurrence-scoped monitoring
// source. The caller owns both on success; partial construction closes transport.
func NewDockerSandbox(cfg config.DeploymentConfig, runID, occurrenceID string, logger *logrus.Logger) (*dockerdeployment.Manager, monitor.ResourceSource, error) {
	transport, err := dockerdeployment.New(dockerdeployment.Options{Socket: cfg.Docker.Socket, Logger: logger, PidsLimit: sandboxPidsLimit})
	if err != nil {
		return nil, nil, fmt.Errorf("construct sandbox deployment: %w", err)
	}
	source, err := dockerdeployment.NewResourceSource(dockerdeployment.ResourceOptions{DockerSocket: cfg.Docker.Socket, RunID: runID, TaskIDs: []string{occurrenceID}})
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("construct Docker resource source: %w", err), transport.Close())
	}
	return transport, source, nil
}

// BuildDockerImage builds image from dockerfile and the context files unless
// the daemon already has it.
func BuildDockerImage(ctx context.Context, cfg config.DeploymentConfig, image, dockerfile string, files map[string][]byte, buildArgs map[string]string) error {
	return dockerdeployment.BuildImage(ctx, cfg.Docker.Socket, image, dockerfile, files, buildArgs)
}

func PullDockerImages(ctx context.Context, cfg config.DeploymentConfig, images []string) error {
	return dockerdeployment.PullImages(ctx, cfg.Docker.Socket, images)
}

// NewDockerRunResources observes the bridge using its selected deployment.
func NewDockerRunResources(cfg config.DeploymentConfig, runID string) (monitor.ResourceSource, error) {
	if cfg.Backend != "docker" {
		return nil, fmt.Errorf("unsupported bridge resource backend %q", cfg.Backend)
	}
	return dockerdeployment.NewResourceSource(dockerdeployment.ResourceOptions{Scope: "run", DockerSocket: cfg.Docker.Socket, RunID: runID})
}
