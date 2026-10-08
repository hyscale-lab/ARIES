package docker

import (
	"context"
	"net/netip"
	"testing"

	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

func TestDeclaredMountsAreIndependentOfComponent(t *testing.T) {
	for _, component := range []string{"bridge", "harness", "sandbox", "custom", ""} {
		t.Run(component, func(t *testing.T) {
			fake := newFakeDocker()
			manager := &Manager{client: fake}
			request := deploymentRequest()
			request.Labels["aries.component"] = component
			request.Mounts = []deployment.Mount{{Source: "/run/service.sock", Target: "/private/backend.sock"}, {Source: "/srv/inputs", Target: "/data", ReadOnly: true}}
			request.ImageVolumes = []string{"/cache"}
			request.InternalPort = 2222
			id, err := manager.Create(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			mounts := fake.created.HostConfig.Mounts
			if len(mounts) != len(request.Mounts) {
				t.Fatalf("mounts=%v", mounts)
			}
			for index, declared := range request.Mounts {
				actual := mounts[index]
				if actual.Type != mount.TypeBind || actual.Source != declared.Source || actual.Target != declared.Target || actual.ReadOnly != declared.ReadOnly {
					t.Fatalf("mount %d changed: %+v", index, actual)
				}
				fake.container.Mounts = append(fake.container.Mounts, container.MountPoint{Type: mount.TypeBind, Source: actual.Source, Destination: actual.Target, RW: !actual.ReadOnly})
			}
			if _, ok := fake.created.HostConfig.PortBindings[network.MustParsePort("2222/tcp")]; ok {
				t.Fatal("internal port published to runner host")
			}
			if _, ok := fake.created.Config.ExposedPorts[network.MustParsePort("2222/tcp")]; !ok {
				t.Fatal("internal port not exposed on task attachment")
			}
			// Inspection need not preserve the request's ordering.
			fake.container.HostConfig.Mounts[0], fake.container.HostConfig.Mounts[1] = mounts[1], mounts[0]
			fake.container.Mounts = append(fake.container.Mounts, container.MountPoint{Type: mount.TypeVolume, Name: "private-image-volume", Destination: "/cache", RW: true})
			if err = manager.Validate(context.Background(), id, request, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDefaultRuntimeGrantsNoHostMounts(t *testing.T) {
	fake := newFakeDocker()
	manager := &Manager{client: fake}
	request := deploymentRequest()
	id, err := manager.Create(context.Background(), request)
	if err != nil || len(fake.created.HostConfig.Mounts) != 0 {
		t.Fatalf("default mounts=%v err=%v", fake.created.HostConfig.Mounts, err)
	}
	fake.container.Mounts = []container.MountPoint{{Type: mount.TypeBind, Source: "/srv/private", Destination: "/private", RW: true}}
	if err := manager.Validate(context.Background(), id, request, nil); err == nil {
		t.Fatal("undeclared host access accepted")
	}
}

func TestMountValidationRejectsChangedAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*container.InspectResponse)
	}{
		{"legacy bind", func(c *container.InspectResponse) { c.HostConfig.Binds = []string{"/host:/runtime"} }},
		{"missing requested mount", func(c *container.InspectResponse) { c.HostConfig.Mounts = nil }},
		{"changed requested source", func(c *container.InspectResponse) { c.HostConfig.Mounts[0].Source = "/other" }},
		{"changed requested target", func(c *container.InspectResponse) { c.HostConfig.Mounts[0].Target = "/other" }},
		{"changed requested mode", func(c *container.InspectResponse) { c.HostConfig.Mounts[0].ReadOnly = false }},
		{"changed requested type", func(c *container.InspectResponse) { c.HostConfig.Mounts[0].Type = mount.TypeVolume }},
		{"missing actual mount", func(c *container.InspectResponse) { c.Mounts = nil }},
		{"changed actual source", func(c *container.InspectResponse) { c.Mounts[0].Source = "/other" }},
		{"changed actual target", func(c *container.InspectResponse) { c.Mounts[0].Destination = "/other" }},
		{"changed actual mode", func(c *container.InspectResponse) { c.Mounts[0].RW = true }},
		{"image volume shadows declared bind", func(c *container.InspectResponse) { c.Mounts[0].Type, c.Mounts[0].Name = mount.TypeVolume, "volume" }},
		{"extra actual mount", func(c *container.InspectResponse) {
			c.Mounts = append(c.Mounts, container.MountPoint{Type: mount.TypeBind, Source: "/other", Destination: "/other"})
		}},
		{"duplicate actual mount", func(c *container.InspectResponse) { c.Mounts = append(c.Mounts, c.Mounts[0]) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := &Manager{client: fake}
			request := deploymentRequest()
			request.Mounts = []deployment.Mount{{Source: "/srv/input", Target: "/data", ReadOnly: true}}
			request.AllowImageVolumes = true
			id, err := manager.Create(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			fake.container.Mounts = []container.MountPoint{{Type: mount.TypeBind, Source: "/srv/input", Destination: "/data", RW: false}}
			test.mutate(&fake.container)
			if err := manager.Validate(context.Background(), id, request, nil); err == nil {
				t.Fatal("changed mount authority accepted")
			}
		})
	}
}

func TestCreateRejectsInvalidMountsBeforeAllocation(t *testing.T) {
	for _, mounts := range [][]deployment.Mount{
		{{Source: "relative", Target: "/data"}},
		{{Source: "/host", Target: "relative"}},
		{{Source: "/host/../other", Target: "/data"}},
		{{Source: "/host", Target: "/data/../other"}},
		{{Source: "/host\x00", Target: "/data"}},
		{{Source: "/host", Target: "/data"}, {Source: "/other", Target: "/data"}},
	} {
		request := deploymentRequest()
		request.Mounts = mounts
		if _, err := (&Manager{}).Create(context.Background(), request); err == nil {
			t.Fatalf("invalid mounts accepted: %+v", mounts)
		}
	}
}
func TestTaskAddressUsesTaskAttachment(t *testing.T) {
	fake := newFakeDocker()
	m := &Manager{client: fake}
	id, err := m.Create(context.Background(), deploymentRequest())
	if err != nil {
		t.Fatal(err)
	}
	fake.container.State = &container.State{Running: true}
	fake.container.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"task-network": {IPAddress: netip.MustParseAddr("172.22.0.4")}}}
	address, err := m.TaskAddress(context.Background(), id, 2222)
	if err != nil || address != "172.22.0.4:2222" {
		t.Fatalf("%q %v", address, err)
	}
	fake.container.NetworkSettings.Networks["other"] = &network.EndpointSettings{}
	if _, err = m.TaskAddress(context.Background(), id, 2222); err == nil {
		t.Fatal("ambiguous task attachment accepted")
	}
}

func TestServiceAddressRequiresPublishedEndpoint(t *testing.T) {
	fake := newFakeDocker()
	m := &Manager{client: fake}
	request := deploymentRequest()
	request.Labels["aries.component"] = "custom"
	id, err := m.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	fake.container.State = &container.State{Running: true}
	fake.container.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"task-network": {IPAddress: netip.MustParseAddr("172.22.0.4")}}}
	got, err := m.Address(context.Background(), id, request.ServicePort)
	if err == nil {
		t.Fatalf("unpublished service was inferred from an interface: %q", got)
	}
	delete(fake.container.Config.Labels, "aries.component")
	if got, err = m.Address(context.Background(), id, request.ServicePort); err == nil {
		t.Fatalf("unpublished service without labels was inferred: %q", got)
	}
	delete(fake.container.HostConfig.PortBindings, network.MustParsePort("18789/tcp"))
	if _, err = m.Address(context.Background(), id, request.ServicePort); err == nil {
		t.Fatal("unrequested host service publication accepted")
	}
}

type addressInspectionClient struct {
	dockerClient
	info container.InspectResponse
}

func (c addressInspectionClient) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: c.info}, nil
}

func TestInternalNetworkServiceAddressPreservesIdentityAndPublicationChecks(t *testing.T) {
	servicePort := network.MustParsePort("18789/tcp")
	for _, test := range []struct {
		name   string
		mutate func(*container.InspectResponse)
	}{
		{"foreign identity", func(c *container.InspectResponse) { c.ID = "other-runtime" }},
		{"stopped runtime", func(c *container.InspectResponse) { c.State.Running = false }},
		{"missing state", func(c *container.InspectResponse) { c.State = nil }},
		{"missing host config", func(c *container.InspectResponse) { c.HostConfig = nil }},
		{"unrequested service", func(c *container.InspectResponse) { c.HostConfig.PortBindings = nil }},
		{"public publication", func(c *container.InspectResponse) {
			c.HostConfig.PortBindings[servicePort] = []network.PortBinding{{HostIP: netip.MustParseAddr("0.0.0.0")}}
		}},
		{"ambiguous publication", func(c *container.InspectResponse) {
			c.HostConfig.PortBindings[servicePort] = append(c.HostConfig.PortBindings[servicePort], c.HostConfig.PortBindings[servicePort][0])
		}},
		{"ambiguous attachment", func(c *container.InspectResponse) {
			c.NetworkSettings.Networks["other"] = &network.EndpointSettings{IPAddress: netip.MustParseAddr("172.23.0.4")}
		}},
		{"missing attachment", func(c *container.InspectResponse) { c.NetworkSettings.Networks = nil }},
		{"missing endpoint", func(c *container.InspectResponse) { c.NetworkSettings.Networks["task-network"] = nil }},
		{"missing address", func(c *container.InspectResponse) {
			c.NetworkSettings.Networks["task-network"].IPAddress = netip.Addr{}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := &Manager{client: fake}
			request := deploymentRequest()
			id, err := manager.Create(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			fake.container.State = &container.State{Running: true}
			fake.container.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{"task-network": {IPAddress: netip.MustParseAddr("172.22.0.4")}}}
			test.mutate(&fake.container)
			manager.client = addressInspectionClient{dockerClient: fake, info: fake.container}
			if address, err := manager.Address(context.Background(), id, request.ServicePort); err == nil {
				t.Fatalf("unconfirmed private address accepted: %q", address)
			}
		})
	}
}
