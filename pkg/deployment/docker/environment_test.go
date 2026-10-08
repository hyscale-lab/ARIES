package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/client"
)

type environmentClient struct {
	*fakeClient
	createFailure error
	removeFailure error
	removeCalls   int
	omitIdentity  bool
}

func (f *environmentClient) NetworkCreate(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	result, err := f.fakeClient.NetworkCreate(ctx, name, options)
	if f.omitIdentity {
		result.ID = ""
	}
	return result, errors.Join(err, f.createFailure)
}
func (f *environmentClient) NetworkRemove(ctx context.Context, id string, options client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	f.removeCalls++
	if f.removeFailure != nil {
		return client.NetworkRemoveResult{}, f.removeFailure
	}
	return f.fakeClient.NetworkRemove(ctx, id, options)
}

func TestTaskEnvironmentOwnershipAndRetry(t *testing.T) {
	ctx := context.Background()
	f := &environmentClient{fakeClient: &fakeClient{}}
	m := &Manager{client: f}
	e := m.NewTaskEnvironment()
	request := core.SandboxRequest{RunID: "run", TaskID: "duplicate"}
	first, err := e.Start(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	f.networkOptions.Labels = map[string]string{"aries.task": "foreign"}
	if e.Validate(ctx) == nil {
		t.Fatal("accepted changed ownership")
	}
	if e.Stop(ctx) == nil || f.removeCalls != 0 {
		t.Fatal("removed foreign network")
	}
	f.networkOptions.Labels = map[string]string{"aries.managed": "true", "aries.kind": "task-network", "aries.run": "run", "aries.task": "duplicate"}
	f.removeFailure = errors.New("network remains")
	if e.Stop(ctx) == nil || !f.networkExists {
		t.Fatal("unconfirmed removal succeeded")
	}
	f.removeFailure = nil
	if err := e.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.Stop(ctx); err != nil || f.removeCalls != 2 {
		t.Fatal(err, f.removeCalls)
	}
	if _, err := e.Start(ctx, request); err == nil {
		t.Fatal("reused occurrence")
	}
	next := m.NewTaskEnvironment()
	second, err := next.Start(ctx, request)
	if err != nil || first.Placement.AttachmentID == second.Placement.AttachmentID {
		t.Fatal("duplicate task reused network", first, second, err)
	}
	if err := next.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTaskEnvironmentRetainsPartialAllocationForCleanup(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("create response failed")
	f := &environmentClient{fakeClient: &fakeClient{}, createFailure: failure}
	e := (&Manager{client: f}).NewTaskEnvironment()
	if _, err := e.Start(ctx, core.SandboxRequest{RunID: "run", TaskID: "task"}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := e.Stop(ctx); err != nil || f.networkExists {
		t.Fatal("partial network leaked", err)
	}
}

func TestTaskEnvironmentRecoversAllocationWithoutReturnedIdentity(t *testing.T) {
	f := &environmentClient{fakeClient: &fakeClient{}, omitIdentity: true}
	e := (&Manager{client: f}).NewTaskEnvironment()
	if _, err := e.Start(context.Background(), core.SandboxRequest{RunID: "run", TaskID: "task"}); err == nil {
		t.Fatal("accepted missing network identity")
	}
	labels := f.networkOptions.Labels
	f.networkOptions.Labels = map[string]string{"aries.task": "foreign"}
	if err := e.Stop(context.Background()); err == nil || f.removeCalls != 0 {
		t.Fatal("removed foreign network during identity recovery", err)
	}
	f.networkOptions.Labels = labels
	if err := e.Stop(context.Background()); err != nil || f.networkExists {
		t.Fatal("lost allocation after missing identity", err)
	}
}

func TestTaskEnvironmentResolvesDeclaredServices(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		port int
		want string
	}{{8123, "http://task-sandbox:8123"}, {0, ""}, {65536, ""}} {
		f := &environmentClient{fakeClient: &fakeClient{}}
		e := (&Manager{client: f}).NewTaskEnvironment()
		request := core.SandboxRequest{RunID: "run", TaskID: "task", Environment: core.Environment{Services: core.TaskServices{SearchPort: test.port}}}
		resolved, err := e.Start(ctx, request)
		if test.port == 65536 {
			if err == nil || f.networkExists {
				t.Fatal("invalid service allocated a network", err)
			}
		} else if err != nil || resolved.SearchURL != test.want || resolved.Placement.AttachmentID == "" {
			t.Fatalf("port %d: resolved = %#v, %v", test.port, resolved, err)
		}
		if err := e.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := e.Start(ctx, request); err == nil {
			t.Fatal("reused revoked attachment")
		}
	}
}
