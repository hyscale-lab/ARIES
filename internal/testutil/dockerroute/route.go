//go:build integration

// Package dockerroute connects test-host fixtures to an owned task network.
// Production bridges run on the task network and do not need a host route.
package dockerroute

import (
	"context"
	"errors"

	"github.com/hyscale-lab/aries/pkg/core"
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
	if n.Name != network || n.Driver != "bridge" || n.Labels["aries.managed"] != "true" || n.Labels["aries.kind"] != "run-network" {
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
