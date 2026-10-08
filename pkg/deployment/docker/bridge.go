package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/client"
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
	name := strings.TrimPrefix(c.Name, "/")
	if c.ID != d.RuntimeID || name != d.SandboxID || c.Config == nil || c.State == nil || !c.State.Running {
		return errors.New("borrowed sandbox identity or running state differs")
	}
	for key, value := range map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": d.RunID, "aries.task": d.TaskID} {
		if value == "" || c.Config.Labels[key] != value {
			return fmt.Errorf("borrowed sandbox ownership label %s differs", key)
		}
	}
	return nil
}
