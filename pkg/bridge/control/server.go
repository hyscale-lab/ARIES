// Package control manages independent sandbox access in one run-owned service.
package control

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Sandbox owns only this resource's native access and finalized evidence.
// Reserve opens the access before any target exists; Bind then attaches the
// registered target to it.
type Sandbox interface {
	Reserve(context.Context) (*v1.Endpoint, error)
	Bind(context.Context, *v1.RegisterSandboxRequest) error
	Release(context.Context) ([]*v1.Artifact, error)
}
type Config struct {
	OperationTimeout time.Duration
	NewSandbox       func(sandboxID string) Sandbox
}
type entry struct {
	mu sync.Mutex
	// taskID is set by a reservation or registration; request only once a
	// target is bound.
	taskID    string
	request   *v1.RegisterSandboxRequest
	access    *v1.SandboxAccess
	native    Sandbox
	busy      bool
	operation chan struct{}
	cancel    context.CancelFunc
}
type Server struct {
	v1.UnimplementedBridgeControlServer
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
	closing bool
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,255}$`)

func NewServer(cfg Config) (*Server, error) {
	if cfg.NewSandbox == nil {
		return nil, errors.New("sandbox factory is required")
	}
	if cfg.OperationTimeout <= 0 {
		cfg.OperationTimeout = time.Minute
	}
	return &Server{cfg: cfg, entries: make(map[string]*entry)}, nil
}
func (s *Server) Register(reg grpc.ServiceRegistrar) {
	v1.RegisterBridgeControlServer(reg, s)
	h := health.NewServer()
	h.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	h.SetServingStatus(v1.BridgeControl_ServiceDesc.ServiceName, healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(reg, h)
}
func (e *entry) snapshot() *v1.SandboxAccess { return proto.Clone(e.access).(*v1.SandboxAccess) }
func (e *entry) wait(ctx context.Context, ch <-chan struct{}) (*v1.SandboxAccess, error) {
	select {
	case <-ch:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.snapshot(), nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// ReserveSandbox opens access for a sandbox that does not exist yet, so its
// endpoint is known before the sandbox starts. A repeated reservation reports
// the same access.
func (s *Server) ReserveSandbox(ctx context.Context, r *v1.ReserveSandboxRequest) (*v1.SandboxAccess, error) {
	if !identifier.MatchString(r.GetSandboxId()) || !identifier.MatchString(r.GetMetadata().GetTaskId()) {
		return nil, status.Error(codes.InvalidArgument, "sandbox identity and task metadata are required")
	}
	s.mu.Lock()
	if e := s.entries[r.SandboxId]; e != nil {
		s.mu.Unlock()
		e.mu.Lock()
		defer e.mu.Unlock()
		// An explicit release reserves even an unseen ID, closing a late reservation.
		if e.taskID != "" && e.taskID != r.Metadata.TaskId {
			return nil, status.Error(codes.AlreadyExists, "sandbox is reserved for a different task")
		}
		return e.snapshot(), nil
	}
	if s.closing {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "bridge service is closing")
	}
	e := &entry{taskID: r.Metadata.TaskId, access: &v1.SandboxAccess{SandboxId: r.SandboxId, State: v1.State_REGISTERING}}
	s.entries[r.SandboxId] = e
	s.mu.Unlock()
	e.mu.Lock()
	ch := s.operate(e, v1.State_RESERVED, func(ctx context.Context, native Sandbox) (*v1.Endpoint, error) {
		return native.Reserve(ctx)
	})
	e.mu.Unlock()
	return e.wait(ctx, ch)
}

// RegisterSandbox binds a started sandbox's target to its reserved access, or
// reserves and binds in one step when the sandbox was not reserved.
func (s *Server) RegisterSandbox(ctx context.Context, r *v1.RegisterSandboxRequest) (*v1.SandboxAccess, error) {
	if !identifier.MatchString(r.GetSandboxId()) || r.GetTarget().GetRuntimeId() == "" || strings.ContainsAny(r.GetTarget().GetRuntimeId(), "\x00\r\n") || !identifier.MatchString(r.GetMetadata().GetTaskId()) {
		return nil, status.Error(codes.InvalidArgument, "sandbox identity, target and task metadata are required")
	}
	request := proto.Clone(r).(*v1.RegisterSandboxRequest)
	bind := func(ctx context.Context, native Sandbox) error { return native.Bind(ctx, request) }
	s.mu.Lock()
	if e := s.entries[r.SandboxId]; e != nil {
		s.mu.Unlock()
		return s.bindReserved(ctx, e, request, bind)
	}
	if s.closing {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "bridge service is closing")
	}
	e := &entry{taskID: r.Metadata.TaskId, request: request, access: &v1.SandboxAccess{SandboxId: r.SandboxId, State: v1.State_REGISTERING}}
	s.entries[r.SandboxId] = e
	s.mu.Unlock()
	e.mu.Lock()
	ch := s.operate(e, v1.State_READY, func(ctx context.Context, native Sandbox) (*v1.Endpoint, error) {
		endpoint, err := native.Reserve(ctx)
		if err == nil {
			err = bind(ctx, native)
		}
		return endpoint, err
	})
	e.mu.Unlock()
	return e.wait(ctx, ch)
}

// bindReserved binds a target to an existing entry: a reservation awaiting
// one, or an entry already bound or closed, which it reports unchanged.
func (s *Server) bindReserved(ctx context.Context, e *entry, request *v1.RegisterSandboxRequest, bind func(context.Context, Sandbox) error) (*v1.SandboxAccess, error) {
	e.mu.Lock()
	// A reservation still opening its access is waited for, not reported.
	for e.busy && e.request == nil {
		ch := e.operation
		e.mu.Unlock()
		if _, err := e.wait(ctx, ch); err != nil {
			return nil, err
		}
		e.mu.Lock()
	}
	if e.request != nil {
		defer e.mu.Unlock()
		if !proto.Equal(e.request, request) {
			return nil, status.Error(codes.AlreadyExists, "sandbox has a different immutable binding")
		}
		return e.snapshot(), nil
	}
	// An explicit release reserves even an unseen ID, closing a late registration.
	if e.access.State != v1.State_RESERVED {
		defer e.mu.Unlock()
		return e.snapshot(), nil
	}
	if e.taskID != request.Metadata.TaskId {
		e.mu.Unlock()
		return nil, status.Error(codes.AlreadyExists, "sandbox is reserved for a different task")
	}
	e.request = request
	endpoint := e.access.Endpoint
	ch := s.operate(e, v1.State_READY, func(ctx context.Context, native Sandbox) (*v1.Endpoint, error) {
		return endpoint, bind(ctx, native)
	})
	e.mu.Unlock()
	return e.wait(ctx, ch)
}

// operate runs one native operation for e, which the caller holds locked,
// outside every lock, and records success as the given state. A failure
// releases whatever the operation opened.
func (s *Server) operate(e *entry, success v1.State, operation func(context.Context, Sandbox) (*v1.Endpoint, error)) chan struct{} {
	e.access.State = v1.State_REGISTERING
	e.busy = true
	e.operation = make(chan struct{})
	ch := e.operation
	opctx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeout)
	e.cancel = cancel
	native := e.native
	id := e.access.SandboxId
	go func() {
		if native == nil {
			native = s.cfg.NewSandbox(id)
		}
		var endpoint *v1.Endpoint
		var err error
		if native == nil {
			err = errors.New("native sandbox factory returned nil")
		} else {
			endpoint, err = operation(opctx, native)
		}
		if err == nil && (endpoint == nil || endpoint.Port == 0 || endpoint.Port > 65535) {
			err = errors.New("native bridge did not provide a ready endpoint")
		}
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.native = native
		e.busy = false
		if err != nil {
			e.access.Error = err.Error()
			e.access.State = v1.State_RELEASING
		}
		if e.access.State == v1.State_REGISTERING {
			e.access.Endpoint = endpoint
			e.access.State = success
		}
		close(ch)
		if e.access.State == v1.State_RELEASING {
			s.releaseLocked(e)
		}
	}()
	return ch
}
func (s *Server) find(id string) (*entry, error) {
	if !identifier.MatchString(id) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox identity")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	if e == nil {
		return nil, status.Error(codes.NotFound, "sandbox not registered")
	}
	return e, nil
}
func (s *Server) GetSandbox(ctx context.Context, r *v1.SandboxRequest) (*v1.SandboxAccess, error) {
	e, err := s.find(r.GetSandboxId())
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.snapshot(), nil
}
func (s *Server) ReleaseSandbox(ctx context.Context, r *v1.SandboxRequest) (*v1.SandboxAccess, error) {
	if !identifier.MatchString(r.GetSandboxId()) {
		return nil, status.Error(codes.InvalidArgument, "invalid sandbox identity")
	}
	s.mu.Lock()
	e := s.entries[r.SandboxId]
	if e == nil {
		// Preserve this closed ID so an in-flight registration cannot admit after
		// cleanup reports success. No target or evidence ever existed for this ID.
		e = &entry{access: &v1.SandboxAccess{SandboxId: r.SandboxId, State: v1.State_RELEASED}}
		s.entries[r.SandboxId] = e
	}
	s.mu.Unlock()
	e.mu.Lock()
	if e.access.State == v1.State_RELEASED {
		a := e.snapshot()
		e.mu.Unlock()
		return a, nil
	}
	s.releaseLocked(e)
	ch := e.operation
	e.mu.Unlock()
	for {
		if _, err := e.wait(ctx, ch); err != nil {
			return nil, err
		}
		e.mu.Lock()
		if !e.busy {
			a := e.snapshot()
			e.mu.Unlock()
			return a, nil
		}
		ch = e.operation
		e.mu.Unlock()
	}
}
func (s *Server) releaseLocked(e *entry) {
	if e.access.State == v1.State_RELEASED {
		return
	}
	e.access.State = v1.State_RELEASING
	e.access.Endpoint = nil
	if e.cancel != nil {
		e.cancel()
	}
	if e.busy {
		return
	}
	e.busy = true
	e.operation = make(chan struct{})
	ch := e.operation
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.OperationTimeout)
	go func() {
		var artifacts []*v1.Artifact
		var err error
		if e.native != nil {
			artifacts, err = e.native.Release(ctx)
		}
		cancel()
		e.mu.Lock()
		defer e.mu.Unlock()
		e.busy = false
		if err != nil {
			e.access.CleanupError = err.Error()
		} else {
			e.access.CleanupError = ""
			e.access.State = v1.State_RELEASED
			e.access.Artifacts = artifacts
			e.native = nil
			e.cancel = nil
		}
		close(ch)
	}()
}

// Close stops new registration and independently releases every retained resource.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	failures := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := s.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: id})
			if err == nil && a.State != v1.State_RELEASED {
				err = errors.New(a.CleanupError)
			}
			failures <- err
		}()
	}
	wg.Wait()
	close(failures)
	var errs []error
	for err := range failures {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
