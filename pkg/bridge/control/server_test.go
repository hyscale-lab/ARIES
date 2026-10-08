package control

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func request() *v1.AssignSandboxRequest {
	return &v1.AssignSandboxRequest{InstanceId: "instance", AssignmentId: "assignment", ProtocolVersion: 1, CredentialId: "ssh", LeaseMillis: 1000, Target: &v1.Target{Version: 1, RunId: "run", TaskId: "task", OccurrenceId: "occurrence", Backend: "docker", RuntimeId: "immutable", RuntimeName: "sandbox", Workdir: "/app", MaxInputBytes: 16 << 20, MaxOutputBytes: 1 << 30, ExpectedLabels: map[string]string{"aries.managed": "true", "aries.kind": "task-container", "aries.component": "sandbox", "aries.run": "run", "aries.task": "task"}}}
}
func ref() *v1.AssignmentRequest {
	return &v1.AssignmentRequest{InstanceId: "instance", AssignmentId: "assignment"}
}
func config() Config {
	return Config{InstanceID: "instance", Token: strings.Repeat("t", 32), MaxLease: time.Second, OperationTimeout: time.Second, Assign: func(context.Context, *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
		return &v1.Endpoint{Host: "127.0.0.1", Port: 22}, nil
	}, Revoke: func(context.Context) ([]*v1.Artifact, error) {
		return []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "complete"}}, nil
	}}
}
func server(t *testing.T, c Config) *Server {
	t.Helper()
	s, err := NewServer(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Revoke)
	return s
}
func TestReservationAndLostAdmissionResponse(t *testing.T) {
	c := config()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c.Assign = func(context.Context, *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
		calls.Add(1)
		close(entered)
		<-release
		return &v1.Endpoint{Host: "127.0.0.1", Port: 22}, nil
	}
	s := server(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := s.AssignSandbox(ctx, request()); result <- err }()
	<-entered
	cancel()
	if status.Code(<-result) != codes.Canceled {
		t.Fatal("RPC did not time out")
	}
	a, err := s.AssignSandbox(context.Background(), request())
	if err != nil || a.State != v1.State_ASSIGNING || calls.Load() != 1 {
		t.Fatalf("duplicate admission: %v %v", a, err)
	}
	changed := proto.Clone(request()).(*v1.AssignSandboxRequest)
	changed.Target.Workdir = "/other"
	if _, err = s.AssignSandbox(context.Background(), changed); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	changed = request()
	changed.AssignmentId = "another"
	if _, err = s.AssignSandbox(context.Background(), changed); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	close(release)
	a, err = s.RevokeAssignment(context.Background(), ref())
	if err != nil || a.State != v1.State_REVOKED {
		t.Fatalf("%v %v", a, err)
	}
	a, err = s.AssignSandbox(context.Background(), request())
	if err != nil || a.State != v1.State_REVOKED || calls.Load() != 1 {
		t.Fatal("terminal identity reused")
	}
}
func TestRevokeContinuesAfterCallerTimeoutAndRetriesFailure(t *testing.T) {
	c := config()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	c.Revoke = func(context.Context) ([]*v1.Artifact, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return nil, errors.New("drain unconfirmed")
		}
		return []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "complete"}}, nil
	}
	s := server(t, c)
	if _, err := s.AssignSandbox(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := s.RevokeAssignment(ctx, ref()); result <- err }()
	<-entered
	cancel()
	if status.Code(<-result) != codes.Canceled {
		t.Fatal("RPC not canceled")
	}
	a, _ := s.GetAssignment(context.Background(), ref())
	if a.State != v1.State_REVOKING || a.Endpoint != nil {
		t.Fatal("revoking exposed endpoint")
	}
	close(release)
	s.mu.Lock()
	done := s.operation
	s.mu.Unlock()
	<-done
	a, _ = s.GetAssignment(context.Background(), ref())
	if a.State != v1.State_REVOKING || len(a.CleanupErrors) != 1 {
		t.Fatalf("failed cleanup lost: %v", a)
	}
	a, err := s.RevokeAssignment(context.Background(), ref())
	if err != nil || a.State != v1.State_REVOKED || len(a.Artifacts) != 1 || len(a.CleanupErrors) != 0 || len(a.Diagnostics) != 1 {
		t.Fatalf("retry: %v %v", a, err)
	}
	select {
	case <-s.Done():
	default:
		t.Fatal("terminal signal absent")
	}
}
func TestLeaseExpiryCannotRevive(t *testing.T) {
	s := server(t, config())
	r := request()
	r.LeaseMillis = 20
	if _, err := s.AssignSandbox(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("expired lease did not revoke")
	}
	_, err := s.RenewLease(context.Background(), &v1.RenewLeaseRequest{InstanceId: "instance", AssignmentId: "assignment", LeaseMillis: 1000})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal("expired lease revived", err)
	}
}
func TestAuthenticatedGRPCAndIdentity(t *testing.T) {
	s := server(t, config())
	g := grpc.NewServer(grpc.UnaryInterceptor(s.UnaryInterceptor), grpc.StreamInterceptor(s.StreamInterceptor))
	s.Register(g)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = g.Serve(l) }()
	t.Cleanup(g.Stop)
	conn, client, err := NewClient(l.Addr().String(), "instance", strings.Repeat("x", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = client.AssignSandbox(context.Background(), request())
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	conn2, client2, err := NewClient(l.Addr().String(), "instance", config().Token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn2.Close()
	r := request()
	r.ProtocolVersion = 2
	_, err = client2.AssignSandbox(context.Background(), r)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	a, err := client2.AssignSandbox(context.Background(), request())
	if err != nil || a.State != v1.State_READY {
		t.Fatalf("%v %v", a, err)
	}
	bad := ref()
	bad.InstanceId = "replacement"
	_, err = client2.GetAssignment(context.Background(), bad)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	if _, _, err = NewClient("192.0.2.1:1234", "instance", config().Token, nil); err == nil {
		t.Fatal("unauthenticated remote transport accepted")
	}
}

func TestAdmissionFailureRetainsIdentityAndCannotExposeEndpoint(t *testing.T) {
	c := config()
	var admitted atomic.Int32
	c.Assign = func(context.Context, *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
		admitted.Add(1)
		return nil, errors.New("target identity changed")
	}
	s := server(t, c)
	_, err := s.AssignSandbox(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.RevokeAssignment(context.Background(), ref())
	if err != nil || a.State != v1.State_REVOKED || a.Endpoint != nil {
		t.Fatalf("%v %v", a, err)
	}
	a, err = s.AssignSandbox(context.Background(), request())
	if err != nil || a.State != v1.State_REVOKED || admitted.Load() != 1 {
		t.Fatal("failed admission was replayed")
	}
	r := request()
	r.AssignmentId = "next"
	if _, err = s.AssignSandbox(context.Background(), r); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
}

func TestAdmissionDelegatesAlternativeBackendValidation(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "accepted"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			r := request()
			r.Target.Backend = "remote-exec"
			c := config()
			var calls atomic.Int32
			c.Assign = func(_ context.Context, received *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
				calls.Add(1)
				if !proto.Equal(received.Target, r.Target) {
					return nil, errors.New("provider received a different grant")
				}
				if reject {
					return nil, errors.New("provider rejected resource ownership")
				}
				return &v1.Endpoint{Host: "127.0.0.1", Port: 22}, nil
			}
			s := server(t, c)
			a, err := s.AssignSandbox(context.Background(), r)
			if err != nil || calls.Load() != 1 || !proto.Equal(a.Target, r.Target) {
				t.Fatalf("alternative backend did not reach provider admission: assignment=%v calls=%d err=%v", a, calls.Load(), err)
			}
			if reject {
				if a.State == v1.State_READY || a.Endpoint != nil || len(a.Diagnostics) != 1 || a.Diagnostics[0].Message != "provider rejected resource ownership" {
					t.Fatalf("provider rejection lost or exposed endpoint: %v", a)
				}
			} else if a.State != v1.State_READY || a.Endpoint == nil {
				t.Fatalf("provider approval did not admit target: %v", a)
			}
		})
	}
}

func TestRenewalChecksMonotonicExpiryBeforeTimerRuns(t *testing.T) {
	s := server(t, config())
	if _, err := s.AssignSandbox(context.Background(), request()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.timer.Stop()
	s.expiry = time.Now().Add(-time.Millisecond)
	s.mu.Unlock()
	_, err := s.RenewLease(context.Background(), &v1.RenewLeaseRequest{InstanceId: "instance", AssignmentId: "assignment", LeaseMillis: 1000})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatal("renewal revived expired grant", err)
	}
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("renewal did not initiate expired cleanup")
	}
}
