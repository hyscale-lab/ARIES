package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// NetworkPolicy is enforced by the CNI plugin, not by Kubernetes. Under a
// plugin that does not implement it the API server still accepts these objects
// and silently enforces nothing. See k8s/README.md for the CNI requirement.

// RunEnvironment borrows the configured namespace and owns one policy that
// keeps everything except ARIES's own non-sandbox pods off the run's bridge.
type RunEnvironment struct {
	mu               sync.Mutex
	manager          *Manager
	runID            string
	policy           string
	policyOwned      bool
	started, stopped bool
}

func (manager *Manager) NewRunEnvironment(runID string) *RunEnvironment {
	return &RunEnvironment{manager: manager, runID: runID}
}

// Start applies the bridge ingress policy before any runtime exists. The
// placement names the namespace runtimes attach to.
func (e *RunEnvironment) Start(ctx context.Context) (core.RuntimePlacement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return core.RuntimePlacement{}, errors.New("run environment cannot be reused")
	}
	e.started = true
	if e.runID == "" {
		return core.RuntimePlacement{}, errors.New("run environment requires a run ID")
	}
	namespace := e.manager.options.Namespace
	nonce, err := randomID()
	if err != nil {
		return core.RuntimePlacement{}, err
	}
	e.policy = "aries-run-" + nonce
	// The bridge's SSH listeners accept clients without authentication, so
	// reachability is the control: sandboxes and other namespaces are excluded.
	manifest, err := json.Marshal(map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{
			"name": e.policy, "namespace": namespace,
			"labels":      map[string]string{managedByLabel: "aries"},
			"annotations": map[string]string{"aries.kind": "run-network", "aries.run": e.runID},
		},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]string{managedByLabel: "aries", componentLabel: "bridge"}},
			"policyTypes": []any{"Ingress"},
			"ingress": []any{map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{
				"matchExpressions": []any{map[string]any{"key": componentLabel, "operator": "NotIn", "values": []string{"sandbox", "evaluation-sandbox"}}},
			}}}}},
		},
	})
	if err != nil {
		return core.RuntimePlacement{}, err
	}
	e.policyOwned = true // Retain cleanup ownership even if the response is lost.
	if _, err := e.manager.run(ctx, manifest, "create", "-n", namespace, "-f", "-"); err != nil {
		return core.RuntimePlacement{}, fmt.Errorf("create Kubernetes run network policy: %w", err)
	}
	return core.RuntimePlacement{AttachmentID: namespace}, nil
}

// Prewarm readies the run's warm pools for these sandbox requests before any
// task claims from them, so the run's first claim also takes a ready pod. Only
// the requests' pod specs matter. Without warm pools it does nothing.
func (e *RunEnvironment) Prewarm(ctx context.Context, requests []deployment.Request) error {
	if err := e.validate(); err != nil {
		return err
	}
	if e.manager.options.WarmPool == 0 {
		return nil
	}
	return e.manager.prewarm(ctx, e.runID, requests)
}

func (e *RunEnvironment) NewTaskEnvironment() deployment.TaskEnvironment {
	return &taskEnvironment{owner: e}
}

// NetworkID and NetworkName report the namespace, the run's shared attachment.
func (e *RunEnvironment) NetworkID() string   { return e.manager.options.Namespace }
func (e *RunEnvironment) NetworkName() string { return e.manager.options.Namespace }

func (e *RunEnvironment) validate() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.stopped || !e.policyOwned {
		return errors.New("run environment is not active")
	}
	return nil
}

// Stop removes only the policy and warm pools this run created, after its
// runtimes are gone.
func (e *RunEnvironment) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	if e.started && e.manager.options.WarmPool > 0 {
		if err := e.manager.deletePools(ctx, e.runID); err != nil {
			return err
		}
	}
	if e.policyOwned {
		if err := e.manager.deletePolicy(ctx, e.policy); err != nil {
			return err
		}
		e.policyOwned = false
	}
	e.stopped = true
	return nil
}

// taskEnvironment owns the NetworkPolicy that isolates one sandbox pod, and the
// Service that exposes the sandbox's task services to the harness.
type taskEnvironment struct {
	mu               sync.Mutex
	owner            *RunEnvironment
	policy           string
	service          string
	started, stopped bool
}

// Start applies the sandbox's policy before its pod exists. One that lands
// second leaves a window in which the container runs unisolated.
func (e *taskEnvironment) Start(ctx context.Context, request deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return core.HarnessConnectivity{}, errors.New("task environment cannot be reused")
	}
	e.started = true
	if request.RunID != e.owner.runID || !validName(request.RuntimeName) {
		return core.HarnessConnectivity{}, errors.New("task environment requires its run and a unique runtime name")
	}
	ports, err := taskServicePorts(request.Environment.Services)
	if err != nil {
		return core.HarnessConnectivity{}, err
	}
	if err := e.owner.validate(); err != nil {
		return core.HarnessConnectivity{}, err
	}
	manager := e.owner.manager
	manifest, err := manager.taskPolicyManifest(request, ports)
	if err != nil {
		return core.HarnessConnectivity{}, err
	}
	// Ownership is retained before each create, so Stop cleans up even when a
	// response is lost.
	e.policy = request.RuntimeName
	if _, err := manager.run(ctx, manifest, "create", "-n", manager.options.Namespace, "-f", "-"); err != nil {
		return core.HarnessConnectivity{}, fmt.Errorf("create Kubernetes task network policy: %w", err)
	}
	result := core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: manager.options.Namespace}}
	if len(ports) == 0 {
		return result, nil
	}
	// The harness needs the addresses before the sandbox pod exists, so the
	// services get a stable name: the same one Docker resolves on its network.
	service, err := manager.taskServiceManifest(request, ports)
	if err != nil {
		return core.HarnessConnectivity{}, err
	}
	e.service = request.RuntimeName
	if _, err := manager.run(ctx, service, "create", "-n", manager.options.Namespace, "-f", "-"); err != nil {
		return core.HarnessConnectivity{}, fmt.Errorf("create Kubernetes task service: %w", err)
	}
	services := request.Environment.Services
	if services.SearchPort != 0 {
		result.SearchURL = fmt.Sprintf("http://%s:%d", request.RuntimeName, services.SearchPort)
	}
	for _, mcp := range services.MCP {
		result.MCPServers = append(result.MCPServers, core.MCPServerConfig{
			Name: mcp.Name, URL: fmt.Sprintf("http://%s:%d%s", request.RuntimeName, mcp.Port, mcp.Path),
			Transport: mcp.Transport, TimeoutSeconds: mcp.TimeoutSeconds,
		})
	}
	return result, nil
}

// taskServicePorts lists the distinct ports a task declares for the harness:
// its search engine and any MCP servers it runs inside the sandbox.
func taskServicePorts(services core.TaskServices) ([]int, error) {
	if services.SearchPort < 0 || services.SearchPort > 65535 {
		return nil, errors.New("invalid task search service port")
	}
	var ports []int
	if services.SearchPort != 0 {
		ports = append(ports, services.SearchPort)
	}
	for _, mcp := range services.MCP {
		if mcp.Port <= 0 || mcp.Port > 65535 {
			return nil, fmt.Errorf("invalid port for task MCP service %q", mcp.Name)
		}
		if !slices.Contains(ports, mcp.Port) {
			ports = append(ports, mcp.Port)
		}
	}
	return ports, nil
}

func (e *taskEnvironment) Validate(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.stopped || e.policy == "" {
		return errors.New("task environment is not active")
	}
	if err := e.owner.validate(); err != nil {
		return err
	}
	manager := e.owner.manager
	name, err := manager.run(ctx, nil, "get", "networkpolicy", "-n", manager.options.Namespace, e.policy, "--ignore-not-found", "-o", "name")
	if err != nil {
		return fmt.Errorf("inspect Kubernetes task network policy: %w", err)
	}
	if strings.TrimSpace(string(name)) == "" {
		return errors.New("task network policy is missing")
	}
	return nil
}

// Stop deletes the policy. The sandbox removes its pod first, so the pod is
// never left running without it.
func (e *taskEnvironment) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	if e.service != "" {
		if err := e.owner.manager.deleteObject(ctx, "service", e.service); err != nil {
			return err
		}
		e.service = ""
	}
	if e.policy != "" {
		if err := e.owner.manager.deletePolicy(ctx, e.policy); err != nil {
			return err
		}
	}
	e.stopped = true
	return nil
}

func (manager *Manager) deletePolicy(ctx context.Context, name string) error {
	return manager.deleteObject(ctx, "networkpolicy", name)
}

// deleteObject removes one namespaced object and confirms that it is gone.
func (manager *Manager) deleteObject(ctx context.Context, kind, name string) error {
	namespace := manager.options.Namespace
	if _, err := manager.run(ctx, nil, "delete", kind, "-n", namespace, name, "--ignore-not-found"); err != nil {
		return fmt.Errorf("delete Kubernetes %s: %w", kind, err)
	}
	remaining, err := manager.run(ctx, nil, "get", kind, "-n", namespace, name, "--ignore-not-found", "-o", "name")
	if err != nil {
		return fmt.Errorf("verify Kubernetes %s removal: %w", kind, err)
	}
	if strings.TrimSpace(string(remaining)) != "" {
		return fmt.Errorf("Kubernetes %s %q remains after removal", kind, name)
	}
	return nil
}

// taskServiceManifest gives one sandbox's task services a cluster address.
func (manager *Manager) taskServiceManifest(request deployment.TaskEnvironmentRequest, ports []int) ([]byte, error) {
	servicePorts := make([]any, 0, len(ports))
	for _, port := range ports {
		servicePorts = append(servicePorts, map[string]any{"name": "port-" + strconv.Itoa(port), "protocol": "TCP", "port": port, "targetPort": port})
	}
	return json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{
			"name": request.RuntimeName, "namespace": manager.options.Namespace,
			"labels":      map[string]string{managedByLabel: "aries", componentLabel: "sandbox", runtimeLabel: request.RuntimeName},
			"annotations": map[string]string{"aries.kind": "task-service", "aries.run": request.RunID, "aries.task": request.TaskID},
		},
		"spec": map[string]any{
			"selector": map[string]string{runtimeLabel: request.RuntimeName},
			"ports":    servicePorts,
		},
	})
}

// taskPolicyManifest isolates one sandbox pod. Every sandbox gets a policy;
// AllowNetwork changes what it permits, never whether it exists.
//
//	AllowNetwork=false → no ingress, no egress.
//	AllowNetwork=true  → no ingress; egress to the internet but not to the
//	                     cluster's own pod and service networks.
//
// Ingress is denied in both cases, and that is the load-bearing half: a sandbox
// that accepts no connections cannot be reached by another task whatever that
// task may send. The bridge needs none either, because it reaches the sandbox
// through the API server's exec endpoint.
//
// The one exception is a declared task service, such as a benchmark's search
// engine or MCP server: harness pods may reach those ports and nothing else. Pods carry their
// role but not their task as a label, so this admits every harness in the
// namespace, as Docker's shared run network does.
//
// Naming a direction in policyTypes with no rule block denies it; an empty rule
// list would instead permit everything.
func (manager *Manager) taskPolicyManifest(request deployment.TaskEnvironmentRequest, ports []int) ([]byte, error) {
	spec := map[string]any{
		"podSelector": map[string]any{"matchLabels": map[string]string{runtimeLabel: request.RuntimeName}},
		"policyTypes": []any{"Ingress", "Egress"},
	}
	if request.Environment.AllowNetwork {
		spec["egress"] = egressRules(manager.options.PodCIDR, manager.options.ServiceCIDR)
	}
	if len(ports) != 0 {
		admitted := make([]any, 0, len(ports))
		for _, port := range ports {
			admitted = append(admitted, map[string]any{"protocol": "TCP", "port": port})
		}
		spec["ingress"] = []any{map[string]any{
			"from":  []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]string{managedByLabel: "aries", componentLabel: "harness"}}}},
			"ports": admitted,
		}}
	}
	return json.Marshal(map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{
			"name": request.RuntimeName, "namespace": manager.options.Namespace,
			"labels":      map[string]string{managedByLabel: "aries", componentLabel: "sandbox", runtimeLabel: request.RuntimeName},
			"annotations": map[string]string{"aries.kind": "task-network", "aries.run": request.RunID, "aries.task": request.TaskID},
		},
		"spec": spec,
	})
}

// egressRules lets a network-allowed task reach the internet while keeping it
// off the cluster's own networks.
//
// DNS is allowed by selecting the CoreDNS pods rather than the kube-dns Service
// address. Service addresses are rewritten before egress policy is evaluated,
// so the destination the policy sees is a CoreDNS pod, which the cluster
// exclusion below would otherwise drop.
//
// With no CIDRs configured the exclusion cannot be expressed and egress is left
// open. Ingress is still denied, so tasks stay isolated from one another.
func egressRules(podCIDR, serviceCIDR string) []any {
	dns := map[string]any{
		"to": []any{map[string]any{
			"namespaceSelector": map[string]any{"matchLabels": map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			"podSelector":       map[string]any{"matchLabels": map[string]string{"k8s-app": "kube-dns"}},
		}},
		"ports": []any{map[string]any{"protocol": "UDP", "port": 53}, map[string]any{"protocol": "TCP", "port": 53}},
	}
	block := map[string]any{"cidr": "0.0.0.0/0"}
	var except []any
	for _, cidr := range []string{podCIDR, serviceCIDR} {
		if strings.TrimSpace(cidr) != "" {
			except = append(except, cidr)
		}
	}
	if len(except) != 0 {
		block["except"] = except
	}
	return []any{dns, map[string]any{"to": []any{map[string]any{"ipBlock": block}}}}
}
