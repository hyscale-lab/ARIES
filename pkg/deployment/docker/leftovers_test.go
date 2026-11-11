package docker

import (
	"context"
	"reflect"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type leftoverClient struct {
	dockerClient
	containers       []container.Summary
	networks         []network.Summary
	containerOptions client.ContainerListOptions
	networkOptions   client.NetworkListOptions
}

func (f *leftoverClient) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	f.containerOptions = options
	return client.ContainerListResult{Items: f.containers}, nil
}

func (f *leftoverClient) NetworkList(_ context.Context, options client.NetworkListOptions) (client.NetworkListResult, error) {
	f.networkOptions = options
	return client.NetworkListResult{Items: f.networks}, nil
}

func TestRunLeftoversListOnlyTheRunsContainersAndNetworks(t *testing.T) {
	ours := map[string]string{"aries.managed": "true", "aries.run": "run-1"}
	fake := &leftoverClient{
		containers: []container.Summary{
			{ID: "c2", Names: []string{"/aries-sandbox-b"}, Labels: ours, State: container.StateExited},
			{ID: "c1", Names: []string{"/aries-hermes-a"}, Labels: ours, State: container.StateRunning},
			// A daemon that ignored the filter must not make cleanup touch another run.
			{ID: "c3", Names: []string{"/other"}, Labels: map[string]string{"aries.managed": "true", "aries.run": "run-2"}},
		},
		networks: []network.Summary{
			{Network: network.Network{ID: "n1", Name: "aries-net-1", Labels: ours}},
			{Network: network.Network{ID: "n2", Name: "bridge"}},
		},
	}
	manager := &Manager{client: fake}
	containers, networks, err := manager.RunLeftovers(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []Leftover{{ID: "c1", Name: "aries-hermes-a"}, {ID: "c2", Name: "aries-sandbox-b"}}; !reflect.DeepEqual(containers, want) {
		t.Fatalf("containers=%v", containers)
	}
	if want := []Leftover{{ID: "n1", Name: "aries-net-1"}}; !reflect.DeepEqual(networks, want) {
		t.Fatalf("networks=%v", networks)
	}
	filters := make(client.Filters).Add("label", "aries.managed=true", "aries.run=run-1")
	if !fake.containerOptions.All || !reflect.DeepEqual(fake.containerOptions.Filters, filters) || !reflect.DeepEqual(fake.networkOptions.Filters, filters) {
		t.Fatalf("container options=%+v network options=%+v", fake.containerOptions, fake.networkOptions)
	}
}

func TestRunLeftoversRejectAnInvalidRunID(t *testing.T) {
	manager := &Manager{client: &leftoverClient{}}
	if _, _, err := manager.RunLeftovers(context.Background(), "../run"); err == nil {
		t.Fatal("an invalid run ID was accepted")
	}
}
