package docker

import (
	"context"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
)

func TestTrustedSocketRestrictedToBridgeAndValidated(t *testing.T) {
	fake := newFakeDocker()
	m := &Manager{client: fake}
	r := deploymentRequest()
	r.TrustedDockerSocket = "/run/docker.sock"
	if _, err := m.Create(context.Background(), r); err == nil || fake.createCalls != 0 {
		t.Fatal("harness granted Docker authority")
	}
	r.Labels["aries.component"] = "bridge"
	r.HarnessPort = 2222
	id, err := m.Create(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	mounts := fake.created.HostConfig.Mounts
	if len(mounts) != 1 || mounts[0].Source != "/run/docker.sock" || mounts[0].Target != "/var/run/docker.sock" {
		t.Fatalf("mounts=%v", mounts)
	}
	if _, ok := fake.created.HostConfig.PortBindings[network.MustParsePort("2222/tcp")]; ok {
		t.Fatal("harness port published to runner host")
	}
	fake.container.Mounts = []container.MountPoint{{Type: mount.TypeBind, Source: "/run/docker.sock", Destination: "/var/run/docker.sock", RW: true}}
	if err = m.Validate(context.Background(), id, r, nil); err != nil {
		t.Fatal(err)
	}
	fake.container.Mounts[0].Source = "/other.sock"
	if err = m.Validate(context.Background(), id, r, nil); err == nil {
		t.Fatal("unexpected socket accepted")
	}
}
func TestHarnessAddressUsesTaskAttachment(t *testing.T) {
	fake := newFakeDocker()
	m := &Manager{client: fake}
	id, err := m.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	fake.container.State = &container.State{Running: true}
	fake.container.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"task-network": {IPAddress: netip.MustParseAddr("172.22.0.4")}}}
	address, err := m.HarnessAddress(context.Background(), id, 2222)
	if err != nil || address != "172.22.0.4:2222" {
		t.Fatalf("%q %v", address, err)
	}
	fake.container.NetworkSettings.Networks["other"] = &network.EndpointSettings{}
	if _, err = m.HarnessAddress(context.Background(), id, 2222); err == nil {
		t.Fatal("ambiguous task attachment accepted")
	}
}

func TestBridgeControlAddressOnInternalTaskNetwork(t *testing.T) {
	fake := newFakeDocker()
	m := &Manager{client: fake}
	request := deploymentRequest()
	request.Labels["aries.component"] = "bridge"
	id, err := m.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fake.container.State = &container.State{Running: true}
	fake.container.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"task-network": {IPAddress: netip.MustParseAddr("172.22.0.4")}}}
	got, err := m.Address(context.Background(), id, request.ServicePort)
	if err != nil || got != "172.22.0.4:18789" {
		t.Fatalf("private bridge control address=%q err=%v", got, err)
	}
	fake.container.Config.Labels["aries.component"] = "harness"
	if _, err = m.Address(context.Background(), id, request.ServicePort); err == nil {
		t.Fatal("non-bridge runtime bypassed host-only address contract")
	}
}
