package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/client"
)

type checkpointClient struct {
	dockerClient
	created []client.CheckpointCreateOptions
	started []client.ContainerStartOptions
	removed []client.CheckpointRemoveOptions
	err     error
}

func (fake *checkpointClient) CheckpointCreate(_ context.Context, id string, options client.CheckpointCreateOptions) (client.CheckpointCreateResult, error) {
	if id != "task-id" {
		return client.CheckpointCreateResult{}, errors.New("unexpected container")
	}
	fake.created = append(fake.created, options)
	return client.CheckpointCreateResult{}, fake.err
}

func (fake *checkpointClient) ContainerStart(_ context.Context, id string, options client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if id != "task-id" {
		return client.ContainerStartResult{}, errors.New("unexpected container")
	}
	fake.started = append(fake.started, options)
	return client.ContainerStartResult{}, fake.err
}

func (fake *checkpointClient) CheckpointRemove(_ context.Context, id string, options client.CheckpointRemoveOptions) (client.CheckpointRemoveResult, error) {
	if id != "task-id" {
		return client.CheckpointRemoveResult{}, errors.New("unexpected container")
	}
	fake.removed = append(fake.removed, options)
	return client.CheckpointRemoveResult{}, fake.err
}

func TestCheckpointStopsAndRestoreStartsFromNamedCheckpoint(t *testing.T) {
	fake := &checkpointClient{}
	manager := &Manager{client: fake}
	ctx := context.Background()
	if err := manager.Checkpoint(ctx, "task-id", "aries-1"); err != nil {
		t.Fatalf("Checkpoint() error = %v", err)
	}
	if err := manager.Restore(ctx, "task-id", "aries-1"); err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if err := manager.DeleteCheckpoint(ctx, "task-id", "aries-1"); err != nil {
		t.Fatalf("DeleteCheckpoint() error = %v", err)
	}
	if len(fake.created) != 1 || fake.created[0] != (client.CheckpointCreateOptions{CheckpointID: "aries-1", Exit: true}) {
		t.Fatalf("checkpoint create = %#v, want one exiting checkpoint in the default directory", fake.created)
	}
	if len(fake.started) != 1 || fake.started[0] != (client.ContainerStartOptions{CheckpointID: "aries-1"}) {
		t.Fatalf("container start = %#v, want restore from aries-1", fake.started)
	}
	if len(fake.removed) != 1 || fake.removed[0] != (client.CheckpointRemoveOptions{CheckpointID: "aries-1"}) {
		t.Fatalf("checkpoint remove = %#v, want aries-1", fake.removed)
	}
}

func TestCheckpointOperationsRequireIDAndReturnDaemonErrors(t *testing.T) {
	daemonErr := errors.New("checkpoint is only supported in experimental mode")
	fake := &checkpointClient{err: daemonErr}
	manager := &Manager{client: fake}
	ctx := context.Background()
	for name, operation := range map[string]func(string) error{
		"checkpoint": func(id string) error { return manager.Checkpoint(ctx, "task-id", id) },
		"restore":    func(id string) error { return manager.Restore(ctx, "task-id", id) },
		"delete":     func(id string) error { return manager.DeleteCheckpoint(ctx, "task-id", id) },
	} {
		if err := operation(""); err == nil {
			t.Fatalf("%s with empty ID error = nil", name)
		}
		if err := operation("aries-1"); !errors.Is(err, daemonErr) {
			t.Fatalf("%s error = %v, want daemon error", name, err)
		}
	}
	if len(fake.created)+len(fake.started)+len(fake.removed) != 3 {
		t.Fatal("an empty checkpoint ID reached the daemon")
	}
}
