package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type admissionClient struct {
	v1.BridgeControlClient
	assign   func(context.Context, *v1.AssignSandboxRequest) (*v1.Assignment, error)
	get      func(context.Context, *v1.AssignmentRequest) (*v1.Assignment, error)
	requests []*v1.AssignSandboxRequest
	gets     int
}

func (c *admissionClient) AssignSandbox(ctx context.Context, r *v1.AssignSandboxRequest, _ ...grpc.CallOption) (*v1.Assignment, error) {
	c.requests = append(c.requests, proto.Clone(r).(*v1.AssignSandboxRequest))
	return c.assign(ctx, r)
}
func (c *admissionClient) GetAssignment(ctx context.Context, r *v1.AssignmentRequest, _ ...grpc.CallOption) (*v1.Assignment, error) {
	c.gets++
	return c.get(ctx, r)
}

func TestAdmissionReconcilesDroppedRequestAndLostResponse(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		name := "dropped request"
		if admitted {
			name = "lost admitted response"
		}
		t.Run(name, func(t *testing.T) {
			grants := 0
			server, err := control.NewServer(control.Config{InstanceID: "instance", Token: strings.Repeat("x", 32), Assign: func(context.Context, *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
				grants++
				return &v1.Endpoint{Host: "127.0.0.1", Port: 22}, nil
			}, Revoke: func(context.Context) ([]*v1.Artifact, error) { return nil, nil }})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Revoke()
			d := core.BridgeTarget{Version: 1, RunID: "run", TaskID: "task", OccurrenceID: "occurrence", Backend: "docker", RuntimeID: "runtime", RuntimeName: "sandbox", Workdir: "/app", MaxInputBytes: 16 << 20, MaxOutputBytes: 1 << 30, ExpectedLabels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run", "aries.task": "task"}}
			request := &v1.AssignSandboxRequest{InstanceId: "instance", AssignmentId: "assignment", ProtocolVersion: 1, Target: control.TargetToProto(d), CredentialId: "ssh"}
			client := &admissionClient{get: server.GetAssignment}
			client.assign = func(ctx context.Context, r *v1.AssignSandboxRequest) (*v1.Assignment, error) {
				if len(client.requests) == 1 {
					if admitted {
						if _, err := server.AssignSandbox(ctx, r); err != nil {
							t.Fatal(err)
						}
					}
					return nil, status.Error(codes.Unavailable, "lost exchange")
				}
				return server.AssignSandbox(ctx, r)
			}
			m := &Manager{client: client, instance: "instance", assignment: "assignment", descriptor: d}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			a, err := m.assignSandbox(ctx, request)
			if err != nil || a.State != v1.State_READY {
				t.Fatalf("reconciliation: %+v %v", a, err)
			}
			want := 2
			if admitted {
				want = 1
			}
			if len(client.requests) != want || client.gets != 1 || grants != 1 {
				t.Fatalf("assign=%d get=%d grants=%d", len(client.requests), client.gets, grants)
			}
			for _, sent := range client.requests {
				if !proto.Equal(sent, request) {
					t.Fatal("reconciliation changed immutable request")
				}
			}
		})
	}
}

func TestAdmissionDoesNotRetryUnrelatedOrIdentityErrors(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.AlreadyExists, codes.FailedPrecondition, codes.PermissionDenied, codes.Unauthenticated, codes.Internal} {
		for _, duringGet := range []bool{false, true} {
			client := &admissionClient{}
			client.assign = func(context.Context, *v1.AssignSandboxRequest) (*v1.Assignment, error) {
				if duringGet {
					return nil, status.Error(codes.Unavailable, "lost response")
				}
				return nil, status.Error(code, "rejected")
			}
			client.get = func(context.Context, *v1.AssignmentRequest) (*v1.Assignment, error) {
				return nil, status.Error(code, "rejected")
			}
			m := &Manager{client: client, instance: "instance", assignment: "assignment"}
			_, err := m.assignSandbox(context.Background(), &v1.AssignSandboxRequest{InstanceId: "instance", AssignmentId: "assignment"})
			if status.Code(err) != code || len(client.requests) != 1 {
				t.Fatalf("code=%s get=%v retried unrelated error: %v calls=%d", code, duringGet, err, len(client.requests))
			}
		}
	}
}
