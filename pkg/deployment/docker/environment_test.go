package docker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
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

func TestRunEnvironmentOwnsNetworkAndTaskHandlesBorrow(t *testing.T) {
	ctx := context.Background()
	f := &environmentClient{fakeClient: &fakeClient{}}
	e := (&Manager{client: f}).NewRunEnvironment("run")
	placement, err := e.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f.networkOptions.Internal || f.networkOptions.Labels["aries.kind"] != "run-network" || f.networkOptions.Labels["aries.task"] != "" {
		t.Fatal("network must be run-owned with shared egress", f.networkOptions)
	}
	first, second := e.NewTaskEnvironment(), e.NewTaskEnvironment()
	req := deployment.TaskEnvironmentRequest{SandboxRequest: core.SandboxRequest{RunID: "run", TaskID: "same", Environment: core.Environment{Services: core.TaskServices{SearchPort: 8123}}}, RuntimeName: "sandbox-a"}
	a, err := first.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	req.RuntimeName = "evaluation-b"
	b, err := second.Start(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Placement != placement || b.Placement != placement || a.SearchURL != "http://sandbox-a:8123" || b.SearchURL != "http://evaluation-b:8123" {
		t.Fatal(a, b, placement)
	}
	if len(a.MCPServers) != 0 {
		t.Fatal("a task without MCP services got servers", a.MCPServers)
	}
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if f.removeCalls != 0 || second.Validate(ctx) != nil {
		t.Fatal("task stop removed shared network")
	}
	if _, err := first.Start(ctx, req); err == nil {
		t.Fatal("reused task handle")
	}
	labels := f.networkOptions.Labels
	f.networkOptions.Labels = map[string]string{"aries.run": "foreign"}
	if second.Validate(ctx) == nil || e.Stop(ctx) == nil || f.removeCalls != 0 {
		t.Fatal("accepted foreign network")
	}
	f.networkOptions.Labels = labels
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
	if second.Validate(ctx) == nil {
		t.Fatal("borrower accepted removed network")
	}
}

func TestRunEnvironmentRetainsPartialAllocationForCleanup(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(fmt.Sprint(omit), func(t *testing.T) {
			f := &environmentClient{fakeClient: &fakeClient{}, omitIdentity: omit, createFailure: errors.New("lost response")}
			e := (&Manager{client: f}).NewRunEnvironment("run")
			if _, err := e.Start(context.Background()); err == nil {
				t.Fatal("accepted failed allocation")
			}
			labels := f.networkOptions.Labels
			f.networkOptions.Labels = map[string]string{"aries.run": "foreign"}
			if e.Stop(context.Background()) == nil || f.removeCalls != 0 {
				t.Fatal("removed foreign network")
			}
			f.networkOptions.Labels = labels
			if err := e.Stop(context.Background()); err != nil || f.networkExists {
				t.Fatal("partial allocation leaked", err)
			}
		})
	}
}

func TestTaskEnvironmentRejectsWrongRunAndInvalidService(t *testing.T) {
	f := &environmentClient{fakeClient: &fakeClient{}}
	e := (&Manager{client: f}).NewRunEnvironment("run")
	if _, err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, request := range []deployment.TaskEnvironmentRequest{
		{SandboxRequest: core.SandboxRequest{RunID: "foreign"}, RuntimeName: "a"},
		{SandboxRequest: core.SandboxRequest{RunID: "run"}},
		{SandboxRequest: core.SandboxRequest{RunID: "run", Environment: core.Environment{Services: core.TaskServices{SearchPort: 65536}}}, RuntimeName: "a"},
	} {
		handle := e.NewTaskEnvironment()
		if _, err := handle.Start(context.Background(), request); err == nil {
			t.Fatal("accepted invalid request", request)
		}
		if err := handle.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if f.removeCalls != 0 {
		t.Fatal("invalid task removed shared network")
	}
	if err := e.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// A task's in-sandbox MCP servers are resolved like its search service: the
// sandbox's own runtime name on the run's network, the declared port and path.
func TestTaskEnvironmentResolvesTaskMCPServices(t *testing.T) {
	ctx := context.Background()
	e := (&Manager{client: &environmentClient{fakeClient: &fakeClient{}}}).NewRunEnvironment("run")
	if _, err := e.Start(ctx); err != nil {
		t.Fatal(err)
	}
	gateway := core.TaskMCPService{Name: "toolathlon", Port: 10086, Path: "/sse", Transport: "sse", TimeoutSeconds: 1200}
	request := deployment.TaskEnvironmentRequest{SandboxRequest: core.SandboxRequest{RunID: "run", TaskID: "task",
		Environment: core.Environment{Services: core.TaskServices{MCP: []core.TaskMCPService{gateway}}}}, RuntimeName: "sandbox-a"}
	got, err := e.NewTaskEnvironment().Start(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	want := []core.MCPServerConfig{{Name: "toolathlon", URL: "http://sandbox-a:10086/sse", Transport: "sse", TimeoutSeconds: 1200}}
	if !reflect.DeepEqual(got.MCPServers, want) {
		t.Fatalf("MCP servers = %+v, want %+v", got.MCPServers, want)
	}
	gateway.Port = 0
	request.Environment.Services.MCP = []core.TaskMCPService{gateway}
	if _, err := e.NewTaskEnvironment().Start(ctx, request); err == nil {
		t.Fatal("accepted a task MCP service without a port")
	}
}
