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
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hyscale-lab/aries/pkg/bridge/control"
	v1 "github.com/hyscale-lab/aries/pkg/bridge/control/v1"
	bridgegrpc "github.com/hyscale-lab/aries/pkg/bridge/grpc"
	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/bridge/internal/bridgekit"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
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
	transport                      string
	artifacts                      []*v1.Artifact
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
	return &v1.Endpoint{Port: 3022, User: "aries", Transport: a.runtime.transport}, nil
}
func (a launchAccess) Release(context.Context) ([]*v1.Artifact, error) {
	r := a.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "release")
	if r.revokeErr != nil {
		return nil, r.revokeErr
	}
	return r.artifacts, nil
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
func evidenceManifest(names ...string) []*v1.Artifact {
	content := evidenceContent()
	sum := sha256.Sum256(content)
	var manifest []*v1.Artifact
	for _, name := range names {
		manifest = append(manifest, &v1.Artifact{Name: name, Status: "complete", Size: int64(len(content)), Sha256: hex.EncodeToString(sum[:])})
	}
	return manifest
}
func evidenceArchive(name string) io.ReadCloser {
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	_ = w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(evidenceContent()))})
	_, _ = w.Write(evidenceContent())
	_ = w.Close()
	return io.NopCloser(&b)
}
func (r *launchRuntime) DownloadArchive(_ context.Context, _ string, path string) (io.ReadCloser, deployment.FileInfo, error) {
	r.event("download")
	r.downloadSource = path
	return evidenceArchive(filepath.Base(path)), deployment.FileInfo{Size: int64(len(evidenceContent())), Mode: 0600}, nil
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
	return launchFixtureWith(t, backend, "hermes-ssh", false)
}
func launchFixtureWith(t *testing.T, backend, bridgeType string, retainRawLog bool) (*Service, *launchRuntime, launchSandbox) {
	t.Helper()
	runtime := &launchRuntime{transport: "ssh", artifacts: evidenceManifest("tool-calls.jsonl")}
	launch := LaunchSpec{RuntimeBackend: "service-fixture", ResourceMetrics: "unsupported", Request: deployment.Request{Image: "fixture", Workdir: "/private/bootstrap", Entrypoint: []string{"/fixture/bridge"}, ServicePort: 9443, Labels: map[string]string{"fixture.owner": "composition"}}, Config: LaunchConfig{Backend: backend, ControlAddress: "127.0.0.1:0", OutputDir: "/private/results"}}
	s, err := NewService(Options{Runtime: runtime, Launch: launch, RunID: "run", Placement: core.RuntimePlacement{AttachmentID: "fixture-run"}, OutputDir: t.TempDir(), BridgeType: bridgeType, RetainRawLog: retainRawLog})
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

// runTransports is the transport each bridge type gives its run.
var runTransports = map[string]string{"hermes-ssh": "ssh", "openclaw-ssh": "ssh", "hermes-grpc": "grpc"}

func TestSessionAcceptsOnlyTheRunTransport(t *testing.T) {
	for bridgeType, transport := range runTransports {
		for _, wire := range []string{"ssh", "grpc"} {
			t.Run(bridgeType+"/"+wire, func(t *testing.T) {
				s, r, sandbox := launchFixtureWith(t, "docker", bridgeType, false)
				r.transport = wire
				if err := s.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				b, err := s.NewSession(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				endpoint, err := b.Start(context.Background(), sandbox)
				if wire != transport {
					if err == nil {
						t.Fatal("foreign transport accepted")
					}
					return
				}
				if err != nil || endpoint.Protocol != transport {
					t.Fatal(endpoint, err)
				}
			})
		}
	}
}
func TestRawLogNameFollowsTransport(t *testing.T) {
	for bridgeType, rawLog := range map[string]string{"hermes-ssh": "ssh_raw.log", "hermes-grpc": "grpc_raw.log"} {
		for _, retain := range []bool{false, true} {
			s, r, sandbox := launchFixtureWith(t, "docker", bridgeType, retain)
			r.transport = runTransports[bridgeType]
			names := []string{"tool-calls.jsonl"}
			if retain {
				names = append(names, rawLog)
			}
			r.artifacts = evidenceManifest(names...)
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			session, endpoint := startSession(t, s, sandbox)
			var want []string
			for _, name := range names {
				want = append(want, filepath.Join(session.local, name))
			}
			if !reflect.DeepEqual(endpoint.LogPaths, want) {
				t.Fatal(endpoint.LogPaths)
			}
			if err := session.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !retain {
				continue
			}
			// A retained run requires the raw log in the manifest.
			r.artifacts = evidenceManifest("tool-calls.jsonl")
			sandbox.descriptor.SandboxID = "sandbox-b"
			later, _ := startSession(t, s, sandbox)
			if err := later.Stop(context.Background()); err == nil {
				t.Fatal("manifest without raw log accepted")
			}
		}
	}
}
func TestServiceRejectsBridgeTypeWithoutTransport(t *testing.T) {
	launch := LaunchSpec{RuntimeBackend: "service-fixture", ResourceMetrics: "unsupported", Request: deployment.Request{Workdir: "/private/bootstrap", ServicePort: 9443}, Config: LaunchConfig{Backend: "docker", ControlAddress: "127.0.0.1:0", OutputDir: "/private/results"}}
	if _, err := NewService(Options{Runtime: &launchRuntime{}, Launch: launch, RunID: "run", OutputDir: t.TempDir(), BridgeType: "fixture-ssh"}); err == nil {
		t.Fatal("bridge type without transport accepted")
	}
}

func TestServiceStagesReplayCaptureForBothBackends(t *testing.T) {
	for _, backend := range []string{"docker", "kubernetes"} {
		for _, capture := range []bool{false, true} {
			t.Run(backend+"/"+strconv.FormatBool(capture), func(t *testing.T) {
				service, runtime, _ := launchFixtureWith(t, backend, "hermes-grpc", false)
				service.options.CaptureReplay = capture
				if err := service.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				if runtime.config.CaptureReplay != capture {
					t.Fatalf("capture=%v, staged=%v", capture, runtime.config.CaptureReplay)
				}
			})
		}
	}
}

const sessionReplayCallCount = 1
const sessionReplayOutput = "/work\n"

// Only runtime deployment and sandbox execution are fake. Control, capture,
// completion, evidence collection and source loading use production code.
type sessionReplayRuntime struct {
	*launchRuntime
	backend sessionReplayBackend
	cancel  context.CancelFunc
	served  chan error
}

type sessionReplayBackend struct {
	bootstrapBackend
	commands chan core.Command
}

func (b sessionReplayBackend) ExecStream(_ context.Context, _ string, command core.Command, _ io.Reader, out, _ io.Writer) (core.CommandResult, error) {
	b.commands <- command
	_, err := io.WriteString(out, sessionReplayOutput)
	return core.CommandResult{}, err
}

func (r *sessionReplayRuntime) Start(context.Context, string) error {
	r.event("start")
	listener, err := net.Listen("tcp", r.config.ControlAddress)
	if err != nil {
		return err
	}
	r.controlAddress = listener.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.served = cancel, make(chan error, sessionReplayCallCount)
	go func() {
		r.served <- serve(ctx, ServeOptions{Config: r.config, Backend: r.backend, NewNative: func(id, output string) (NativeServer, error) {
			return bridgegrpc.New(bridgegrpc.Options{
				SandboxID: id, OutputDir: output, CaptureReplay: r.config.CaptureReplay, OmitRawLog: !r.config.RetainRawLog,
				ResolveListen: func(context.Context) (core.BridgeListen, error) {
					return core.BridgeListen{BindHost: "127.0.0.1"}, nil
				},
			})
		}}, listener)
	}()
	return nil
}

func (r *sessionReplayRuntime) TaskAddress(_ context.Context, _ string, port int) (string, error) {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
}

func (r *sessionReplayRuntime) DownloadArchive(_ context.Context, _ string, path string) (io.ReadCloser, deployment.FileInfo, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	var data bytes.Buffer
	archive := tar.NewWriter(&data)
	if err := archive.WriteHeader(&tar.Header{Name: filepath.Base(path), Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(content))}); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	_, err = archive.Write(content)
	if err = errors.Join(err, archive.Close()); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	return io.NopCloser(&data), deployment.FileInfo{Mode: 0600, Size: int64(len(content))}, nil
}

func (r *sessionReplayRuntime) Stop(ctx context.Context, _ string) error {
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	select {
	case err := <-r.served:
		r.cancel = nil
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSessionFinalizedRecordingLoadsForReplay(t *testing.T) {
	const logicalID = "catalog-task"
	const image = "task-image:tag"
	const benchmark = "terminalbench2"
	const script = "pwd"
	const score = 0.75
	const reward = 1
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	service, base, sandbox := launchFixtureWith(t, "docker", "hermes-grpc", false)
	runtime := &sessionReplayRuntime{launchRuntime: base, backend: sessionReplayBackend{commands: make(chan core.Command, sessionReplayCallCount)}}
	service.options.Runtime = runtime
	service.options.Launch.Config.OutputDir = t.TempDir()
	service.options.CaptureReplay = true
	service.options.BenchmarkType = benchmark
	service.options.BenchmarkRevisions = map[string]string{"revision": "pinned-revision"}
	if err := service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	bridge, err := service.NewSession(root)
	if err != nil {
		t.Fatal(err)
	}
	session := bridge.(*Session)
	session.RecordSource(logicalID, image)
	session.RecordTask(image)
	endpoint, err := session.Start(ctx, sandbox)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := grpc.NewClient(endpoint.Address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	response, err := sandboxv1.NewSandboxClient(connection).Exec(ctx, &sandboxv1.ExecRequest{Script: script})
	if err != nil || string(response.GetStdout()) != sessionReplayOutput {
		t.Fatalf("recorded Exec response = %+v, error = %v", response, err)
	}
	command := <-runtime.backend.commands
	if endpoint.Workdir != sandbox.descriptor.Workdir || command.Dir != endpoint.Workdir {
		t.Fatalf("Exec workdir = %q, endpoint = %+v, target = %+v", command.Dir, endpoint, sandbox.descriptor)
	}
	if err := session.CompleteHarness(ctx); err != nil {
		t.Fatal(err)
	}
	if err := session.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	metadataData, err := os.ReadFile(filepath.Join(session.local, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var metadata sourceMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(endpoint.LogPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	remoteLog, err := os.ReadFile(filepath.Join(service.options.Launch.Config.OutputDir, sandbox.descriptor.SandboxID, "tool-calls.jsonl"))
	if err != nil || !bytes.Equal(log, remoteLog) {
		t.Fatalf("collected audit differs from captured audit: %v", err)
	}
	digest := sha256.Sum256(log)
	if !metadata.ReplayReady || metadata.RunID != sandbox.descriptor.RunID || metadata.TaskID != logicalID || metadata.SandboxID != sandbox.descriptor.SandboxID || metadata.HarnessCompletedAt == "" || metadata.ToolCallsBytes == nil || *metadata.ToolCallsBytes != int64(len(log)) || metadata.ToolCallsSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("finalized recording metadata = %+v", metadata)
	}
	var record bridgekit.ToolCallRecord
	if err := json.Unmarshal(bytes.TrimSpace(log), &record); err != nil {
		t.Fatal(err)
	}
	if record.RunID != sandbox.descriptor.RunID || record.TaskID != sandbox.descriptor.TaskID || record.SandboxID != sandbox.descriptor.SandboxID || record.Workdir != endpoint.Workdir {
		t.Fatalf("captured call binding = %+v", record)
	}
	evaluation := core.Evaluation{Status: core.StatusSucceeded, Score: score, Reward: reward}
	result := core.RunResult{RunID: sandbox.descriptor.RunID, Tasks: []core.TaskResult{{TaskID: sandbox.descriptor.TaskID, Evaluation: evaluation}}}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run-result.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	task := core.Task{ID: logicalID, Environment: core.Environment{Image: image, Workdir: sandbox.descriptor.Workdir}}
	plan, err := bridgegrpc.LoadReplaySource(filepath.Join(root, sandbox.descriptor.TaskID), task, benchmark, service.options.BenchmarkRevisions)
	if err != nil {
		t.Fatalf("load unchanged production recording: %v", err)
	}
	if plan.SourceRunID != result.RunID || plan.SourceTaskID != sandbox.descriptor.TaskID || !reflect.DeepEqual(plan.Evaluation, evaluation) || len(plan.Calls) != sessionReplayCallCount {
		t.Fatalf("loaded production recording = %+v", plan)
	}
	call := plan.Calls[0]
	if call.Operation != "Exec" || call.Script != script || call.Workdir != endpoint.Workdir || call.ResponseCode != "OK" || call.ResponseSHA256 == "" || call.ResponseBytes <= 0 || !bytes.Equal(call.ResponseStdout, response.Stdout) {
		t.Fatalf("loaded captured Exec = %+v", call)
	}
}
