package bridge

import (
	"context"
	"errors"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Reconcile an uncertain registration by sandbox ID. Only a confirmed NotFound
// permits resubmitting the same immutable binding; tool commands are never replayed.
func (s *Session) registerSandbox(ctx context.Context, request *v1.RegisterSandboxRequest) (*v1.SandboxAccess, error) {
	immutable := proto.Clone(request).(*v1.RegisterSandboxRequest)
	return s.admit(ctx, func(ctx context.Context) (*v1.SandboxAccess, error) {
		return s.service.client.RegisterSandbox(ctx, immutable)
	})
}

// admit makes one admission call and reconciles an uncertain or still-pending
// result by reading the sandbox's state, repeating the call when the bridge
// has no record of it.
func (s *Session) admit(ctx context.Context, call func(context.Context) (*v1.SandboxAccess, error)) (*v1.SandboxAccess, error) {
	access, err := call(ctx)
	for {
		if ctx.Err() != nil {
			return nil, errors.Join(err, ctx.Err())
		}
		if err == nil {
			if identityErr := s.validateAccess(access); identityErr != nil {
				return nil, identityErr
			}
			if access.State != v1.State_REGISTERING {
				return access, nil
			}
		} else if !uncertainControlResult(err) {
			return nil, err
		}
		if err := waitPoll(ctx); err != nil {
			return nil, err
		}
		access, err = s.service.client.GetSandbox(ctx, s.request())
		if status.Code(err) == codes.NotFound {
			access, err = call(ctx)
		}
	}
}
func uncertainControlResult(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		return true
	default:
		return false
	}
}
