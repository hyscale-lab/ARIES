package apptainer

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
)

func (manager *Manager) BridgeBackend() string { return "apptainer" }

// ValidateBridgeTarget confirms the instance is this run's live sandbox before
// the bridge executes in it. Instance names carry a random nonce and are never
// reused, and the record ties the name to the instance's PID.
func (manager *Manager) ValidateBridgeTarget(ctx context.Context, d core.BridgeTarget) error {
	if d.Backend != "apptainer" {
		return errors.New("Apptainer cannot execute another target backend")
	}
	if d.RuntimeID != d.SandboxID {
		return errors.New("borrowed sandbox identity differs from its runtime")
	}
	current, _, err := manager.loadRecord(d.RuntimeID)
	if err != nil {
		return fmt.Errorf("inspect borrowed sandbox: %w", err)
	}
	running, err := manager.Running(ctx, d.RuntimeID)
	if err != nil {
		return fmt.Errorf("inspect borrowed sandbox: %w", err)
	}
	if !running {
		return errors.New("borrowed sandbox identity or running state differs")
	}
	for key, value := range map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": d.RunID, "aries.task": d.TaskID} {
		if value == "" || current.Labels[key] != value {
			return fmt.Errorf("borrowed sandbox ownership label %s differs", key)
		}
	}
	return nil
}
