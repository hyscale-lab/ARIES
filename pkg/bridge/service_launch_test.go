package bridge

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"google.golang.org/grpc"
)

type launchSandbox struct {
	runner.Sandbox
	descriptor core.BridgeTarget
}

func (s launchSandbox) ExportBridgeTarget() (core.BridgeTarget, error) { return s.descriptor, nil }

type launchRuntime struct {
	deployment.Runtime
	mu                             sync.Mutex
	request                        deployment.Request
	config                         LaunchConfig
	files                          map[string][]byte
	events                         []string
	controlAddress, downloadSource string
	controlPort, harnessPort       int
	server                         *grpc.Server
	done                           chan struct{}
	uploadErr, revokeErr           error
}

func (r *launchRuntime) event(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, v)
}
func (r *launchRuntime) Create(_ context.Context, req deployment.Request) (string, error) {
	r.event("create")
	r.request = req
	return "service-fixture-123", nil
}
func (r *launchRuntime) UploadArchive(_ context.Context, _ string, _ string, input io.Reader) error {
	r.event("upload")
	if r.uploadErr != nil {
		return r.uploadErr
	}
	r.files = map[string][]byte{}
	a := tar.NewReader(input)
	for {
		h, err := a.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || h.Mode&0077 != 0 {
			return errors.New("private archive required")
		}
		r.files[h.Name], err = io.ReadAll(a)
		if err != nil {
			return err
		}
	}
	return json.Unmarshal(r.files["config.json"], &r.config)
}
func (r *launchRuntime) Validate(_ context.Context, id string, req deployment.Request, secrets [][]byte) error {
	r.event("validate")
	if id != "service-fixture-123" || !reflect.DeepEqual(req, r.request) || len(secrets) != 0 {
		return errors.New("launch validation differs")
	}
	return nil
}

type launchAccess struct{ runtime *launchRuntime }

func (a launchAccess) Register(context.Context) (*v1.Endpoint, error) {
	a.runtime.event("register")
	return &v1.Endpoint{Port: 3022, User: "aries", Transport: "ssh"}, nil
}
func (a launchAccess) Release(context.Context) ([]*v1.Artifact, error) {
	r := a.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "release")
	if r.revokeErr != nil {
		return nil, r.revokeErr
	}
	return evidenceManifest(), nil
}
func (r *launchRuntime) Start(context.Context, string) error {
	r.event("start")
	service, err := control.NewServer(control.Config{NewSandbox: func(*v1.RegisterSandboxRequest) control.Sandbox { return launchAccess{r} }})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", r.config.ControlAddress)
	if err != nil {
		return err
	}
	r.controlAddress = listener.Addr().String()
	r.server = grpc.NewServer()
	service.Register(r.server)
	r.done = make(chan struct{})
	go func() { defer close(r.done); _ = r.server.Serve(listener) }()
	return nil
}
func (r *launchRuntime) Address(_ context.Context, _ string, p int) (string, error) {
	r.controlPort = p
	return r.controlAddress, nil
}
func (r *launchRuntime) TaskAddress(_ context.Context, _ string, p int) (string, error) {
	r.harnessPort = p
	return "bridge.fixture:13022", nil
}
func (r *launchRuntime) Running(context.Context, string) (bool, error) { return true, nil }
func evidenceContent() []byte                                          { return []byte("{\"command\":\"fixture\"}\n") }
func evidenceManifest() []*v1.Artifact {
	content := evidenceContent()
	sum := sha256.Sum256(content)
	return []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "complete", Size: int64(len(content)), Sha256: hex.EncodeToString(sum[:])}}
}
func evidenceArchive() io.ReadCloser {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: "tool-calls.jsonl", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(evidenceContent()))})
	_, _ = w.Write(evidenceContent())
	_ = w.Close()
	return io.NopCloser(&b)
}
func (r *launchRuntime) DownloadArchive(_ context.Context, _ string, path string) (io.ReadCloser, deployment.FileInfo, error) {
	r.event("download")
	r.downloadSource = path
	return evidenceArchive(), deployment.FileInfo{Size: int64(len(evidenceContent())), Mode: 0600}, nil
}
func (r *launchRuntime) shutdown() {
	if r.server != nil {
		r.server.Stop()
		<-r.done
	}
}
func (r *launchRuntime) Stop(context.Context, string) error {
	r.event("stop")
	r.shutdown()
	return nil
}
func (r *launchRuntime) Close() error { r.event("close"); return nil }
func launchFixture(t *testing.T, backend string) (*Service, *launchRuntime, launchSandbox) {
	t.Helper()
	runtime := &launchRuntime{}
	launch := LaunchSpec{RuntimeBackend: "service-fixture", ResourceMetrics: "unsupported", Request: deployment.Request{Image: "fixture", Workdir: "/private/bootstrap", Entrypoint: []string{"/fixture/bridge"}, ServicePort: 9443, Labels: map[string]string{"fixture.owner": "composition"}}, Config: LaunchConfig{Backend: backend, ControlAddress: "127.0.0.1:0", OutputDir: "/private/results"}}
	s, err := NewService(Options{Runtime: runtime, Launch: launch, RunID: "run", Placement: core.RuntimePlacement{AttachmentID: "fixture-run"}, OutputDir: t.TempDir(), BridgeType: "fixture-ssh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.shutdown)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	sandbox := launchSandbox{descriptor: core.BridgeTarget{RunID: "run", TaskID: "task", SandboxID: "sandbox-a", Backend: backend, RuntimeID: "sandbox-runtime", Workdir: "/work"}}
	return s, runtime, sandbox
}
func startSession(t *testing.T, s *Service, sandbox runner.Sandbox) (*Session, core.ToolEndpoint) {
	t.Helper()
	bridge, err := s.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := bridge.(*Session)
	endpoint, err := session.Start(context.Background(), sandbox)
	if err != nil {
		t.Fatal(err)
	}
	return session, endpoint
}
func TestServiceUsesInjectedLaunchAndResolvedEndpoint(t *testing.T) {
	for _, backend := range []string{"docker", "sandbox-fixture"} {
		t.Run(backend, func(t *testing.T) {
			s, r, sandbox := launchFixture(t, backend)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			session, endpoint := startSession(t, s, sandbox)
			if r.request.Placement.AttachmentID != "fixture-run" || r.request.Labels["aries.task"] != "" || r.request.Labels["aries.attempt"] != "" || r.request.Labels["aries.run"] != "run" {
				t.Fatal(r.request)
			}
			if len(r.files) != 1 || len(r.files["config.json"]) == 0 {
				t.Fatal("credentials staged", r.files)
			}
			if r.controlPort != 9443 || r.harnessPort != 3022 || endpoint.Address != "bridge.fixture:13022" || endpoint.Workdir != "/work" {
				t.Fatal(endpoint, r.controlPort, r.harnessPort)
			}
			if err := session.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.downloadSource != "/private/results/sandbox-a/tool-calls.jsonl" {
				t.Fatal(r.downloadSource)
			}
			if !reflect.DeepEqual(r.events, []string{"create", "upload", "validate", "start", "register", "release", "download"}) {
				t.Fatal("session touched service ownership", r.events)
			}
			if _, err := os.Stat(filepath.Join(session.local, "runtime.json")); err != nil {
				t.Fatal(err)
			}
			// A later occurrence reuses this same control connection/runtime.
			sandbox.descriptor.SandboxID = "sandbox-b"
			sandbox.descriptor.RuntimeID = "runtime-b"
			later, _ := startSession(t, s, sandbox)
			if err := later.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := s.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if r.events[len(r.events)-2] != "stop" || r.events[len(r.events)-1] != "close" {
				t.Fatal(r.events)
			}
			if _, err := s.NewSession(t.TempDir()); err == nil {
				t.Fatal("closed service admitted session")
			}
		})
	}
}
func TestBootstrapFailureRetainsServiceCleanup(t *testing.T) {
	s, r, _ := launchFixture(t, "docker")
	r.uploadErr = errors.New("upload failed")
	if err := s.Start(context.Background()); !errors.Is(err, r.uploadErr) {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(r.events, []string{"create", "upload", "stop", "close"}) {
		t.Fatal(r.events)
	}
}
func TestSessionMismatchDoesNotRemoveSharedRuntime(t *testing.T) {
	s, r, sandbox := launchFixture(t, "docker")
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := s.NewSession(t.TempDir())
	sandbox.descriptor.Backend = "other"
	if _, err := b.Start(context.Background(), sandbox); err == nil {
		t.Fatal("mismatched backend accepted")
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.events) != 4 {
		t.Fatal(r.events)
	}
}
