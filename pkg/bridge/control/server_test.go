package control

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

type testSandbox struct {
	register func(context.Context) (*v1.Endpoint, error)
	bind     func(context.Context, *v1.RegisterSandboxRequest) error
	release  func(context.Context) ([]*v1.Artifact, error)
}

func (s testSandbox) Reserve(ctx context.Context) (*v1.Endpoint, error) {
	if s.register != nil {
		return s.register(ctx)
	}
	return &v1.Endpoint{Port: 2222, Transport: "ssh", User: "aries"}, nil
}
func (s testSandbox) Bind(ctx context.Context, r *v1.RegisterSandboxRequest) error {
	if s.bind != nil {
		return s.bind(ctx, r)
	}
	return nil
}
func (s testSandbox) Release(ctx context.Context) ([]*v1.Artifact, error) {
	if s.release != nil {
		return s.release(ctx)
	}
	return nil, nil
}
func request(id string) *v1.RegisterSandboxRequest {
	return &v1.RegisterSandboxRequest{SandboxId: id, Target: &v1.Target{RuntimeId: "runtime-" + id, Workdir: "/work"}, Metadata: &v1.Metadata{TaskId: "repeated-task"}}
}
func TestIndependentSandboxLifecycle(t *testing.T) {
	blocked := make(chan struct{})
	releaseStarted := make(chan struct{})
	var once sync.Once
	s, err := NewServer(Config{NewSandbox: func(id string) Sandbox {
		return testSandbox{release: func(ctx context.Context) ([]*v1.Artifact, error) {
			if id == "a" {
				once.Do(func() { close(releaseStarted) })
				select {
				case <-blocked:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return nil, nil
		}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range []string{"a", "b"} {
		a, err := s.RegisterSandbox(ctx, request(id))
		if err != nil || a.State != v1.State_READY || a.SandboxId != id {
			t.Fatalf("registration %+v %v", a, err)
		}
	}
	if a, err := s.RegisterSandbox(ctx, request("a")); err != nil || a.State != v1.State_READY {
		t.Fatal(a, err)
	}
	conflict := request("a")
	conflict.Target.RuntimeId = "different"
	if _, err := s.RegisterSandbox(ctx, conflict); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: "a"}); done <- err }()
	<-releaseStarted
	b, err := s.GetSandbox(ctx, &v1.SandboxRequest{SandboxId: "b"})
	if err != nil || b.State != v1.State_READY {
		t.Fatal(b, err)
	}
	c, err := s.RegisterSandbox(ctx, request("c"))
	if err != nil || c.State != v1.State_READY {
		t.Fatal(c, err)
	}
	close(blocked)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a, err := s.RegisterSandbox(ctx, request("a")); err != nil || a.State != v1.State_RELEASED {
		t.Fatal("release reopened", a, err)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterSandbox(ctx, request("later")); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
}
func TestCanceledWaitRetainsRegistrationAndReleaseOwnership(t *testing.T) {
	entered := make(chan struct{})
	done := make(chan struct{})
	releases := 0
	s, _ := NewServer(Config{NewSandbox: func(string) Sandbox {
		return testSandbox{register: func(ctx context.Context) (*v1.Endpoint, error) {
			close(entered)
			<-done
			return &v1.Endpoint{Port: 2222}, nil
		}, release: func(context.Context) ([]*v1.Artifact, error) { releases++; return nil, nil }}
	}})
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan error, 1)
	go func() { _, err := s.RegisterSandbox(ctx, request("a")); out <- err }()
	<-entered
	cancel()
	if status.Code(<-out) != codes.Canceled {
		t.Fatal("canceled waiter accepted")
	}
	close(done)
	bounded, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	a, err := s.ReleaseSandbox(bounded, &v1.SandboxRequest{SandboxId: "a"})
	if err != nil || a.State != v1.State_RELEASED || releases != 1 {
		t.Fatal(a, err, releases)
	}
}
func TestReleaseBeforeLateRegistrationClosesSandboxID(t *testing.T) {
	starts := 0
	s, _ := NewServer(Config{NewSandbox: func(string) Sandbox { starts++; return testSandbox{} }})
	ctx := context.Background()
	a, err := s.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: "late"})
	if err != nil || a.State != v1.State_RELEASED {
		t.Fatal(a, err)
	}
	a, err = s.RegisterSandbox(ctx, request("late"))
	if err != nil || a.State != v1.State_RELEASED || starts != 0 {
		t.Fatal(a, err, starts)
	}
}
func TestFailedRegistrationCleansOnlyItsOwnNativeState(t *testing.T) {
	mu := sync.Mutex{}
	calls := 0
	s, _ := NewServer(Config{NewSandbox: func(id string) Sandbox {
		return testSandbox{register: func(context.Context) (*v1.Endpoint, error) {
			if id == "bad" {
				return nil, errors.New("admission failed")
			}
			return &v1.Endpoint{Port: 3333}, nil
		}, release: func(context.Context) ([]*v1.Artifact, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if id == "bad" && calls == 1 {
				return nil, errors.New("cleanup failed")
			}
			return nil, nil
		}}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.RegisterSandbox(ctx, request("good"))
	_, _ = s.RegisterSandbox(ctx, request("bad"))
	for {
		a, _ := s.GetSandbox(ctx, &v1.SandboxRequest{SandboxId: "bad"})
		if a.CleanupError != "" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	a, err := s.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: "bad"})
	if err != nil || a.State != v1.State_RELEASED || a.Error != "admission failed" || a.CleanupError != "" {
		t.Fatal(a, err)
	}
	good, _ := s.GetSandbox(ctx, &v1.SandboxRequest{SandboxId: "good"})
	if good.State != v1.State_READY {
		t.Fatal(good)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestPlaintextGRPCSandboxIdentity(t *testing.T) {
	s, _ := NewServer(Config{NewSandbox: func(string) Sandbox { return testSandbox{} }})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	s.Register(g)
	go g.Serve(l)
	defer g.Stop()
	conn, c, err := NewClient(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal(err)
	}
	if a, err := c.RegisterSandbox(ctx, request("a")); err != nil || a.SandboxId != "a" {
		t.Fatal(a, err)
	}
	if _, err := c.GetSandbox(ctx, &v1.SandboxRequest{SandboxId: "missing"}); status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
	if a, err := c.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: "a"}); err != nil || a.State != v1.State_RELEASED {
		t.Fatal(a, err)
	}
}
func TestClientAcceptsDeploymentAddress(t *testing.T) {
	conn, _, err := NewClient("bridge.fixture:8443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func reservation(id, task string) *v1.ReserveSandboxRequest {
	return &v1.ReserveSandboxRequest{SandboxId: id, Metadata: &v1.Metadata{TaskId: task}}
}

// A reservation opens access before the sandbox exists; registration then
// binds the target to the same endpoint.
func TestReservationThenRegistrationBindsTheSameEndpoint(t *testing.T) {
	var bound []*v1.RegisterSandboxRequest
	reserves := 0
	s, _ := NewServer(Config{NewSandbox: func(string) Sandbox {
		return testSandbox{register: func(context.Context) (*v1.Endpoint, error) {
			reserves++
			return &v1.Endpoint{Port: 4022, Transport: "ssh", User: "aries"}, nil
		}, bind: func(_ context.Context, r *v1.RegisterSandboxRequest) error {
			bound = append(bound, r)
			return nil
		}}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := s.ReserveSandbox(ctx, reservation("a", "repeated-task"))
	if err != nil || a.State != v1.State_RESERVED || a.Endpoint.GetPort() != 4022 || len(bound) != 0 {
		t.Fatal(a, err, bound)
	}
	if again, err := s.ReserveSandbox(ctx, reservation("a", "repeated-task")); err != nil || again.State != v1.State_RESERVED || reserves != 1 {
		t.Fatal(again, err, reserves)
	}
	if _, err := s.ReserveSandbox(ctx, reservation("a", "other-task")); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	other := request("a")
	other.Metadata.TaskId = "other-task"
	if _, err := s.RegisterSandbox(ctx, other); status.Code(err) != codes.AlreadyExists {
		t.Fatal(err)
	}
	a, err = s.RegisterSandbox(ctx, request("a"))
	if err != nil || a.State != v1.State_READY || a.Endpoint.GetPort() != 4022 || len(bound) != 1 || bound[0].Target.RuntimeId != "runtime-a" || reserves != 1 {
		t.Fatal(a, err, bound, reserves)
	}
	if a, err := s.RegisterSandbox(ctx, request("a")); err != nil || a.State != v1.State_READY || len(bound) != 1 {
		t.Fatal(a, err, bound)
	}
}

// A reserved sandbox that never starts is released like a registered one,
// and its ID stays closed.
func TestReleasedReservationClosesTheSandboxID(t *testing.T) {
	releases := 0
	s, _ := NewServer(Config{NewSandbox: func(string) Sandbox {
		return testSandbox{release: func(context.Context) ([]*v1.Artifact, error) { releases++; return nil, nil }}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if a, err := s.ReserveSandbox(ctx, reservation("a", "repeated-task")); err != nil || a.State != v1.State_RESERVED {
		t.Fatal(a, err)
	}
	if a, err := s.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: "a"}); err != nil || a.State != v1.State_RELEASED || releases != 1 {
		t.Fatal(a, err, releases)
	}
	if a, err := s.RegisterSandbox(ctx, request("a")); err != nil || a.State != v1.State_RELEASED {
		t.Fatal("release reopened", a, err)
	}
	if a, err := s.ReserveSandbox(ctx, reservation("a", "repeated-task")); err != nil || a.State != v1.State_RELEASED {
		t.Fatal("release reopened", a, err)
	}
}

// A failed bind releases the reserved access.
func TestFailedBindReleasesTheReservation(t *testing.T) {
	releases := 0
	s, _ := NewServer(Config{NewSandbox: func(string) Sandbox {
		return testSandbox{bind: func(context.Context, *v1.RegisterSandboxRequest) error { return errors.New("target missing") },
			release: func(context.Context) ([]*v1.Artifact, error) { releases++; return nil, nil }}
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.ReserveSandbox(ctx, reservation("a", "repeated-task")); err != nil {
		t.Fatal(err)
	}
	a, err := s.RegisterSandbox(ctx, request("a"))
	if err != nil || a.State == v1.State_READY || a.Error != "target missing" {
		t.Fatal(a, err)
	}
	if a, err := s.ReleaseSandbox(ctx, &v1.SandboxRequest{SandboxId: "a"}); err != nil || a.State != v1.State_RELEASED || releases != 1 {
		t.Fatal(a, err, releases)
	}
}
