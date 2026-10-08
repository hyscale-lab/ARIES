package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/client"
)

// NetworkRequest describes a Docker network owned by the run.
type NetworkRequest struct {
	Name     string
	Labels   map[string]string
	Internal bool
}

// RunEnvironment owns one shared attachment; task handles only borrow it.
type RunEnvironment struct {
	mu               sync.Mutex
	manager          *Manager
	runID            string
	request          NetworkRequest
	id               string
	started, stopped bool
}

func (manager *Manager) NewRunEnvironment(runID string) *RunEnvironment {
	return &RunEnvironment{manager: manager, runID: runID}
}
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
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return core.RuntimePlacement{}, err
	}
	e.request = NetworkRequest{Name: "aries-net-" + hex.EncodeToString(nonce[:]), Internal: false, Labels: map[string]string{"aries.managed": "true", "aries.kind": "run-network", "aries.run": e.runID}}
	var err error
	e.id, err = e.manager.CreateNetwork(ctx, e.request)
	if err != nil {
		return core.RuntimePlacement{}, err
	}
	return core.RuntimePlacement{AttachmentID: e.request.Name}, nil
}
func (e *RunEnvironment) NewTaskEnvironment() deployment.TaskEnvironment {
	return &taskEnvironment{owner: e}
}
func (e *RunEnvironment) NetworkID() string { e.mu.Lock(); defer e.mu.Unlock(); return e.id }
func (e *RunEnvironment) NetworkName() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.request.Name
}
func (e *RunEnvironment) validate(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" || e.stopped {
		return errors.New("run environment is not active")
	}
	return e.manager.ValidateNetwork(ctx, e.id, e.request)
}
func (e *RunEnvironment) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	// Recover a lost create response using the random name and exact ownership.
	if e.id == "" && e.request.Name != "" {
		inspection, err := e.manager.client.NetworkInspect(ctx, e.request.Name, client.NetworkInspectOptions{})
		if errdefs.IsNotFound(err) {
			e.stopped = true
			return nil
		}
		if err != nil {
			return err
		}
		id := inspection.Network.ID
		if id == "" {
			return errors.New("run network inspection returned an empty identity")
		}
		if err := validateNetwork(inspection, id, e.request); err != nil {
			return err
		}
		e.id = id
	}
	if e.id != "" {
		inspection, err := e.manager.client.NetworkInspect(ctx, e.id, client.NetworkInspectOptions{})
		if errdefs.IsNotFound(err) {
			e.stopped = true
			return nil
		}
		if err != nil {
			return err
		}
		if err := validateNetwork(inspection, e.id, e.request); err != nil {
			return err
		}
		if err := e.manager.StopNetwork(ctx, e.id); err != nil {
			return err
		}
	}
	e.stopped = true
	return nil
}

type taskEnvironment struct {
	mu               sync.Mutex
	owner            *RunEnvironment
	started, stopped bool
}

func (e *taskEnvironment) Start(ctx context.Context, request deployment.TaskEnvironmentRequest) (core.HarnessConnectivity, error) {
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
	if err := e.owner.validate(ctx); err != nil {
		return core.HarnessConnectivity{}, err
	}
	result := core.HarnessConnectivity{Placement: core.RuntimePlacement{AttachmentID: e.owner.NetworkName()}}
	if port != 0 {
		result.SearchURL = fmt.Sprintf("http://%s:%d", request.RuntimeName, port)
	}
	return result, nil
}
func (e *taskEnvironment) Validate(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.stopped {
		return errors.New("task environment is not active")
	}
	return e.owner.validate(ctx)
}
func (e *taskEnvironment) Stop(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = true
	return nil
}
