package remote

import (
	"context"
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

type dockerNotFound struct{}

func (dockerNotFound) Error() string { return "No such container" }
func (dockerNotFound) NotFound()     {}

// fakeDocker models one bridge container: whether it exists, runs, when it
// started, and which networks it is attached to with what address.
type fakeDocker struct {
	listed       []container.Summary
	exists       bool
	running      bool
	startedAt    string
	networks     map[string]string
	connects     []string
	disconnects  []string
	nextAddress  string
	connectError error
}

func (f *fakeDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: f.listed}, nil
}

func (f *fakeDocker) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if !f.exists {
		return client.ContainerInspectResult{}, dockerNotFound{}
	}
	endpoints := map[string]*network.EndpointSettings{}
	for name, address := range f.networks {
		endpoints[name] = &network.EndpointSettings{IPAddress: netip.MustParseAddr(address)}
	}
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID:              "b1d93e0f5a8c7d6e5f4a3b2c1d0e9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e",
		State:           &container.State{Running: f.running, StartedAt: f.startedAt},
		NetworkSettings: &container.NetworkSettings{Networks: endpoints},
	}}, nil
}

func (*fakeDocker) ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error) {
	panic("exec is covered by the live Docker run")
}

func (*fakeDocker) ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error) {
	panic("exec is covered by the live Docker run")
}

func (*fakeDocker) ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error) {
	panic("exec is covered by the live Docker run")
}

func (f *fakeDocker) NetworkConnect(_ context.Context, name string, _ client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if f.connectError != nil {
		return client.NetworkConnectResult{}, f.connectError
	}
	f.connects = append(f.connects, name)
	f.networks[name] = f.nextAddress
	return client.NetworkConnectResult{}, nil
}

func (f *fakeDocker) NetworkDisconnect(_ context.Context, name string, _ client.NetworkDisconnectOptions) (client.NetworkDisconnectResult, error) {
	f.disconnects = append(f.disconnects, name)
	delete(f.networks, name)
	return client.NetworkDisconnectResult{}, nil
}

func liveBridge() *fakeDocker {
	return &fakeDocker{
		listed: []container.Summary{{ID: "b1d93e0f5a8c"}}, exists: true, running: true,
		startedAt: "2026-10-03T03:00:00Z", networks: map[string]string{"aries_default": "172.18.0.2"},
		nextAddress: "172.30.0.3",
	}
}

func TestDockerLocateNeedsExactlyOneRunningBridge(t *testing.T) {
	ctx := context.Background()
	fake := liveBridge()
	target, err := (&DockerTransport{api: fake}).Locate(ctx)
	if err != nil || target.Name != "b1d93e0f5a8c" || target.Identity != fake.startedAt {
		t.Fatalf("target = %+v, %v", target, err)
	}
	for name, mutate := range map[string]func(*fakeDocker){
		"none":        func(f *fakeDocker) { f.listed = nil },
		"two":         func(f *fakeDocker) { f.listed = append(f.listed, container.Summary{ID: "other"}) },
		"not running": func(f *fakeDocker) { f.running = false },
	} {
		t.Run(name, func(t *testing.T) {
			fake := liveBridge()
			mutate(fake)
			if _, err := (&DockerTransport{api: fake}).Locate(ctx); err == nil {
				t.Fatal("located a bridge that cannot be granted on")
			}
		})
	}
}

// Gone is half of the revocation proof, so each way a container can stop
// being the process that held the grants must count, and a live one must not.
func TestDockerGoneProvesOnlyAbsenceOrReplacement(t *testing.T) {
	ctx := context.Background()
	target := Target{Name: "b1d93e0f5a8c", Identity: "2026-10-03T03:00:00Z"}
	for name, test := range map[string]struct {
		mutate func(*fakeDocker)
		gone   bool
	}{
		"still running": {func(*fakeDocker) {}, false},
		"removed":       {func(f *fakeDocker) { f.exists = false }, true},
		"stopped":       {func(f *fakeDocker) { f.running = false }, true},
		"restarted":     {func(f *fakeDocker) { f.startedAt = "2026-10-03T03:05:00Z" }, true},
	} {
		t.Run(name, func(t *testing.T) {
			fake := liveBridge()
			test.mutate(fake)
			gone, err := (&DockerTransport{api: fake}).Gone(ctx, target)
			if err != nil || gone != test.gone {
				t.Fatalf("gone = %v, %v; want %v", gone, err, test.gone)
			}
		})
	}
}

// The grant listens on the bridge's address on that task's network, so it is
// reachable from that task and no other; Leave undoes it once.
func TestDockerJoinAndLeaveTheTaskNetwork(t *testing.T) {
	ctx := context.Background()
	fake := liveBridge()
	transport := &DockerTransport{api: fake}
	target := Target{Name: "b1d93e0f5a8c"}
	sandbox := SandboxRef{Backend: BackendDocker, Network: "aries-net-a1b2c3"}

	address, err := transport.Join(ctx, target, sandbox)
	if err != nil || address != "172.30.0.3" {
		t.Fatalf("join = %q, %v; want the address on the task network, not the default one", address, err)
	}
	if again, err := transport.Join(ctx, target, sandbox); err != nil || again != address || len(fake.connects) != 1 {
		t.Fatalf("a retried join must reuse the attachment: %q %v after %d connects", again, err, len(fake.connects))
	}
	if err := transport.Leave(ctx, target, sandbox); err != nil || len(fake.disconnects) != 1 {
		t.Fatalf("leave = %v after %d disconnects", err, len(fake.disconnects))
	}
	if err := transport.Leave(ctx, target, sandbox); err != nil || len(fake.disconnects) != 1 {
		t.Fatalf("leaving twice must be a no-op: %v after %d disconnects", err, len(fake.disconnects))
	}
	fake.exists = false
	if err := transport.Leave(ctx, target, sandbox); err != nil {
		t.Fatalf("a bridge that is gone has nothing to leave: %v", err)
	}
}

func TestDockerDescribeRefusesNonDockerSandboxes(t *testing.T) {
	transport := &DockerTransport{api: liveBridge()}
	if _, err := transport.Describe(runnerSandbox{namespace: testNamespace}); err == nil {
		t.Error("described a Kubernetes sandbox as a Docker one")
	}
	if _, err := transport.Describe(&podSandbox{}); err == nil {
		t.Error("described a sandbox whose container ID is namespace/pod")
	}
}
