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
	"time"

	sandboxv1 "github.com/hyscale-lab/aries/pkg/bridge/grpc/v1"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/replay"
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
	closeErr      error
	stagedTimeout time.Duration
}

func (runtime *fakeRuntime) UploadArchive(_ context.Context, _, _ string, source io.Reader) error {
	archive := tar.NewReader(source)
	header, err := archive.Next()
	if err != nil {
		return err
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
	return "owned-caller", runtime.createErr
}
func (runtime *fakeRuntime) Stop(ctx context.Context, _ string) error {
	runtime.stops++
	_, runtime.stopBounded = ctx.Deadline()
	return ctx.Err()
}
func (runtime *fakeRuntime) Close() error { return runtime.closeErr }

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
	closeErr := errors.New("runtime transport close failed")
	runtime := &fakeRuntime{createErr: deployment.ErrAllocationUnconfirmed, closeErr: closeErr}
	placement := core.RuntimePlacement{AttachmentID: "task-network"}
	caller, err := New(Options{Runtime: runtime, Image: "aries-bridge:v1", Backend: "docker", BridgeBackend: "docker", BridgePlacement: placement, CallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if err := caller.Start(context.Background(), core.ToolEndpoint{Protocol: "grpc", Address: "10.0.0.1:8443"}, core.HarnessConnectivity{Placement: placement}, "run", "task"); !errors.Is(err, deployment.ErrAllocationUnconfirmed) {
		t.Fatalf("partial create error = %v", err)
	}
	if err := caller.Stop(context.Background()); !errors.Is(err, deployment.ErrAllocationUnconfirmed) || !errors.Is(err, closeErr) || runtime.stops != 1 {
		t.Fatalf("partial allocation not removed or failures lost: %v, stops %d", err, runtime.stops)
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
