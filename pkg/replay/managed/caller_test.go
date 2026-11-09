package managed

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/replay"
	"github.com/hyscale-lab/aries/pkg/runner"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

type fakeRuntime struct {
	deployment.Runtime
	request       deployment.Request
	creates       int
	stops         int
	stopBounded   bool
	createErr     error
	emptyID       bool
	stopErr       error
	closeErr      error
	stagedTimeout time.Duration
	files         map[string][]byte
	resultDelay   time.Duration
	resultReady   <-chan struct{}
	planUploads   int
}

func (runtime *fakeRuntime) UploadArchive(_ context.Context, _, _ string, source io.Reader) error {
	archive := tar.NewReader(source)
	header, err := archive.Next()
	if err != nil {
		return err
	}
	if runtime.files != nil {
		data, err := io.ReadAll(archive)
		if err != nil {
			return err
		}
		runtime.files[header.Name] = data
		if header.Name == planFile {
			runtime.planUploads++
		}
		if header.Name == planReadyFile {
			runtime.files[resultFile] = []byte(`{"done":true}`)
		}
		return nil
	}
	if header.Name == launchFile {
		data, err := io.ReadAll(archive)
		if err != nil {
			return err
		}
		var launch childLaunch
		if err := json.Unmarshal(data, &launch); err != nil {
			return err
		}
		runtime.stagedTimeout = launch.CallTimeout
	}
	return errors.New("staging failed")
}

func (runtime *fakeRuntime) Create(_ context.Context, request deployment.Request) (string, error) {
	runtime.creates++
	runtime.request = request
	if runtime.emptyID {
		return "", runtime.createErr
	}
	return "owned-caller", runtime.createErr
}
func (runtime *fakeRuntime) Stop(ctx context.Context, _ string) error {
	runtime.stops++
	_, runtime.stopBounded = ctx.Deadline()
	return errors.Join(runtime.stopErr, ctx.Err())
}
func (runtime *fakeRuntime) Close() error { return runtime.closeErr }

func (runtime *fakeRuntime) Validate(context.Context, string, deployment.Request, [][]byte) error {
	return nil
}
func (runtime *fakeRuntime) Start(context.Context, string) error {
	runtime.files[readyFile] = nil
	return nil
}
func (runtime *fakeRuntime) Running(ctx context.Context, _ string) (bool, error) {
	return true, ctx.Err()
}
func (runtime *fakeRuntime) DownloadArchive(ctx context.Context, _, name string) (io.ReadCloser, deployment.FileInfo, error) {
	name = filepath.Base(name)
	data, ok := runtime.files[name]
	if !ok {
		return nil, deployment.FileInfo{}, runner.ErrNotFound
	}
	if name == resultFile {
		if runtime.resultReady != nil {
			select {
			case <-runtime.resultReady:
			case <-ctx.Done():
				return nil, deployment.FileInfo{}, ctx.Err()
			}
		}
		timer := time.NewTimer(runtime.resultDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, deployment.FileInfo{}, ctx.Err()
		}
	}
	var buffer bytes.Buffer
	archive := tar.NewWriter(&buffer)
	if err := archive.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(data))}); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if _, err := archive.Write(data); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if err := archive.Close(); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	return io.NopCloser(&buffer), deployment.FileInfo{Mode: 0600, Size: int64(len(data))}, nil
}

func TestReplayCallerAllowsChildStartupAndResultTransfer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const callTimeout = time.Second
		plan := replay.Plan{Calls: []replay.Call{{StartOffset: callTimeout}}, FinalWait: callTimeout}
		const childBudget = callTimeout + callTimeout + callTimeout
		const startupDelay = pollInterval
		const transferDelay = pollInterval
		runtime := &fakeRuntime{files: make(map[string][]byte), resultDelay: startupDelay + childBudget + transferDelay}
		placement := core.RuntimePlacement{AttachmentID: "task-network"}
		caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: callTimeout})
		if err != nil {
			t.Fatal(err)
		}
		if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := caller.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		}()
		if err := caller.Run(context.Background(), plan); err != nil {
			t.Fatalf("child execution lost startup or result allowance: %v", err)
		}
	})
}

func TestReplayCallerRejectsRepeatedRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const onePlan = 1
		release := make(chan struct{})
		runtime := &fakeRuntime{files: make(map[string][]byte), resultReady: release}
		placement := core.RuntimePlacement{AttachmentID: "task-network"}
		caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := make(chan error, onePlan)
		go func() { first <- caller.Run(ctx, replay.Plan{}) }()
		synctest.Wait()
		second := make(chan error, onePlan)
		go func() { second <- caller.Run(ctx, replay.Plan{}) }()
		synctest.Wait()
		select {
		case err := <-second:
			if err == nil || !strings.Contains(err.Error(), "already run") {
				t.Fatalf("concurrent run not rejected: %v", err)
			}
		default:
			cancel()
			t.Fatal("concurrent run waited for the first plan's result")
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatalf("first run failed: %v", err)
		}
		if err := caller.Run(ctx, replay.Plan{FinalWait: time.Second}); err == nil || !strings.Contains(err.Error(), "already run") {
			t.Fatalf("later run reused a stale result: %v", err)
		}
		if runtime.planUploads != onePlan {
			t.Fatalf("plan uploads = %d, want %d", runtime.planUploads, onePlan)
		}
		if err := caller.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReplayCallerRejectsInvalidDeadline(t *testing.T) {
	const maxDuration = time.Duration(1<<63 - 1)
	for _, test := range []struct {
		name    string
		timeout time.Duration
		plan    replay.Plan
	}{
		{name: "negative final wait", timeout: time.Second, plan: replay.Plan{FinalWait: -time.Second}},
		{name: "execution overflow", timeout: maxDuration, plan: replay.Plan{FinalWait: time.Second}},
		{name: "offset overflow", timeout: time.Second, plan: replay.Plan{Calls: []replay.Call{{StartOffset: maxDuration}}}},
		{name: "headroom overflow", timeout: maxDuration - startTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeRuntime{files: make(map[string][]byte)}
			placement := core.RuntimePlacement{AttachmentID: "task-network"}
			caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: test.timeout})
			if err != nil {
				t.Fatal(err)
			}
			if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); err != nil {
				t.Fatal(err)
			}
			if err := caller.Run(context.Background(), test.plan); err == nil || !strings.Contains(err.Error(), "schedule") {
				t.Fatalf("invalid replay deadline not rejected before execution: %v", err)
			}
			if err := caller.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplayChildConfirmsBridgeReachabilityBeforePlan(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := grpc.NewServer()
	go func() { _ = service.Serve(listener) }()
	t.Cleanup(service.Stop)

	folder := t.TempDir()
	launch, err := json.Marshal(childLaunch{Endpoint: listener.Addr().String(), CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, launchFile), launch, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunChild(ctx, filepath.Join(folder, launchFile)) }()
	for {
		if _, err := os.Stat(filepath.Join(folder, readyFile)); err == nil {
			break
		}
		if err := poll(ctx); err != nil {
			t.Fatalf("bridge readiness not confirmed: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(folder, planFile), []byte(`{"calls":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, planReadyFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if result, err := os.ReadFile(filepath.Join(folder, resultFile)); err == nil {
			var outcome childResult
			if err := json.Unmarshal(result, &outcome); err != nil || !outcome.Done || outcome.Error != "" {
				t.Fatalf("empty replay result = %s, %v", result, err)
			}
			break
		}
		if err := poll(ctx); err != nil {
			t.Fatalf("replay child did not finish: %v", err)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("replay child shutdown: %v", err)
	}
}

// A local gRPC call and private staging must complete before this test deadline.
const childTestDeadline = 5 * time.Second

type largeExecServer struct {
	sandboxv1.UnimplementedSandboxServer
	response *sandboxv1.ExecResponse
}

func (server *largeExecServer) Exec(context.Context, *sandboxv1.ExecRequest) (*sandboxv1.ExecResponse, error) {
	return server.response, nil
}

func TestReplayChildReceivesLargeExecReply(t *testing.T) {
	const grpcDefaultReplyBytes = 4 << 20 // grpc-go rejects larger replies without a receive option.
	response := &sandboxv1.ExecResponse{Stdout: bytes.Repeat([]byte("x"), grpcDefaultReplyBytes+1)}
	encoded, err := (proto.MarshalOptions{Deterministic: true}).Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.New()
	var responseBytes int64
	for _, field := range [][]byte{[]byte("aries.grpc.outcome.v1"), []byte(sandboxv1.Sandbox_Exec_FullMethodName), encoded, []byte(codes.OK.String()), nil} {
		var prefix [binary.MaxVarintLen64]byte
		length := binary.PutUvarint(prefix[:], uint64(len(field)))
		_, _ = fingerprint.Write(prefix[:length])
		_, _ = fingerprint.Write(field)
		responseBytes += int64(length + len(field))
	}
	plan := replay.Plan{Calls: []replay.Call{{Operation: "Exec", Script: "true", ResponseCode: codes.OK.String(), ResponseSHA256: hex.EncodeToString(fingerprint.Sum(nil)), ResponseBytes: responseBytes}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := grpc.NewServer()
	sandboxv1.RegisterSandboxServer(service, &largeExecServer{response: response})
	go func() { _ = service.Serve(listener) }()
	t.Cleanup(service.Stop)
	folder := t.TempDir()
	launch, _ := json.Marshal(childLaunch{Endpoint: listener.Addr().String(), CallTimeout: childTestDeadline})
	if err := os.WriteFile(filepath.Join(folder, launchFile), launch, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), childTestDeadline)
	done := make(chan error, 1)
	go func() { done <- RunChild(ctx, filepath.Join(folder, launchFile)) }()
	t.Cleanup(func() { cancel(); <-done })
	for {
		if _, err := os.Stat(filepath.Join(folder, readyFile)); err == nil {
			break
		}
		if err := poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, planFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, planReadyFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(folder, resultFile)); err == nil {
			var outcome childResult
			if err := json.Unmarshal(data, &outcome); err != nil || !outcome.Done || outcome.Error != "" {
				t.Fatalf("large Exec replay = %s, %v", data, err)
			}
			return
		}
		if err := poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplayChildWaitsForCompletePlanUpload(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := grpc.NewServer()
	go func() { _ = service.Serve(listener) }()
	t.Cleanup(service.Stop)
	folder := t.TempDir()
	launch, _ := json.Marshal(childLaunch{Endpoint: listener.Addr().String(), CallTimeout: childTestDeadline})
	if err := os.WriteFile(filepath.Join(folder, launchFile), launch, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), childTestDeadline)
	done := make(chan error, 1)
	go func() { done <- RunChild(ctx, filepath.Join(folder, launchFile)) }()
	t.Cleanup(func() { cancel(); <-done })
	for {
		if _, err := os.Stat(filepath.Join(folder, readyFile)); err == nil {
			break
		}
		if err := poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	complete := []byte(`{"calls":[]}`)
	partial := complete[:len(complete)/2]
	planPath := filepath.Join(folder, planFile)
	if err := os.WriteFile(planPath, partial, 0600); err != nil {
		t.Fatal(err)
	}
	observation := time.NewTimer(time.Second)
	defer observation.Stop()
	for {
		if data, err := os.ReadFile(filepath.Join(folder, resultFile)); err == nil {
			t.Fatalf("caller read partial plan before publication: %s", data)
		}
		select {
		case <-observation.C:
			goto completed
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(pollInterval):
		}
	}
completed:
	file, err := os.OpenFile(planPath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(complete[len(partial):]); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, planReadyFile), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(folder, resultFile)); err == nil {
			var outcome childResult
			if err := json.Unmarshal(data, &outcome); err != nil || !outcome.Done || outcome.Error != "" {
				t.Fatalf("complete plan replay = %s, %v", data, err)
			}
			return
		}
		if err := poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReplayChildRejectsUnreachableBridgeBeforeReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	folder := t.TempDir()
	launch, _ := json.Marshal(childLaunch{Endpoint: address, CallTimeout: time.Second})
	if err := os.WriteFile(filepath.Join(folder, launchFile), launch, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- RunChild(ctx, filepath.Join(folder, launchFile)) }()
	<-ctx.Done()
	if err := <-done; err != nil {
		t.Fatalf("unreachable bridge child: %v", err)
	}
	if _, err := os.Stat(filepath.Join(folder, readyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("caller reported readiness without private bridge access")
	}
	result, err := os.ReadFile(filepath.Join(folder, resultFile))
	if err != nil || !strings.Contains(string(result), "deadline exceeded") {
		t.Fatalf("unreachable bridge was not reported: %s, %v", result, err)
	}
}

func TestReplayCallerUsesLiveTimeoutAndRejectsNegativeTimeout(t *testing.T) {
	placement := core.RuntimePlacement{AttachmentID: "task-network"}
	positive := harness.DefaultAgentTimeout + time.Minute
	for _, test := range []struct {
		name        string
		given, want time.Duration
		reject      bool
	}{
		{name: "zero uses live default", given: 0, want: harness.DefaultAgentTimeout},
		{name: "positive stays unchanged", given: positive, want: positive},
		{name: "negative is invalid", given: -time.Second, reject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeRuntime{}
			caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: test.given})
			if test.reject {
				if err == nil || !strings.Contains(err.Error(), "timeout") || runtime.creates != 0 {
					t.Fatalf("negative call timeout accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); err == nil || runtime.stagedTimeout != test.want {
				t.Fatalf("staged call timeout = %s, want %s; startup = %v", runtime.stagedTimeout, test.want, err)
			}
			if err := caller.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReplayCallerRejectsDifferentBridgeBackendOrTaskAttachment(t *testing.T) {
	attachment := core.RuntimePlacement{AttachmentID: "task-network"}
	for _, test := range []struct {
		name, backend   string
		bridgePlacement core.RuntimePlacement
	}{
		{name: "backend", backend: "kubernetes", bridgePlacement: attachment},
		{name: "task attachment", backend: "docker", bridgePlacement: core.RuntimePlacement{AttachmentID: "another-network"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeRuntime{}
			caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: test.backend, BridgeBackend: "docker", BridgePlacement: test.bridgePlacement, CallTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			err = caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: attachment}, "run", "task")
			if err == nil || !strings.Contains(err.Error(), "bridge") {
				t.Fatalf("incompatible route accepted: %v", err)
			}
			if runtime.creates != 0 {
				t.Fatal("allocated caller before the route check")
			}
		})
	}
}

func TestReplayCallerRemovesKnownIdentityAfterPartialCreate(t *testing.T) {
	const oneRemoval = 1
	closeErr := errors.New("runtime transport close failed")
	stopErr := errors.New("runtime removal unconfirmed")
	for _, test := range []struct {
		name      string
		emptyID   bool
		stopErr   error
		closeErr  error
		uncertain bool
	}{
		{name: "owned removal clears uncertainty"},
		{name: "owned removal preserves close error", closeErr: closeErr},
		{name: "unknown identity stays uncertain", emptyID: true, uncertain: true},
		{name: "failed removal stays uncertain", stopErr: stopErr, uncertain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &fakeRuntime{createErr: deployment.ErrAllocationUnconfirmed, emptyID: test.emptyID, stopErr: test.stopErr, closeErr: test.closeErr}
			placement := core.RuntimePlacement{AttachmentID: "task-network"}
			caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); !errors.Is(err, deployment.ErrAllocationUnconfirmed) {
				t.Fatalf("partial create error = %v", err)
			}
			err = caller.Stop(context.Background())
			if errors.Is(err, deployment.ErrAllocationUnconfirmed) != test.uncertain || test.closeErr != nil && !errors.Is(err, test.closeErr) || test.stopErr != nil && !errors.Is(err, test.stopErr) {
				t.Fatalf("cleanup errors changed: %v", err)
			}
			wantStops := oneRemoval
			if test.emptyID {
				wantStops = 0
			}
			if runtime.stops != wantStops {
				t.Fatalf("removals = %d, want %d", runtime.stops, wantStops)
			}
		})
	}
}

func TestReplayCallerUsesPrivateTaskAttachmentAndStopsAfterStagingFailure(t *testing.T) {
	runtime := &fakeRuntime{}
	placement := core.RuntimePlacement{AttachmentID: "task-network"}
	caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); err == nil {
		t.Fatal("staging failure was not reported")
	}
	request := runtime.request
	if runtime.creates != 1 || request.Image != "aries-bridge:v1" || request.Placement != placement || request.ServicePort != 0 || request.InternalPort != 0 || len(request.Mounts) != 0 || len(request.Env) != 0 || request.NoNewPrivileges || !request.DisableServiceAccountToken {
		t.Fatalf("caller request differs from the private live harness defaults: %+v", request)
	}
	if err := caller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := caller.Stop(context.Background()); err != nil || runtime.stops != 1 || !runtime.stopBounded {
		t.Fatalf("stop not idempotent or bounded: %v, stops %d, bounded %t", err, runtime.stops, runtime.stopBounded)
	}
}
