package kubernetes

import (
	"context"
	"errors"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
)

func (manager *Manager) BridgeBackend() string { return "kubernetes" }

// ValidateBridgeTarget confirms the pod is this run's live sandbox before the
// bridge executes in it. Pod names carry a random nonce and are never reused.
// The pod is the runtime; the sandbox is the runtime name ARIES chose, which
// it labels the pod with. They differ for a pod claimed from a warm pool.
func (manager *Manager) ValidateBridgeTarget(ctx context.Context, d core.BridgeTarget) error {
	if d.Backend != "kubernetes" {
		return errors.New("Kubernetes cannot execute another target backend")
	}
	pod, found, err := manager.pod(ctx, d.RuntimeID)
	if err != nil {
		return fmt.Errorf("inspect borrowed sandbox: %w", err)
	}
	if !found || pod.Status.Phase != "Running" || pod.Metadata.DeletionTimestamp != "" {
		return errors.New("borrowed sandbox identity or running state differs")
	}
	if pod.Metadata.Labels[managedByLabel] != "aries" || pod.Metadata.Labels[componentLabel] != "sandbox" || pod.Metadata.Labels[runtimeLabel] != d.SandboxID {
		return errors.New("borrowed sandbox ownership labels differ")
	}
	for key, value := range map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": d.RunID, "aries.task": d.TaskID} {
		if value == "" || pod.Metadata.Annotations[key] != value {
			return fmt.Errorf("borrowed sandbox ownership label %s differs", key)
		}
	}
	return nil
}
