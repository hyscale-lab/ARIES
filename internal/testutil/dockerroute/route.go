//go:build integration

// Package dockerroute connects test-host fixtures to an owned task network.
// Production bridges run on the task network and do not need a host route.
package dockerroute

import (
	"context"
	"errors"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/client"
)

func Listen(ctx context.Context, network string) (core.BridgeListen, error) {
	if network == "" {
		return core.BridgeListen{}, errors.New("test task network is not active")
	}
	api, err := client.New(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return core.BridgeListen{}, err
	}
	defer api.Close()
	result, err := api.NetworkInspect(ctx, network, client.NetworkInspectOptions{})
	if err != nil {
		return core.BridgeListen{}, err
	}
	n := result.Network
	if n.Name != network || n.Driver != "bridge" || n.Labels["aries.managed"] != "true" || n.Labels["aries.kind"] != "task-network" {
		return core.BridgeListen{}, errors.New("test task network ownership mismatch")
	}
	for _, config := range n.IPAM.Config {
		if config.Gateway.Is4() {
			host := config.Gateway.String()
			return core.BridgeListen{BindHost: host, AdvertiseHost: host}, nil
		}
	}
	return core.BridgeListen{}, errors.New("test network has no IPv4 gateway")
}

// Capture records the exact attachment created for a test occurrence.
func Capture(inner deployment.TaskEnvironment, network *string) deployment.TaskEnvironment {
	return &environment{TaskEnvironment: inner, network: network}
}

type environment struct {
	deployment.TaskEnvironment
	network *string
}

func (e *environment) Start(ctx context.Context, request core.SandboxRequest) (core.HarnessConnectivity, error) {
	connectivity, err := e.TaskEnvironment.Start(ctx, request)
	if err == nil {
		*e.network = connectivity.Placement.DockerNetwork
	}
	return connectivity, err
}
