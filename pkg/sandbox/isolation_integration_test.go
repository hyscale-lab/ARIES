//go:build integration

package sandbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/client"
)

func TestConcurrentOccurrencesKeepSeparateNetworks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	ensureFixtureImage(t, ctx, api)
	var managers [2]*Manager
	var sandboxes [2]*Sandbox
	var failures [2]error
	for i := range managers {
		managers[i], err = newIntegrationManager(t, Options{OutputDir: t.TempDir(), CleanupTimeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Register after provider cleanup so resources are removed before clients close.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for i, sandbox := range sandboxes {
			if sandbox != nil {
				if err := managers[i].Stop(cleanup, sandbox); err != nil {
					t.Error(err)
				}
			}
		}
	})
	var started sync.WaitGroup
	for i := range managers {
		started.Add(1)
		go func(i int) {
			defer started.Done()
			live, err := managers[i].Start(ctx, core.SandboxRequest{RunID: "concurrent-isolation", TaskID: "duplicate", Environment: core.Environment{Image: fixtureImage, Workdir: "/work", MemoryMB: 32, CPU: 1}})
			failures[i] = err
			if live != nil {
				sandboxes[i] = live.(*Sandbox)
			}
		}(i)
	}
	started.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if sandboxes[0].NetworkName() == sandboxes[1].NetworkName() || sandboxes[0].ContainerID() == sandboxes[1].ContainerID() {
		t.Fatal("duplicate task IDs shared resources")
	}
	var addresses, peers [2]string
	var peerIDs [2]string
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		for i, id := range peerIDs {
			if id != "" {
				if err := managers[i].deployment.Stop(cleanup, id); err != nil {
					t.Error(err)
				}
			}
		}
	})
	for i, sandbox := range sandboxes {
		inspection, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		networks := inspection.Container.NetworkSettings.Networks
		attachment := networks[sandbox.NetworkName()]
		if len(networks) != 1 || attachment == nil {
			t.Fatalf("unexpected attachments: %#v", networks)
		}
		addresses[i] = attachment.IPAddress.String()
		// A peer on each owned network is the positive reachability control.
		peerRequest := deployment.Request{Name: sandbox.ContainerName() + "-peer", Image: fixtureImage, Network: sandbox.NetworkName(), Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.kind": "isolation-peer", "aries.run": "concurrent-isolation", "aries.task": "duplicate"}}
		peerIDs[i], err = managers[i].deployment.Create(ctx, peerRequest)
		if err != nil {
			t.Fatal(err)
		}
		if err := managers[i].deployment.Validate(ctx, peerIDs[i], peerRequest, nil); err != nil {
			t.Fatal(err)
		}
		if err := managers[i].deployment.Start(ctx, peerIDs[i]); err != nil {
			t.Fatal(err)
		}
		peer, err := api.ContainerInspect(ctx, peerIDs[i], client.ContainerInspectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		peers[i] = peer.Container.NetworkSettings.Networks[sandbox.NetworkName()].IPAddress.String()
		result := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/sh", Args: []string{"-c", "printf '%s' \"$1\" > /work/state", "aries", fmt.Sprint(i)}})
		if result.ExitCode != 0 {
			t.Fatalf("write failed: %#v", result)
		}
	}
	for i, sandbox := range sandboxes {
		own := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/ping", Args: []string{"-c", "1", "-w", "2", peers[i]}, Timeout: 5 * time.Second})
		if own.ExitCode != 0 {
			t.Fatalf("own network unreachable: %#v", own)
		}
		other := execForTest(t, ctx, sandbox, core.Command{Path: "/bin/ping", Args: []string{"-c", "1", "-w", "2", addresses[1-i]}, Timeout: 5 * time.Second})
		if other.ExitCode != 1 {
			t.Fatalf("cross-task traffic was not denied: %#v", other)
		}
		assertExec(t, ctx, sandbox, core.Command{Path: "/bin/cat", Args: []string{"/work/state"}}, 0, fmt.Sprint(i), "")
	}
	for i, id := range peerIDs {
		if err := managers[i].deployment.Stop(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := managers[0].Stop(ctx, sandboxes[0]); err != nil {
		t.Fatal(err)
	}
	assertExec(t, ctx, sandboxes[1], core.Command{Path: "/bin/cat", Args: []string{"/work/state"}}, 0, "1", "")
	if err := managers[1].Stop(ctx, sandboxes[1]); err != nil {
		t.Fatal(err)
	}
	for _, sandbox := range sandboxes {
		if _, err := api.ContainerInspect(ctx, sandbox.ContainerID(), client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Fatalf("container absence unconfirmed: %v", err)
		}
		if _, err := api.NetworkInspect(ctx, sandbox.NetworkName(), client.NetworkInspectOptions{}); !errdefs.IsNotFound(err) {
			t.Fatalf("network absence unconfirmed: %v", err)
		}
	}
}
