package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/deployment"
)

// A warm pool keeps pods of one spec running before any task asks for one,
// and a task claims one of them instead of creating its own. The run owns one
// SandboxTemplate and SandboxWarmPool per distinct pod spec, created by the
// first claim that needs it and removed by RunEnvironment.Stop.
//
// A pooled pod exists before its task, so it is weaker than a pod ARIES
// creates for the task: its name is the pool's, not the runtime name ARIES
// chose, and the task's identity labels and annotations are added after it
// already runs. Until then the template's own NetworkPolicy denies all its
// ingress and egress. The task's policy, applied before the claim, selects it
// once labelled; policies add up, so the task's rules then apply.

const (
	claimAPIVersion  = "extensions.agents.x-k8s.io/v1beta1"
	claimResource    = "sandboxclaims.extensions.agents.x-k8s.io"
	templateResource = "sandboxtemplates.extensions.agents.x-k8s.io"
	poolResource     = "sandboxwarmpools.extensions.agents.x-k8s.io"
	// runHashLabel selects one run's templates and pools. Run IDs exceed the
	// label-value limit, so it carries a hash and the ID is an annotation.
	runHashLabel = "aries.dev/run-hash"
)

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

// claimCreate claims a pooled pod for the request and gives it the request's
// identity. It returns the pod's name; on a failure after the claim exists it
// returns the claim's name, which Stop also accepts.
func (manager *Manager) claimCreate(ctx context.Context, request deployment.Request) (string, error) {
	runID := request.Labels["aries.run"]
	if runID == "" {
		return "", errors.New("Kubernetes warm pool requires a run-owned request")
	}
	pool, err := manager.ensurePool(ctx, runID, request)
	if err != nil {
		return "", err
	}
	claim, err := json.Marshal(map[string]any{
		"apiVersion": claimAPIVersion, "kind": "SandboxClaim",
		"metadata": map[string]any{
			"name": request.Name, "namespace": manager.options.Namespace,
			"labels":      map[string]string{managedByLabel: "aries", runtimeLabel: request.Name},
			"annotations": request.Labels,
		},
		"spec": map[string]any{
			"warmPoolRef": map[string]string{"name": pool},
			// Deleting the claim then waits for the Sandbox and its pod.
			"lifecycle": map[string]string{"shutdownPolicy": "DeleteForeground"},
		},
	})
	if err != nil {
		return "", err
	}
	if _, err := manager.run(ctx, claim, "create", "-n", manager.options.Namespace, "-f", "-"); err != nil {
		return manager.recoverCreate(ctx, request, fmt.Errorf("create agent-sandbox SandboxClaim: %w", err))
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.options.ReadyTimeout)
	defer cancel()
	timeout := strconv.Itoa(int(manager.options.ReadyTimeout.Seconds())) + "s"
	if _, err := manager.run(readyCtx, nil, "wait", "-n", manager.options.Namespace, "--for=condition=Ready", claimResource+"/"+request.Name, "--timeout="+timeout); err != nil {
		return request.Name, fmt.Errorf("wait for SandboxClaim %s: %w", request.Name, err)
	}
	raw, err := manager.run(ctx, nil, "get", claimResource, "-n", manager.options.Namespace, request.Name, "-o", "jsonpath={.status.sandbox.name}")
	if err != nil {
		return request.Name, fmt.Errorf("read SandboxClaim %s: %w", request.Name, err)
	}
	pod := strings.TrimSpace(string(raw))
	if !validName(pod) {
		return request.Name, fmt.Errorf("SandboxClaim %s names no Sandbox", request.Name)
	}
	// The task's NetworkPolicy and Service select runtimeLabel, and the bridge
	// checks it and the annotations before it executes in the pod.
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"labels":      map[string]string{runtimeLabel: request.Name},
		"annotations": request.Labels,
	}})
	if err != nil {
		return request.Name, err
	}
	if _, err := manager.run(ctx, nil, "patch", "pod", "-n", manager.options.Namespace, pod, "--type=merge", "-p", string(patch)); err != nil {
		return request.Name, fmt.Errorf("label claimed pod %s: %w", pod, err)
	}
	return pod, nil
}

// ensurePool creates the run's template and pool for this request's pod spec,
// or reuses the ones another task of the run created.
func (manager *Manager) ensurePool(ctx context.Context, runID string, request deployment.Request) (string, error) {
	labels, spec, err := manager.podParts(request)
	if err != nil {
		return "", err
	}
	podTemplate := map[string]any{"metadata": map[string]any{"labels": labels}, "spec": spec}
	key, err := json.Marshal(podTemplate)
	if err != nil {
		return "", err
	}
	name := "aries-pool-" + shortHash(runID+"\x00"+string(key))
	owner := map[string]any{
		"name": name, "namespace": manager.options.Namespace,
		"labels":      map[string]string{managedByLabel: "aries", runHashLabel: shortHash(runID)},
		"annotations": map[string]string{"aries.managed": "true", "aries.kind": "sandbox-pool", "aries.run": runID},
	}
	template, err := json.Marshal(map[string]any{
		"apiVersion": claimAPIVersion, "kind": "SandboxTemplate", "metadata": owner,
		"spec": map[string]any{
			"podTemplate": podTemplate, "service": false, "failedPodPolicy": "Retain",
			// Empty rule lists deny all ingress and egress to pooled pods
			// until a task's own policy selects one.
			"networkPolicyManagement": "Managed",
			"networkPolicy":           map[string]any{"ingress": []any{}, "egress": []any{}},
		},
	})
	if err != nil {
		return "", err
	}
	pool, err := json.Marshal(map[string]any{
		"apiVersion": claimAPIVersion, "kind": "SandboxWarmPool", "metadata": owner,
		"spec": map[string]any{"replicas": manager.options.WarmPool, "sandboxTemplateRef": map[string]string{"name": name}},
	})
	if err != nil {
		return "", err
	}
	for _, object := range []struct {
		kind     string
		manifest []byte
	}{{"SandboxTemplate", template}, {"SandboxWarmPool", pool}} {
		if _, err := manager.run(ctx, object.manifest, "create", "-n", manager.options.Namespace, "-f", "-"); err != nil && !strings.Contains(err.Error(), "AlreadyExists") {
			return "", fmt.Errorf("create agent-sandbox %s: %w", object.kind, err)
		}
	}
	return name, nil
}

// stopClaimed deletes the claim behind a pooled pod and confirms that the
// claim, its Sandbox and the pod are gone. id is the pod's name, or the
// claim's when Create failed before the pod was known.
func (manager *Manager) stopClaimed(ctx context.Context, id string) error {
	claim := id
	pod, found, err := manager.pod(ctx, id)
	if err != nil {
		return fmt.Errorf("inspect claimed pod: %w", err)
	}
	if found && pod.Metadata.Labels[runtimeLabel] != "" {
		claim = pod.Metadata.Labels[runtimeLabel]
	}
	if _, err := manager.run(ctx, nil, "delete", claimResource, "-n", manager.options.Namespace, claim, "--ignore-not-found", "--cascade=foreground", "--wait=true"); err != nil {
		return fmt.Errorf("delete agent-sandbox SandboxClaim: %w", err)
	}
	for _, check := range []struct{ resource, name string }{{claimResource, claim}, {sandboxResource, id}, {"pod", id}} {
		_, found, err := manager.object(ctx, check.resource, check.name)
		if err != nil {
			return fmt.Errorf("verify %s removal: %w", check.resource, err)
		}
		if found {
			return fmt.Errorf("deployment %s %s remains after removal", check.resource, check.name)
		}
	}
	return nil
}

// deletePools removes the run's templates and pools, and with them the pooled
// pods and the templates' policies, then confirms that none remain.
func (manager *Manager) deletePools(ctx context.Context, runID string) error {
	selector := runHashLabel + "=" + shortHash(runID)
	namespace := manager.options.Namespace
	// The pool goes first, so it does not replace pods while its template is
	// being removed.
	for _, resource := range []string{poolResource, templateResource} {
		deleteCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, err := manager.run(deleteCtx, nil, "delete", resource, "-n", namespace, "-l", selector, "--ignore-not-found", "--cascade=foreground", "--wait=true")
		cancel()
		if err != nil {
			return fmt.Errorf("delete agent-sandbox %s: %w", resource, err)
		}
		remaining, err := manager.run(ctx, nil, "get", resource, "-n", namespace, "-l", selector, "-o", "name")
		if err != nil {
			return fmt.Errorf("verify agent-sandbox %s removal: %w", resource, err)
		}
		if len(bytes.TrimSpace(remaining)) != 0 {
			return fmt.Errorf("agent-sandbox %s remain after removal: %s", resource, strings.TrimSpace(string(remaining)))
		}
	}
	return nil
}
