// Package control manages one sandbox assignment per runtime.
package control

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Config struct {
	InstanceID       string
	OperationTimeout time.Duration
	Assign           func(context.Context, *v1.AssignSandboxRequest) (*v1.Endpoint, error)
	Revoke           func(context.Context) ([]*v1.Artifact, error)
}
type Server struct {
	revoking     chan struct{}
	revokingOnce sync.Once
	v1.UnimplementedBridgeControlServer
	cfg        Config
	mu         sync.Mutex
	request    *v1.AssignSandboxRequest
	assignment *v1.Assignment
	busy       bool
	operation  chan struct{}
	cancel     context.CancelFunc
	terminal   chan struct{}
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

func NewServer(cfg Config) (*Server, error) {
	if !identifier.MatchString(cfg.InstanceID) || cfg.Assign == nil || cfg.Revoke == nil {
		return nil, errors.New("invalid bridge control configuration")
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = time.Minute
	}
	return &Server{cfg: cfg, terminal: make(chan struct{}), revoking: make(chan struct{})}, nil
}

// Done closes only after confirmed native revocation and evidence finalization.
func (s *Server) Done() <-chan struct{} { return s.terminal }

// Revoking closes when admission is closed, even if eventual cleanup fails.
func (s *Server) Revoking() <-chan struct{} { return s.revoking }
func (s *Server) HasAssignment() bool       { s.mu.Lock(); defer s.mu.Unlock(); return s.assignment != nil }
func (s *Server) Register(reg grpc.ServiceRegistrar) {
	v1.RegisterBridgeControlServer(reg, s)
	h := health.NewServer()
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	h.SetServingStatus(v1.BridgeControl_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(reg, h)
}

func (s *Server) identity(instance, id string) error {
	if instance != s.cfg.InstanceID {
		return status.Error(codes.FailedPrecondition, "bridge instance mismatch")
	}
	if !identifier.MatchString(id) {
		return status.Error(codes.InvalidArgument, "invalid assignment identity")
	}
	return nil
}
func (s *Server) snapshot() *v1.Assignment {
	return proto.Clone(s.assignment).(*v1.Assignment)
}
func (s *Server) wait(ctx context.Context, ch <-chan struct{}) (*v1.Assignment, error) {
	select {
	case <-ch:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.snapshot(), nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}
func (s *Server) AssignSandbox(ctx context.Context, r *v1.AssignSandboxRequest) (*v1.Assignment, error) {
	if err := s.identity(r.GetInstanceId(), r.GetAssignmentId()); err != nil {
		return nil, err
	}
	if r.ProtocolVersion != 1 || r.Target == nil || !identifier.MatchString(r.CredentialId) {
		return nil, status.Error(codes.InvalidArgument, "invalid assignment protocol or credential identifier")
	}
	if err := target.Validate(TargetFromProto(r.Target)); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	if s.request != nil {
		defer s.mu.Unlock()
		if !proto.Equal(s.request, r) {
			return nil, status.Error(codes.AlreadyExists, "runtime already reserved with different immutable inputs")
		}
		return s.snapshot(), nil
	}
	s.request = proto.Clone(r).(*v1.AssignSandboxRequest)
	s.assignment = &v1.Assignment{InstanceId: r.InstanceId, AssignmentId: r.AssignmentId, Target: proto.Clone(r.Target).(*v1.Target), State: v1.State_ASSIGNING}
	s.busy = true
	s.operation = make(chan struct{})
	ch := s.operation
	opctx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeout)
	s.cancel = cancel
	s.mu.Unlock()
	go func() {
		endpoint, err := s.cfg.Assign(opctx, proto.Clone(r).(*v1.AssignSandboxRequest))
		if err == nil && (endpoint == nil || endpoint.Host == "" || endpoint.Port == 0 || endpoint.Port > 65535) {
			err = errors.New("native bridge did not provide a ready endpoint")
		}
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.busy = false
		if err != nil {
			s.assignment.Diagnostics = append(s.assignment.Diagnostics, &v1.CleanupError{Stage: "admission", Message: err.Error()})
			s.assignment.State = v1.State_REVOKING
		}
		if s.assignment.State == v1.State_ASSIGNING {
			s.assignment.Endpoint = endpoint
			s.assignment.State = v1.State_READY
		} else {
			s.assignment.State = v1.State_REVOKING
		}
		close(ch)
		if s.assignment.State == v1.State_REVOKING {
			s.revokeLocked()
		}
	}()
	return s.wait(ctx, ch)
}
func (s *Server) match(r *v1.AssignmentRequest) error {
	if err := s.identity(r.GetInstanceId(), r.GetAssignmentId()); err != nil {
		return err
	}
	if s.assignment == nil || s.assignment.AssignmentId != r.AssignmentId {
		return status.Error(codes.NotFound, "assignment not found")
	}
	return nil
}
func (s *Server) GetAssignment(ctx context.Context, r *v1.AssignmentRequest) (*v1.Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.match(r); err != nil {
		return nil, err
	}
	return s.snapshot(), nil
}
func (s *Server) RevokeAssignment(ctx context.Context, r *v1.AssignmentRequest) (*v1.Assignment, error) {
	s.mu.Lock()
	if err := s.match(r); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.assignment.State == v1.State_REVOKED {
		a := s.snapshot()
		s.mu.Unlock()
		return a, nil
	}
	s.revokeLocked()
	ch := s.operation
	s.mu.Unlock()
	// Admission may still be running. Wait for its ownership to transfer to cleanup.
	for {
		if _, err := s.wait(ctx, ch); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if !s.busy {
			a := s.snapshot()
			s.mu.Unlock()
			return a, nil
		}
		ch = s.operation
		s.mu.Unlock()
	}
}

// Revoke starts service-owned cleanup during explicit service shutdown.
func (s *Server) Revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.assignment != nil {
		s.revokeLocked()
	}
}
func (s *Server) revokeLocked() {
	if s.assignment.State == v1.State_REVOKED {
		return
	}
	s.revokingOnce.Do(func() { close(s.revoking) })
	s.assignment.State = v1.State_REVOKING
	s.assignment.Endpoint = nil
	if s.cancel != nil {
		s.cancel()
	}
	if !s.busy {
		s.startRevokeLocked()
	}
}
func (s *Server) startRevokeLocked() {
	s.busy = true
	s.operation = make(chan struct{})
	ch := s.operation
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeout)
	go func() {
		artifacts, err := s.cfg.Revoke(ctx)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.busy = false
		if err != nil {
			s.assignment.CleanupErrors = []*v1.CleanupError{{Stage: "revocation", Message: err.Error()}}
			s.assignment.Diagnostics = append(s.assignment.Diagnostics, &v1.CleanupError{Stage: "revocation", Message: err.Error()})
		} else {
			s.assignment.CleanupErrors = nil
			s.assignment.State = v1.State_REVOKED
			s.assignment.Artifacts = artifacts
			close(s.terminal)
		}
		close(ch)
	}()
}
