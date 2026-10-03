package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/client"
)

// NetworkRequest describes the Docker-specific task network.
type NetworkRequest struct {
	Name     string
	Labels   map[string]string
	Internal bool
}

type taskEnvironment struct {
	mu      sync.Mutex
	manager *Manager
	request NetworkRequest
	id      string
	started bool
	stopped bool
}

// NewTaskEnvironment returns a fresh owner for one task occurrence.
func (manager *Manager) NewTaskEnvironment() deployment.TaskEnvironment {
	return &taskEnvironment{manager: manager}
}

func (e *taskEnvironment) Start(ctx context.Context, request core.SandboxRequest) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopped {
		return "", errors.New("task environment cannot be reused")
	}
	e.started = true
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	e.request = NetworkRequest{Name: "aries-net-" + hex.EncodeToString(nonce[:]), Internal: !request.Environment.AllowNetwork,
		Labels: map[string]string{"aries.managed": "true", "aries.kind": "task-network", "aries.run": request.RunID, "aries.task": request.TaskID}}
	var err error
	e.id, err = e.manager.CreateNetwork(ctx, e.request)
	return e.request.Name, err
}
func (e *taskEnvironment) Validate(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" || e.stopped {
		return errors.New("task environment is not active")
	}
	return e.manager.ValidateNetwork(ctx, e.id, e.request)
}
func (e *taskEnvironment) BridgeListen(ctx context.Context) (core.BridgeListen, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.id == "" || e.stopped {
		return core.BridgeListen{}, errors.New("task environment is not active")
	}
	gateway, err := e.manager.NetworkGateway(ctx, e.id, e.request)
	if err != nil {
		return core.BridgeListen{}, err
	}
	return core.BridgeListen{BindHost: gateway, AdvertiseHost: gateway}, nil
}
func (e *taskEnvironment) Stop(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return nil
	}
	// A lost create response may omit the immutable ID after allocation.
	// Recover only through this occurrence's random name and verified ownership.
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
			return errors.New("task network inspection returned an empty identity")
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
