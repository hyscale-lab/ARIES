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
	release  func(context.Context) ([]*v1.Artifact, error)
}

func (s testSandbox) Register(ctx context.Context) (*v1.Endpoint, error) {
	if s.register != nil {
		return s.register(ctx)
	}
	return &v1.Endpoint{Port: 2222, Transport: "ssh", User: "aries"}, nil
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
	s, err := NewServer(Config{NewSandbox: func(r *v1.RegisterSandboxRequest) Sandbox {
		return testSandbox{release: func(ctx context.Context) ([]*v1.Artifact, error) {
			if r.SandboxId == "a" {
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
	s, _ := NewServer(Config{NewSandbox: func(*v1.RegisterSandboxRequest) Sandbox {
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
	s, _ := NewServer(Config{NewSandbox: func(*v1.RegisterSandboxRequest) Sandbox { starts++; return testSandbox{} }})
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
	s, _ := NewServer(Config{NewSandbox: func(r *v1.RegisterSandboxRequest) Sandbox {
		return testSandbox{register: func(context.Context) (*v1.Endpoint, error) {
			if r.SandboxId == "bad" {
				return nil, errors.New("admission failed")
			}
			return &v1.Endpoint{Port: 3333}, nil
		}, release: func(context.Context) ([]*v1.Artifact, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if r.SandboxId == "bad" && calls == 1 {
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
	s, _ := NewServer(Config{NewSandbox: func(*v1.RegisterSandboxRequest) Sandbox { return testSandbox{} }})
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
