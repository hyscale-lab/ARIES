package bridge

import (
	"context"
	"errors"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// assignSandbox reconciles an uncertain submission on the same control
// connection. Only an explicit NotFound permits resubmitting the identical grant.
func (m *Manager) assignSandbox(ctx context.Context, request *v1.AssignSandboxRequest) (*v1.Assignment, error) {
	immutable := proto.Clone(request).(*v1.AssignSandboxRequest)
	assignment, err := m.client.AssignSandbox(ctx, immutable)
	for {
		if ctx.Err() != nil {
			return nil, errors.Join(err, ctx.Err())
		}
		if err == nil {
			if identityErr := m.validateAssignment(assignment); identityErr != nil {
				return nil, identityErr
			}
			if assignment.State != v1.State_ASSIGNING {
				return assignment, nil
			}
		} else if !uncertainControlResult(err) {
			return nil, err
		}
		if err := waitPoll(ctx); err != nil {
			return nil, err
		}
		assignment, err = m.client.GetAssignment(ctx, m.assignmentRequest())
		if status.Code(err) == codes.NotFound {
			// A late original request is safe: the server atomically reserves the
			// same ID and rejects any changed immutable inputs or another instance.
			assignment, err = m.client.AssignSandbox(ctx, immutable)
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
