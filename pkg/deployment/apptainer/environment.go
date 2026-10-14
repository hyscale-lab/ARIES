package apptainer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// NetworkPolicy names the connectivity of an Apptainer run: one private
// network namespace shared by Runner, the bridge and every instance, with
// internet access and no route to the host's loopback.
const NetworkPolicy = "run-namespace"

// RunEnvironment is the run's network namespace, which RunInNamespace created
// before the run began. There is nothing to allocate or remove: the namespace
// ends with the run's last process. Start confirms the run is inside it.
type RunEnvironment struct {
	mu               sync.Mutex
	runID, network   string
	started, stopped bool
	current          func() (string, error)
}

func (manager *Manager) NewRunEnvironment(runID string) *RunEnvironment {
	return &RunEnvironment{runID: runID, current: currentRunNamespace}
}

func (e *RunEnvironment) Start(context.Context) (core.RuntimePlacement, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return core.RuntimePlacement{}, errors.New("run environment cannot be reused")
	}
	e.started = true
	if e.runID == "" {
		return core.RuntimePlacement{}, errors.New("run environment requires a run ID")
	}
	network, err := e.current()
	if err != nil {
		return core.RuntimePlacement{}, err
	}
	e.network = network
	return core.RuntimePlacement{AttachmentID: e.attachment()}, nil
}

func (e *RunEnvironment) attachment() string { return "run-namespace:" + e.network }

func (e *RunEnvironment) NewTaskEnvironment() deployment.TaskEnvironment {
	return &taskEnvironment{owner: e}
}

func (e *RunEnvironment) NetworkID() string { e.mu.Lock(); defer e.mu.Unlock(); return e.network }

func (e *RunEnvironment) NetworkName() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.network == "" {
		return ""
	}
	return e.attachment()
}

func (e *RunEnvironment) validate() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.network == "" || e.stopped {
		return errors.New("run environment is not active")
	}
	network, err := e.current()
	if err != nil {
		return err
	}
	if network != e.network {
		return errors.New("run left its network namespace")
	}
	return nil
}

// Stop releases the environment. The namespace itself outlives it until the
// run's processes exit, and RunInNamespace's caller then removes nothing:
// a network namespace without processes is gone.
func (e *RunEnvironment) Stop(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	return nil
}

type taskEnvironment struct {
	mu               sync.Mutex
	owner            *RunEnvironment
	started, stopped bool
}

// Start resolves the task's services on the shared loopback. Every runtime of
// the run shares it, so two tasks' services could not both use one port; runs
// are limited to one task at a time.
func (e *taskEnvironment) Start(_ context.Context, request deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return core.HarnessConnectivity{}, errors.New("task environment cannot be reused")
	}
	e.started = true
	if request.RunID != e.owner.runID || request.RuntimeName == "" {
		return core.HarnessConnectivity{}, errors.New("task environment requires its run and a unique runtime name")
	}
	port := request.Environment.Services.SearchPort
	if port < 0 || port > 65535 {
		return core.HarnessConnectivity{}, errors.New("invalid task search service port")
	}
	for _, service := range request.Environment.Services.MCP {
		if service.Port <= 0 || service.Port > 65535 {
			return core.HarnessConnectivity{}, fmt.Errorf("invalid port for task MCP service %q", service.Name)
		}
	}
	if err := e.owner.validate(); err != nil {
		return core.HarnessConnectivity{}, err
	}
	result := core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: e.owner.NetworkName()}}
	if port != 0 {
		result.SearchURL = fmt.Sprintf("http://%s:%d", loopbackHost, port)
	}
	for _, service := range request.Environment.Services.MCP {
		result.MCPServers = append(result.MCPServers, core.MCPServerConfig{
			Name: service.Name, URL: fmt.Sprintf("http://%s:%d%s", loopbackHost, service.Port, service.Path),
			Transport: service.Transport, TimeoutSeconds: service.TimeoutSeconds,
		})
	}
	return result, nil
}

func (e *taskEnvironment) Validate(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.stopped {
		return errors.New("task environment is not active")
	}
	return e.owner.validate()
}

func (e *taskEnvironment) Stop(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	return nil
}
