package bridge

import (
	"context"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

type admissionClient struct {
	v1.BridgeControlClient
	calls, gets int
	request     *v1.RegisterSandboxRequest
	missing     bool
}

func (c *admissionClient) RegisterSandbox(_ context.Context, r *v1.RegisterSandboxRequest, _ ...grpc.CallOption) (*v1.SandboxAccess, error) {
	c.calls++
	if c.request != nil && !proto.Equal(c.request, r) {
		panic("registration changed")
	}
	c.request = proto.Clone(r).(*v1.RegisterSandboxRequest)
	if c.calls == 1 {
		return nil, status.Error(codes.Unavailable, "response lost")
	}
	return &v1.SandboxAccess{SandboxId: r.SandboxId, State: v1.State_READY}, nil
}
func (c *admissionClient) GetSandbox(_ context.Context, r *v1.SandboxRequest, _ ...grpc.CallOption) (*v1.SandboxAccess, error) {
	c.gets++
	if c.missing {
		return nil, status.Error(codes.NotFound, "not yet reserved")
	}
	return &v1.SandboxAccess{SandboxId: r.SandboxId, State: v1.State_READY}, nil
}
func TestRegistrationReconcilesOriginalSandboxID(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "response-lost", true: "not-reserved"}[missing], func(t *testing.T) {
			c := &admissionClient{missing: missing}
			s := &Session{service: &Service{client: c}, descriptor: core.BridgeTarget{SandboxID: "sandbox"}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			a, err := s.registerSandbox(ctx, &v1.RegisterSandboxRequest{SandboxId: "sandbox", Target: &v1.Target{RuntimeId: "immutable"}})
			if err != nil || a.SandboxId != "sandbox" {
				t.Fatal(a, err)
			}
			want := 1
			if missing {
				want = 2
			}
			if c.calls != want || c.gets != 1 {
				t.Fatal(c.calls, c.gets)
			}
		})
	}
}
func TestUncertainControlCodes(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled} {
		if !uncertainControlResult(status.Error(code, "test")) {
			t.Fatal(code)
		}
	}
	for _, code := range []codes.Code{codes.InvalidArgument, codes.AlreadyExists, codes.FailedPrecondition} {
		if uncertainControlResult(status.Error(code, "test")) {
			t.Fatal(code)
		}
	}
}
