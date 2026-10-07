package docker

import (
	"context"
	"errors"
	"fmt"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/client"
	"strings"
)

func (manager *Manager) BridgeBackend() string { return "docker" }

// ValidateBridgeTarget inspects an immutable ID; it never resolves a reusable name.
func (manager *Manager) ValidateBridgeTarget(ctx context.Context, d core.BridgeTarget) error {
	if d.Backend != "docker" {
		return errors.New("Docker cannot execute another target backend")
	}
	result, err := manager.client.ContainerInspect(ctx, d.RuntimeID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect borrowed sandbox: %w", err)
	}
	c := result.Container
	if c.ID != d.RuntimeID || strings.TrimPrefix(c.Name, "/") != d.RuntimeName || c.Config == nil || c.State == nil || !c.State.Running {
		return errors.New("borrowed sandbox identity or running state differs")
	}
	for k, v := range d.ExpectedLabels {
		if c.Config.Labels[k] != v {
			return fmt.Errorf("borrowed sandbox ownership label %s differs", k)
		}
	}
	return nil
}

func (manager *Manager) SnapshotBridgeProcesses(ctx context.Context, id string) ([]deployment.ProcessIdentity, error) {
	return deployment.SnapshotBridgeProcesses(ctx, manager, id)
}
func (manager *Manager) RevokeBridgeProcesses(ctx context.Context, id string, baseline []deployment.ProcessIdentity) error {
	return deployment.RevokeBridgeProcesses(ctx, manager, id, baseline)
}
