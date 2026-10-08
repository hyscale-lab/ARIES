package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/client"
)

type createRecoveryClient struct {
	*fakeDocker
	createErr      error
	lookupErr      error
	mutate         func()
	lookupCanceled bool
	lookupBounded  bool
}

func (f *createRecoveryClient) ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	_, err := f.fakeDocker.ContainerCreate(ctx, options)
	if f.mutate != nil {
		f.mutate()
	}
	return client.ContainerCreateResult{}, errors.Join(err, f.createErr)
}
func (f *createRecoveryClient) ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if id == f.created.Name {
		f.lookupCanceled = ctx.Err() != nil
		_, f.lookupBounded = ctx.Deadline()
		if f.lookupErr != nil {
			return client.ContainerInspectResult{}, f.lookupErr
		}
		return client.ContainerInspectResult{Container: f.container}, nil
	}
	return f.fakeDocker.ContainerInspect(ctx, id, options)
}
func TestCreateRecoversMissingIdentityForOwnedCleanup(t *testing.T) {
	for _, failed := range []bool{false, true} {
		f := &createRecoveryClient{fakeDocker: newFakeDocker()}
		if failed {
			f.createErr = errors.New("lost create response")
		}
		manager := &Manager{client: f}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		id, err := manager.Create(ctx, deploymentRequest())
		if id != "openclaw-id" || err == nil || failed && !errors.Is(err, f.createErr) || errors.Is(err, deployment.ErrAllocationUnconfirmed) {
			t.Fatal(id, err)
		}
		if f.lookupCanceled || !f.lookupBounded {
			t.Fatal("recovery needs fresh bounded context")
		}
		if err := manager.Stop(context.Background(), id); err != nil || !f.removed {
			t.Fatal("recovered runtime not removed", err)
		}
	}
}
func TestCreateRecoveryRefusesForeignOrUnconfirmedAllocation(t *testing.T) {
	for _, kind := range []string{"name", "labels", "empty ID", "missing config", "absent", "unavailable", "no requested name", "no requested labels"} {
		t.Run(kind, func(t *testing.T) {
			f := &createRecoveryClient{fakeDocker: newFakeDocker(), createErr: errors.New("lost response")}
			request := deploymentRequest()
			switch kind {
			case "name":
				f.mutate = func() { f.container.Name = "/foreign" }
			case "labels":
				f.mutate = func() { f.container.Config.Labels = map[string]string{"aries.task": "foreign"} }
			case "empty ID":
				f.mutate = func() { f.container.ID = "" }
			case "missing config":
				f.mutate = func() { f.container.Config = nil }
			case "absent":
				f.lookupErr = errdefs.ErrNotFound
			case "unavailable":
				f.lookupErr = errors.New("daemon unavailable")
			case "no requested name":
				request.Name = ""
			case "no requested labels":
				request.Labels = nil
			}
			id, err := (&Manager{client: f}).Create(context.Background(), request)
			if id != "" || !errors.Is(err, f.createErr) || f.removeCalls != 0 {
				t.Fatal("returned unsafe cleanup handle", id, err)
			}
			if uncertain := errors.Is(err, deployment.ErrAllocationUnconfirmed); uncertain != (kind != "absent") {
				t.Fatalf("allocation uncertainty = %v, error: %v", uncertain, err)
			}
			if kind == "unavailable" && !errors.Is(err, f.lookupErr) {
				t.Fatal("lost cleanup uncertainty", err)
			}
		})
	}
}

func TestCreateRejectedBeforeAllocationHasNoCleanupUncertainty(t *testing.T) {
	f := &createRecoveryClient{fakeDocker: newFakeDocker()}
	request := deploymentRequest()
	request.Placement.AttachmentID = ""
	id, err := (&Manager{client: f}).Create(context.Background(), request)
	if id != "" || err == nil || errors.Is(err, deployment.ErrAllocationUnconfirmed) || f.createCalls != 0 {
		t.Fatalf("pre-allocation rejection returned identity=%q, error=%v, create calls=%d", id, err, f.createCalls)
	}
}
