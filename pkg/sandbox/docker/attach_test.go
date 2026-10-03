package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const attachID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// attachFake answers ContainerInspect with a configurable task container and
// takes everything else from fakeClient.
type attachFake struct {
	*fakeClient
	inspect container.InspectResponse
}

func (f *attachFake) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{Container: f.inspect}, nil
}

func newAttachFake(mutate func(*container.InspectResponse)) *attachFake {
	labels := map[string]string{
		"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox",
		"aries.run": "run-1", "aries.task": "task-1",
	}
	inspect := container.InspectResponse{
		ID: attachID, Name: "/aries-task-a1b2c3",
		State:  &container.State{Running: true},
		Config: &container.Config{Labels: labels, WorkingDir: "/app"},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"aries-net-a1b2c3": {},
		}},
	}
	if mutate != nil {
		mutate(&inspect)
	}
	return &attachFake{
		fakeClient: &fakeClient{networkName: "aries-net-a1b2c3", networkExists: true, networkOptions: client.NetworkCreateOptions{
			Labels: map[string]string{"aries.managed": "true", "aries.run": "run-1", "aries.task": "task-1"},
		}},
		inspect: inspect,
	}
}

func attachOptions() AttachOptions {
	return AttachOptions{ContainerID: attachID, Network: "aries-net-a1b2c3", Workdir: "/app", RunID: "run-1", TaskID: "task-1"}
}

func TestAttachAcceptsTheNamedTaskContainer(t *testing.T) {
	attacher := &Attacher{client: newAttachFake(nil)}
	sandbox, err := attacher.Attach(context.Background(), attachOptions())
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.ContainerID() != attachID || sandbox.NetworkName() != "aries-net-a1b2c3" || sandbox.Workdir() != "/app" {
		t.Fatalf("handle names the wrong container: %+v", sandbox)
	}
	manager := &Manager{client: newAttachFake(nil)}
	if err := manager.Stop(context.Background(), sandbox); err == nil {
		t.Fatal("a manager stopped a sandbox it did not start")
	}
}

// Each of these is a container the bridge must never exec into. The request
// names the container, so these checks are what stop a wrong request from
// reaching a harness container or another task's sandbox.
func TestAttachRefusesOtherContainers(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*container.InspectResponse)
		options func(*AttachOptions)
	}{
		"harness container":      {mutate: func(c *container.InspectResponse) { c.Config.Labels["aries.component"] = "harness" }},
		"not a task container":   {mutate: func(c *container.InspectResponse) { c.Config.Labels["aries.kind"] = "task-network" }},
		"another run":            {mutate: func(c *container.InspectResponse) { c.Config.Labels["aries.run"] = "run-2" }},
		"not running":            {mutate: func(c *container.InspectResponse) { c.State.Running = false }},
		"other workdir":          {mutate: func(c *container.InspectResponse) { c.Config.WorkingDir = "/" }},
		"not on its network":     {mutate: func(c *container.InspectResponse) { c.NetworkSettings.Networks = nil }},
		"not ARIES-named":        {mutate: func(c *container.InspectResponse) { c.Name = "/aries-hermes-a1b2c3" }},
		"another task network":   {options: func(o *AttachOptions) { o.Network = "aries-net-ffffff" }},
		"short container ID":     {options: func(o *AttachOptions) { o.ContainerID = attachID[:12] }},
		"bad run identity":       {options: func(o *AttachOptions) { o.RunID = "run/1" }},
		"missing network":        {options: func(o *AttachOptions) { o.Network = "" }},
		"workdir does not match": {options: func(o *AttachOptions) { o.Workdir = "/tmp" }},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			options := attachOptions()
			if test.options != nil {
				test.options(&options)
			}
			attacher := &Attacher{client: newAttachFake(test.mutate)}
			if _, err := attacher.Attach(context.Background(), options); err == nil {
				t.Fatal("attached to a container it must refuse")
			} else if strings.Contains(err.Error(), "panic") {
				t.Fatal(err)
			}
		})
	}
}
