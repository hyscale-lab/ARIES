package bridge

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/proto"
)

type launchSandbox struct {
	runner.Sandbox
	descriptor core.BridgeTarget
}

func (s launchSandbox) ExportBridgeTarget() (core.BridgeTarget, error) { return s.descriptor, nil }
func (launchSandbox) Connectivity() core.HarnessConnectivity {
	return core.HarnessConnectivity{Placement: core.RuntimePlacement{DockerNetwork: "fixture-task-attachment"}}
}

// launchRuntime models an independently deployed service. Bootstrap transfers stay
// in memory, and the real authenticated control protocol owns assignment/revocation.
// Its paths, commands, port mapping and metadata deliberately differ from Docker's.
type launchRuntime struct {
	deployment.Runtime
	request                  deployment.Request
	config                   LaunchConfig
	files                    map[string][]byte
	uploadDestination        string
	downloadSource           string
	controlPort, harnessPort int
	controlAddress           string
	uploadErr                error
	server                   *grpc.Server
	listener                 net.Listener
	done                     chan struct{}
	mu                       sync.Mutex
	events                   []string
	assignment               *v1.AssignSandboxRequest
	drainErr                 error
}

func (r *launchRuntime) event(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, name)
}
func (r *launchRuntime) Create(_ context.Context, request deployment.Request) (string, error) {
	r.event("create")
	r.request = request
	return "service-fixture-123", nil
}
func (r *launchRuntime) UploadArchive(_ context.Context, _ string, destination string, input io.Reader) error {
	r.event("upload")
	if r.uploadErr != nil {
		return r.uploadErr
	}
	r.uploadDestination = destination
	r.files = make(map[string][]byte)
	archive := tar.NewReader(input)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg || header.Mode&0077 != 0 {
			return errors.New("bootstrap archive must contain private regular files")
		}
		r.files[header.Name], err = io.ReadAll(archive)
		if err != nil {
			return err
		}
	}
	return json.Unmarshal(r.files["config.json"], &r.config)
}
func (r *launchRuntime) Validate(_ context.Context, id string, request deployment.Request, secrets [][]byte) error {
	r.event("validate")
	if id != "service-fixture-123" || !reflect.DeepEqual(request, r.request) || len(secrets) != 5 {
		return errors.New("validation lost runtime identity, launch request or credentials")
	}
	return nil
}
func (r *launchRuntime) Start(context.Context, string) error {
	r.event("start")
	tls, err := controlTLS(r.files["ca.pem"], r.files["server.pem"], r.files["server.key"], true)
	if err != nil {
		return err
	}
	host, err := ssh.ParsePrivateKey(r.files["host.key"])
	if err != nil {
		return err
	}
	service, err := control.NewServer(control.Config{
		InstanceID: r.config.InstanceID, Token: string(r.files["token"]),
		Assign: func(_ context.Context, request *v1.AssignSandboxRequest) (*v1.Endpoint, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, "assign")
			r.assignment = proto.Clone(request).(*v1.AssignSandboxRequest)
			return &v1.Endpoint{Host: r.config.Listen.AdvertiseHost, Port: uint32(r.config.Listen.AdvertisePort), User: "aries", Transport: "ssh", HostKey: string(ssh.MarshalAuthorizedKey(host.PublicKey()))}, nil
		},
		Revoke: func(context.Context) ([]*v1.Artifact, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.events = append(r.events, "drain")
			if r.drainErr != nil {
				return nil, r.drainErr
			}
			r.events = append(r.events, "finalize")
			content := launchEvidence()
			sum := sha256.Sum256(content)
			return []*v1.Artifact{{Name: "tool-calls.jsonl", Status: "complete", Size: int64(len(content)), Sha256: hex.EncodeToString(sum[:])}}, nil
		},
	})
	if err != nil {
		return err
	}
	r.listener, err = net.Listen("tcp", r.config.ControlAddress)
	if err != nil {
		return err
	}
	r.controlAddress = r.listener.Addr().String()
	r.server = grpc.NewServer(grpc.Creds(credentials.NewTLS(tls)), grpc.UnaryInterceptor(service.UnaryInterceptor), grpc.StreamInterceptor(service.StreamInterceptor))
	service.Register(r.server)
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		_ = r.server.Serve(r.listener)
	}()
	return nil
}
func (r *launchRuntime) Address(_ context.Context, _ string, port int) (string, error) {
	r.controlPort = port
	return r.controlAddress, nil
}
func (r *launchRuntime) HarnessAddress(_ context.Context, _ string, port int) (string, error) {
	r.harnessPort = port
	return net.JoinHostPort(r.config.Listen.AdvertiseHost, strconv.Itoa(r.config.Listen.AdvertisePort)), nil
}
func (*launchRuntime) Running(context.Context, string) (bool, error) { return true, nil }
func launchEvidence() []byte                                         { return []byte("{\"command\":\"fixture\"}\n") }
func (r *launchRuntime) DownloadArchive(_ context.Context, _ string, source string) (io.ReadCloser, deployment.FileInfo, error) {
	r.event("download")
	r.downloadSource = source
	content := launchEvidence()
	var buf bytes.Buffer
	archive := tar.NewWriter(&buf)
	if err := archive.WriteHeader(&tar.Header{Name: "tool-calls.jsonl", Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(content))}); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if _, err := archive.Write(content); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if err := archive.Close(); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	return io.NopCloser(&buf), deployment.FileInfo{Size: int64(len(content)), Mode: 0600}, nil
}
func (r *launchRuntime) shutdown() {
	if r.server != nil {
		r.server.Stop()
		<-r.done
	}
}
func (r *launchRuntime) Stop(_ context.Context, id string) error {
	if id != "service-fixture-123" {
		return fmt.Errorf("removing unexpected runtime %q", id)
	}
	r.event("stop")
	r.shutdown()
	return nil
}
func (r *launchRuntime) Close() error { r.event("close"); return nil }

func launchFixture(t *testing.T, sandboxBackend string) (*Manager, *launchRuntime, launchSandbox) {
	t.Helper()
	runtime := &launchRuntime{}
	launch := LaunchSpec{
		RuntimeBackend: "service-fixture", ResourceMetrics: "unsupported",
		Request: deployment.Request{Image: "fixture-artifact", Workdir: "/private/bootstrap", Entrypoint: []string{"/opt/fixture/bridge"}, Args: []string{"--config", "config.json"}, Env: []string{"FIXTURE=true"}, ServicePort: 9443, HarnessPort: 3022, Labels: map[string]string{"fixture.owner": "composition"}},
		Config:  LaunchConfig{Backend: sandboxBackend, ControlAddress: "127.0.0.1:0", OutputDir: "/private/results", Listen: core.BridgeListen{BindHost: "127.0.0.1", BindPort: 3022, AdvertiseHost: "bridge.fixture", AdvertisePort: 13022}},
	}
	m, err := New(Options{Runtime: runtime, Launch: launch, OutputDir: t.TempDir(), BridgeType: "hermes-ssh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(runtime.shutdown)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Stop(ctx)
	})
	sandbox := launchSandbox{descriptor: core.BridgeTarget{Version: 1, RunID: "run", TaskID: "task", OccurrenceID: "attempt", Backend: sandboxBackend, RuntimeID: "sandbox-runtime", RuntimeName: "sandbox", Workdir: "/workspace", MaxInputBytes: 16 << 20, MaxOutputBytes: 1 << 30, ExpectedLabels: map[string]string{"aries.managed": "true", "aries.component": "sandbox", "aries.kind": "task-container", "aries.run": "run", "aries.task": "task"}}}
	return m, runtime, sandbox
}

func TestManagerUsesInjectedRuntimeLaunch(t *testing.T) {
	// Bridge deployment and sandbox execution are independent choices. Both a
	// Docker target and a different target must work with this non-Docker runtime.
	for _, backend := range []string{"docker", "sandbox-fixture"} {
		t.Run(backend, func(t *testing.T) {
			m, runtime, sandbox := launchFixture(t, backend)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			endpoint, err := m.Start(ctx, sandbox)
			if err != nil {
				t.Fatal(err)
			}
			wantRequest := m.options.Launch.Request
			wantRequest.Name = runtime.request.Name
			wantRequest.Placement = sandbox.Connectivity().Placement
			wantRequest.Labels = map[string]string{"fixture.owner": "composition", "aries.managed": "true", "aries.component": "bridge", "aries.kind": "tool-bridge", "aries.run": "run", "aries.task": "task", "aries.attempt": "attempt"}
			if !reflect.DeepEqual(runtime.request, wantRequest) || runtime.request.Name == "" {
				t.Fatalf("runtime launch changed outside occurrence ownership: %+v", runtime.request)
			}
			if !reflect.DeepEqual(m.options.Launch.Request.Labels, map[string]string{"fixture.owner": "composition"}) {
				t.Fatal("manager mutated the wiring's launch labels")
			}
			wantConfig := m.options.Launch.Config
			wantConfig.InstanceID, wantConfig.BridgeType = m.instance, "hermes-ssh"
			if !reflect.DeepEqual(runtime.config, wantConfig) || runtime.uploadDestination != "/private/bootstrap" || runtime.controlPort != 9443 || runtime.harnessPort != 3022 {
				t.Fatalf("runtime configuration, staging or address mapping replaced: %+v", runtime.config)
			}
			if endpoint.Address != "bridge.fixture:13022" || endpoint.Workdir != "/workspace" {
				t.Fatalf("endpoint ignored independent task addressing: %+v", endpoint)
			}
			runtime.mu.Lock()
			assigned := proto.Clone(runtime.assignment).(*v1.AssignSandboxRequest)
			runtime.mu.Unlock()
			if assigned.InstanceId != m.instance || assigned.AssignmentId != m.assignment || !proto.Equal(assigned.Target, control.TargetToProto(sandbox.descriptor)) {
				t.Fatal("control assignment lost occurrence or sandbox identity")
			}
			metadata, err := os.ReadFile(filepath.Join(m.local, "runtime.json"))
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]string
			if err := json.Unmarshal(metadata, &record); err != nil {
				t.Fatal(err)
			}
			if record["Backend"] != "service-fixture" || record["ResourceMetrics"] != "unsupported" || record["RuntimeID"] != "service-fixture-123" {
				t.Fatalf("metadata misrepresented runtime or measurement support: %s", metadata)
			}
			// A real control response with an unconfirmed drain must retain the
			// runtime and evidence ownership; a later confirmed retry may collect.
			runtime.mu.Lock()
			runtime.drainErr = errors.New("fixture target drain not confirmed")
			runtime.mu.Unlock()
			if err := m.Stop(ctx); err == nil {
				t.Fatal("unconfirmed drain accepted")
			}
			runtime.mu.Lock()
			firstStop := append([]string(nil), runtime.events...)
			runtime.drainErr = nil
			runtime.mu.Unlock()
			if !reflect.DeepEqual(firstStop, []string{"create", "upload", "validate", "start", "assign", "drain"}) {
				t.Fatalf("unconfirmed drain collected or removed runtime: %v", firstStop)
			}
			if err := m.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(runtime.events, []string{"create", "upload", "validate", "start", "assign", "drain", "drain", "finalize", "download", "stop", "close"}) {
				t.Fatalf("cleanup ownership order changed: %v", runtime.events)
			}
			if runtime.downloadSource != "/private/results/task/bridge/tool-calls.jsonl" {
				t.Fatalf("collection ignored configured evidence root: %q", runtime.downloadSource)
			}
			if content, err := os.ReadFile(endpoint.LogPaths[0]); err != nil || !bytes.Equal(content, launchEvidence()) {
				t.Fatalf("finalized evidence missing or changed: %v", err)
			}
			if _, err := os.Stat(endpoint.IdentitySourceFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("client credential survived confirmed cleanup: %v", err)
			}
		})
	}
}

func TestManagerRejectsExecutionBackendMismatchBeforeAllocation(t *testing.T) {
	m, runtime, sandbox := launchFixture(t, "docker")
	sandbox.descriptor.Backend = "sandbox-fixture"
	if _, err := m.Start(context.Background(), sandbox); err == nil {
		t.Fatal("mismatched execution backend admitted")
	}
	if len(runtime.events) != 0 {
		t.Fatalf("backend mismatch allocated a runtime: %v", runtime.events)
	}
}

func TestManagerCleansInjectedRuntimeAfterBootstrapFailure(t *testing.T) {
	m, runtime, sandbox := launchFixture(t, "docker")
	runtime.uploadErr = errors.New("fixture private transfer failed")
	if _, err := m.Start(context.Background(), sandbox); !errors.Is(err, runtime.uploadErr) {
		t.Fatalf("bootstrap failure lost: %v", err)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtime.events, []string{"create", "upload", "stop", "close"}) {
		t.Fatalf("partial launch leaked or admitted runtime: %v", runtime.events)
	}
	if _, err := os.Stat(filepath.Join(m.local, "id_ed25519")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial launch left a private credential: %v", err)
	}
}
