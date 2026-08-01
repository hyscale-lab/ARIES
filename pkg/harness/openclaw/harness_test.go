package openclaw

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	audioinput "github.com/hyscale-lab/aries/pkg/audio"
	"github.com/hyscale-lab/aries/pkg/core"
	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
	realtimeclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/realtime"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

const testOpenClawImage = "ghcr.io/openclaw/openclaw:2026.7.1"

type fakeDocker struct {
	mu               sync.Mutex
	created          client.ContainerCreateOptions
	container        container.InspectResponse
	archive          []byte
	execs            map[string]client.ExecCreateOptions
	exitCodes        map[string]int
	execRunning      map[string]bool
	execPresent      map[string]bool
	removed          bool
	copyToErr        error
	containerLogsErr error
	copyFromErr      error
	inspectErr       error
	stopErr          error
	killErr          error
	removeErr        error
	keepAttachOpen   bool
	nextExec         int
	startCalls       int
	logsCalls        int
	stopCalls        int
	killCalls        int
	removeCalls      int
	createCalls      int
	closeCalls       int
	closeErr         error
}

func (fake *fakeDocker) Close() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.closeCalls++
	return fake.closeErr
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	closeFailure := errors.New("close failed")
	fake := newFakeDocker()
	fake.closeErr = closeFailure
	manager := &Manager{client: fake}
	if err := manager.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, closeFailure) {
		t.Fatalf("error = %v", err)
	}
	if fake.closeCalls != 1 {
		t.Fatalf("close calls = %d", fake.closeCalls)
	}
}

func TestHarnessAppliesOnlyPresentCheckedResources(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	cpu, memory := 2.5, 1536
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), CPU: &cpu, MemoryMB: &memory}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := fake.created.HostConfig.Resources; got.NanoCPUs != 2_500_000_000 || got.Memory != 1536<<20 {
		t.Fatalf("resources = %#v", got)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestHarnessRejectsInvalidResourcesBeforeContainerCreate(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*core.HarnessRequest)
		want string
	}{
		{"cpu", func(request *core.HarnessRequest) { request.CPU = floatPointer(math.Exp2(63) / 1e9) }, "OpenClaw CPU must be finite, positive, and convert to NanoCPUs below 2^63"},
		{"memory", func(request *core.HarnessRequest) { request.MemoryMB = intPointer(int(math.MaxInt64>>20) + 1) }, fmt.Sprintf("OpenClaw memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := newTestManager(t, fake, []byte("model-secret"))
			request := core.HarnessRequest{
				RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(),
			}
			test.set(&request)
			if err := manager.Start(context.Background(), request); err == nil || err.Error() != test.want {
				t.Fatalf("Start() error = %v, want exactly %q", err, test.want)
			}
			if fake.createCalls != 0 {
				t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
			}
		})
	}
}

func floatPointer(value float64) *float64 { return &value }
func intPointer(value int) *int           { return &value }

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		execs: make(map[string]client.ExecCreateOptions), exitCodes: make(map[string]int),
		execRunning: make(map[string]bool), execPresent: make(map[string]bool),
	}
}

func (fake *fakeDocker) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.createCalls++
	fake.created = options
	fake.container = container.InspectResponse{
		ID: "openclaw-id", Config: options.Config, HostConfig: options.HostConfig,
		State:           &container.State{},
		NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{}},
	}
	for port, bindings := range options.HostConfig.PortBindings {
		copied := append([]network.PortBinding(nil), bindings...)
		for index := range copied {
			if copied[index].HostPort == "" {
				copied[index].HostPort = "38089"
			}
			if !copied[index].HostIP.IsValid() {
				copied[index].HostIP = netip.MustParseAddr("127.0.0.1")
			}
		}
		fake.container.NetworkSettings.Ports[port] = copied
	}
	return client.ContainerCreateResult{ID: "openclaw-id"}, nil
}

func (fake *fakeDocker) CopyToContainer(_ context.Context, id string, options client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if id != "openclaw-id" || options.DestinationPath != "/" || !options.CopyUIDGID {
		return client.CopyToContainerResult{}, errors.New("unexpected copy request")
	}
	if fake.copyToErr != nil {
		return client.CopyToContainerResult{}, fake.copyToErr
	}
	content, err := io.ReadAll(options.Content)
	if err != nil {
		return client.CopyToContainerResult{}, err
	}
	fake.mu.Lock()
	fake.archive = content
	fake.mu.Unlock()
	return client.CopyToContainerResult{}, nil
}

func (fake *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if id != fake.container.ID || fake.removed {
		return client.ContainerStartResult{}, errdefs.ErrNotFound
	}
	fake.startCalls++
	fake.container.State.Running = true
	return client.ContainerStartResult{}, nil
}

func (fake *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.removed || id != fake.container.ID {
		return client.ContainerInspectResult{}, fmt.Errorf("container absent: %w", errdefs.ErrNotFound)
	}
	if fake.inspectErr != nil {
		return client.ContainerInspectResult{}, fake.inspectErr
	}
	return client.ContainerInspectResult{Container: fake.container}, nil
}

func (fake *fakeDocker) ContainerTop(context.Context, string, client.ContainerTopOptions) (client.ContainerTopResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	processes := make([][]string, 0)
	for _, present := range fake.execPresent {
		if present {
			processes = append(processes, []string{"123"})
		}
	}
	return client.ContainerTopResult{Titles: []string{"PID"}, Processes: processes}, nil
}

func (fake *fakeDocker) ExecCreate(_ context.Context, id string, options client.ExecCreateOptions) (client.ExecCreateResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if id != fake.container.ID || !fake.container.State.Running {
		return client.ExecCreateResult{}, errdefs.ErrNotFound
	}
	fake.nextExec++
	execID := fmt.Sprintf("exec-%d", fake.nextExec)
	fake.execs[execID] = options
	fake.execRunning[execID] = true
	fake.execPresent[execID] = true
	return client.ExecCreateResult{ID: execID}, nil
}

func (fake *fakeDocker) ExecAttach(_ context.Context, execID string, _ client.ExecAttachOptions) (client.ExecAttachResult, error) {
	fake.mu.Lock()
	options, ok := fake.execs[execID]
	fake.mu.Unlock()
	if !ok {
		return client.ExecAttachResult{}, errdefs.ErrNotFound
	}
	clientSide, engineSide := net.Pipe()
	response := client.ExecAttachResult{HijackedResponse: client.NewHijackedResponse(clientSide, "application/vnd.docker.multiplexed-stream")}
	go func() {
		defer engineSide.Close()
		exitCode := 0
		if len(options.Cmd) > 5 && options.Cmd[5] == launcherPath {
			_ = writeMux(engineSide, stdcopy.Stdout, []byte(`{"status":"ok","result":{"payloads":[{"text":"task complete"}]}}`))
			_ = writeMux(engineSide, stdcopy.Stderr, []byte("agent diagnostic\n"))
		}
		if len(options.Cmd) > 4 {
			trailer := fmt.Sprintf("\x1eARIES_OPENCLAW_EXIT_%s=%d\x1f", options.Cmd[4], exitCode)
			_ = writeMux(engineSide, stdcopy.Stderr, []byte(trailer))
		}
		fake.mu.Lock()
		fake.exitCodes[execID] = exitCode
		fake.execPresent[execID] = false
		if !fake.keepAttachOpen {
			fake.execRunning[execID] = false
		}
		fake.mu.Unlock()
		if fake.keepAttachOpen {
			_, _ = io.Copy(io.Discard, engineSide)
			fake.mu.Lock()
			fake.execRunning[execID] = false
			fake.mu.Unlock()
		}
	}()
	return response, nil
}

func (fake *fakeDocker) ExecInspect(_ context.Context, execID string, _ client.ExecInspectOptions) (client.ExecInspectResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	status := fake.exitCodes[execID]
	pid := 0
	if fake.execRunning[execID] {
		pid = 123
	}
	return client.ExecInspectResult{ID: execID, ContainerID: fake.container.ID, Running: fake.execRunning[execID], PID: pid, ExitCode: status}, nil
}

func (fake *fakeDocker) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	fake.mu.Lock()
	fake.logsCalls++
	err := fake.containerLogsErr
	fake.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(multiplexed([]byte("gateway ready\n"), nil))), nil
}

func (fake *fakeDocker) CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error) {
	if fake.copyFromErr != nil {
		return client.CopyFromContainerResult{}, fake.copyFromErr
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("{\"event\":\"tool\"}\n")
	_ = writer.WriteHeader(&tar.Header{Name: "sessions/run.trajectory.jsonl", Mode: 0o600, Size: int64(len(content))})
	_, _ = writer.Write(content)
	_ = writer.Close()
	return client.CopyFromContainerResult{Content: io.NopCloser(bytes.NewReader(archive.Bytes()))}, nil
}

func TestCollectTelemetryIgnoresOnlyTypedMissingPath(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	active := &session{containerID: "openclaw-id", artifactDir: t.TempDir()}

	fake.copyFromErr = fmt.Errorf("session path absent: %w", errdefs.ErrNotFound)
	if paths, err := manager.collectTelemetry(context.Background(), active); err != nil || len(paths) != 0 {
		t.Fatalf("typed missing telemetry = %v, %v", paths, err)
	}

	fake.copyFromErr = errors.New("daemon not found while unavailable")
	if _, err := manager.collectTelemetry(context.Background(), active); err == nil || !strings.Contains(err.Error(), "daemon not found") {
		t.Fatalf("untyped daemon failure was masked: %v", err)
	}
}

func (fake *fakeDocker) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.stopCalls++
	if fake.stopErr != nil {
		return client.ContainerStopResult{}, fake.stopErr
	}
	fake.container.State.Running = false
	return client.ContainerStopResult{}, nil
}

func (fake *fakeDocker) ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.killCalls++
	if fake.killErr != nil {
		return client.ContainerKillResult{}, fake.killErr
	}
	fake.container.State.Running = false
	return client.ContainerKillResult{}, nil
}

func (fake *fakeDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.removeCalls++
	if fake.removeErr != nil {
		return client.ContainerRemoveResult{}, fake.removeErr
	}
	fake.removed = true
	return client.ContainerRemoveResult{}, nil
}

func multiplexed(stdout, stderr []byte) []byte {
	var content bytes.Buffer
	_ = writeMux(&content, stdcopy.Stdout, stdout)
	_ = writeMux(&content, stdcopy.Stderr, stderr)
	return content.Bytes()
}

func writeMux(writer io.Writer, stream stdcopy.StdType, content []byte) error {
	if len(content) == 0 {
		return nil
	}
	var header [8]byte
	header[0] = byte(stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(content)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err := writer.Write(content)
	return err
}

func newTestManager(t *testing.T, fake *fakeDocker, secret []byte) *Manager {
	t.Helper()
	manager, err := New(Options{
		Image: testOpenClawImage, OutputDir: t.TempDir(), StartTimeout: time.Second, AgentTimeout: time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return secret, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.client = fake
	manager.newID = func() (string, error) { return "attempt", nil }
	manager.newGateway = func(string, []byte) (gatewayConnection, error) { return &stubGateway{}, nil }
	return manager
}

type stubGateway struct{}

func (stubGateway) Connect(context.Context, gatewayclient.ConnectOptions) (gatewayclient.ConnectSummary, error) {
	return gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}, nil
}

func (stubGateway) Agent(context.Context, gatewayclient.AgentRequest) (gatewayclient.AgentResult, error) {
	return gatewayclient.AgentResult{RunID: "run-agent", Text: "task complete"}, nil
}

func (stubGateway) Call(context.Context, string, map[string]any) (gatewayclient.Frame, error) {
	return nil, nil
}

func (stubGateway) RecvEvent(context.Context) (gatewayclient.Frame, error) {
	return nil, context.Canceled
}

func (stubGateway) FatalError() error { return nil }

func (stubGateway) Close() error {
	return nil
}

type recordingGateway struct {
	summary    gatewayclient.ConnectSummary
	agentCalls int
	request    gatewayclient.AgentRequest
}

type secretAgentGateway struct {
	summary gatewayclient.ConnectSummary
	result  gatewayclient.AgentResult
	err     error
}

func (gateway *secretAgentGateway) Connect(context.Context, gatewayclient.ConnectOptions) (gatewayclient.ConnectSummary, error) {
	return gateway.summary, nil
}
func (gateway *secretAgentGateway) Agent(context.Context, gatewayclient.AgentRequest) (gatewayclient.AgentResult, error) {
	return gateway.result, gateway.err
}
func (*secretAgentGateway) Call(context.Context, string, map[string]any) (gatewayclient.Frame, error) {
	return nil, errors.New("unexpected realtime call")
}
func (*secretAgentGateway) RecvEvent(context.Context) (gatewayclient.Frame, error) {
	return nil, errors.New("unexpected realtime event")
}
func (*secretAgentGateway) FatalError() error { return nil }
func (*secretAgentGateway) Close() error      { return nil }

func (gateway *recordingGateway) Connect(context.Context, gatewayclient.ConnectOptions) (gatewayclient.ConnectSummary, error) {
	return gateway.summary, nil
}

func (gateway *recordingGateway) Agent(_ context.Context, request gatewayclient.AgentRequest) (gatewayclient.AgentResult, error) {
	gateway.agentCalls++
	gateway.request = request
	return gatewayclient.AgentResult{RunID: "run-1", Text: "first\nsecond"}, nil
}

func (*recordingGateway) Call(context.Context, string, map[string]any) (gatewayclient.Frame, error) {
	return nil, errors.New("unexpected realtime call")
}

func (*recordingGateway) RecvEvent(context.Context) (gatewayclient.Frame, error) {
	return nil, errors.New("unexpected realtime event")
}

func (*recordingGateway) FatalError() error { return nil }

func (*recordingGateway) Close() error { return nil }

type stubRunner struct {
	result realtimeclient.Result
	err    error
}

func (runner stubRunner) Run(context.Context) (realtimeclient.Result, error) {
	return runner.result, runner.err
}

type stubSpeechSynthesizer struct {
	request *audioinput.SpeechRequest
}

func TestRealtimeResultAndErrorsRedactEverySessionSecret(t *testing.T) {
	active := &session{
		artifactDir: t.TempDir(),
		apiKey:      []byte(`model-"quoted"-secret`), realtimeAPIKey: []byte(`tts-\backslash-secret`),
		gatewayToken: []byte(`gateway-"quote"-and-\slash-secret`),
	}
	secrets := []string{string(active.apiKey), string(active.realtimeAPIKey), string(active.gatewayToken)}
	for _, secret := range [][]byte{active.apiKey, active.realtimeAPIKey, active.gatewayToken} {
		if err := validateAPIKey(secret); err != nil {
			t.Fatalf("distinctive canary must be a valid API key: %v", err)
		}
	}
	result := realtimeclient.Result{
		SchemaVersion:  realtimeclient.ResultSchemaVersion,
		OriginalPrompt: strings.Join(secrets, " "), OutputText: secrets[0], Errors: append([]string(nil), secrets...),
		ConnectAuth: map[string]any{"token": secrets[2]},
		Timings:     map[string]any{secrets[0]: secrets[1]},
		Events:      []gatewayclient.Frame{{"type": "event", "payload": map[string]any{"authorization": secrets[2], "tts": secrets[1], "model": secrets[0]}}},
	}
	redacted := redactRealtimeResult(result, active)
	assertJSONValueHasNoSecrets(t, redacted, secrets)
	manager := &Manager{}
	path, err := manager.writeRealtimeResult(active, result)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var artifactValue any
	if err := json.Unmarshal(artifact, &artifactValue); err != nil {
		t.Fatal(err)
	}
	assertJSONValueHasNoSecrets(t, artifactValue, secrets)
	rawErr := fmt.Errorf("operation contained model=%s tts=%s gateway=%s: %w", secrets[0], secrets[1], secrets[2], context.Canceled)
	redactedErr := redactSessionError(rawErr, active)
	if !errors.Is(redactedErr, context.Canceled) {
		t.Fatalf("redacted error lost classification or retained secret: %v", redactedErr)
	}
	for _, secret := range secrets {
		if strings.Contains(redactedErr.Error(), secret) {
			t.Fatalf("redacted error retained %q: %v", secret, redactedErr)
		}
	}
	harnessResult := failedHarnessResult(active, time.Now(), rawErr)
	if harnessResult.Status != core.StatusCanceled {
		t.Fatalf("HarnessResult = %#v", harnessResult)
	}
	for _, secret := range secrets {
		if strings.Contains(harnessResult.Error, secret) {
			t.Fatalf("HarnessResult retained %q: %#v", secret, harnessResult)
		}
	}
}

func TestAgentResultAndErrorsRedactEverySessionSecret(t *testing.T) {
	secrets := []string{`model-"quoted"-secret`, `tts-\backslash-secret`, `gateway-"quote"-and-\slash-secret`}
	for _, test := range []struct {
		name    string
		gateway *secretAgentGateway
		status  string
	}{
		{name: "success", status: core.StatusSucceeded, gateway: &secretAgentGateway{
			summary: gatewayclient.ConnectSummary{Role: secrets[0], Scopes: []string{"operator.write", secrets[1]}},
			result:  gatewayclient.AgentResult{RunID: secrets[2], Text: strings.Join(secrets, " ")},
		}},
		{name: "canceled", status: core.StatusCanceled, gateway: &secretAgentGateway{
			summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.write"}},
			err:     fmt.Errorf("connect/agent contained %s %s %s: %w", secrets[0], secrets[1], secrets[2], context.Canceled),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDocker()
			manager := newTestManager(t, fake, []byte("initial-model-key"))
			manager.newGateway = func(string, []byte) (gatewayConnection, error) { return test.gateway, nil }
			request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: time.Second}
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			manager.active.apiKey = []byte(secrets[0])
			manager.active.realtimeAPIKey = []byte(secrets[1])
			manager.active.gatewayToken = []byte(secrets[2])
			result, runErr := manager.Run(context.Background(), "repair git")
			if result.Status != test.status {
				t.Fatalf("result = %#v, error = %v", result, runErr)
			}
			if test.status == core.StatusCanceled && !errors.Is(runErr, context.Canceled) {
				t.Fatalf("cancellation lost: %v", runErr)
			}
			for _, secret := range secrets {
				if strings.Contains(result.FinalResponse, secret) || strings.Contains(result.Error, secret) || runErr != nil && strings.Contains(runErr.Error(), secret) {
					t.Fatalf("returned result retained %q: %#v / %v", secret, result, runErr)
				}
			}
			artifact, err := os.ReadFile(filepath.Join(manager.active.artifactDir, "agent-result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(artifact, &decoded); err != nil {
				t.Fatal(err)
			}
			assertJSONValueHasNoSecrets(t, decoded, secrets)
		})
	}
}

func assertJSONValueHasNoSecrets(t *testing.T, value any, secrets []string) {
	t.Helper()
	var visit func(any)
	check := func(text string) {
		for _, secret := range secrets {
			if strings.Contains(text, secret) {
				t.Fatalf("JSON value retained secret %q in %q", secret, text)
			}
		}
	}
	visit = func(value any) {
		switch typed := value.(type) {
		case string:
			check(typed)
		case []any:
			for _, item := range typed {
				visit(item)
			}
		case map[string]any:
			for key, item := range typed {
				check(key)
				visit(item)
			}
		case realtimeclient.Result:
			content, err := json.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			var decoded any
			if err := json.Unmarshal(content, &decoded); err != nil {
				t.Fatal(err)
			}
			visit(decoded)
		}
	}
	visit(value)
}

func (synthesizer stubSpeechSynthesizer) Synthesize(_ context.Context, request audioinput.SpeechRequest) (audioinput.SpeechResult, error) {
	*synthesizer.request = request
	return audioinput.SpeechResult{
		Audio: testWAVBytes(24000, []byte{0, 0, 1, 0}),
		Model: "tts-model", Voice: "alloy", Format: "wav", TextSHA256: strings.Repeat("a", 64),
	}, nil
}

func (stubSpeechSynthesizer) Close() {}

func TestNewRequiresExactNonLatestTaggedImage(t *testing.T) {
	if manager, err := New(Options{Image: testOpenClawImage, OutputDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	} else if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{
		"",
		" " + testOpenClawImage,
		testOpenClawImage + "\n",
		"example.invalid/openclaw",
		"example.invalid/openclaw:latest",
		"example.invalid/openclaw:fixture@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"not a valid image",
	} {
		if _, err := New(Options{Image: image, OutputDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "OpenClaw image") {
			t.Fatalf("New(%q) error = %v", image, err)
		}
	}
}

func endpointFiles(t *testing.T) core.ToolEndpoint {
	t.Helper()
	root := t.TempDir()
	write := func(name string, mode os.FileMode, content string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries", Network: "aries-net-test",
		ClientCommand: "/opt/aries/bin/aries-ssh", ClientSourceFile: write("aries-ssh", 0o555, "client"),
		IdentityFile: "/run/aries/ssh/id_ed25519", IdentitySourceFile: write("id_ed25519", 0o600, "identity"),
		KnownHostsFile: "/run/aries/ssh/known_hosts", KnownHostsSourceFile: write("known_hosts", 0o600, "known"),
	}
}

func TestHarnessUsesOneSDKContainerAndDirectPrivateArchive(t *testing.T) {
	fake := newFakeDocker()
	// Docker 29 may keep the hijacked attach socket open after ExecInspect says
	// the process exited. The harness must still drain and return exact output.
	fake.keepAttachOpen = true
	secret := []byte("model-secret")
	manager := newTestManager(t, fake, secret)
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair git")
	if err != nil || result.Status != core.StatusSucceeded || result.FinalResponse != "task complete" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
	if fake.startCalls != 1 || fake.stopCalls != 1 || fake.removeCalls != 1 {
		t.Fatalf("container lifecycle = start %d stop %d remove %d", fake.startCalls, fake.stopCalls, fake.removeCalls)
	}
	if fake.created.Name != "aries-openclaw-attempt" || len(fake.created.Config.Cmd) == 0 || len(fake.created.HostConfig.Mounts) != 0 || len(fake.created.HostConfig.Binds) != 0 {
		t.Fatalf("container create = %#v", fake.created)
	}
	if !equalStrings(fake.created.Config.Cmd, append([]string{launcherPath}, gatewayLauncherCommand...)) {
		t.Fatalf("agent gateway command = %#v", fake.created.Config.Cmd)
	}
	bindings := fake.created.HostConfig.PortBindings[gatewayPort]
	if len(bindings) != 1 || bindings[0].HostIP.String() != "127.0.0.1" {
		t.Fatalf("agent gateway port bindings = %#v", bindings)
	}
	if _, err := os.Stat(filepath.Join(manager.outputDir, "fix-git", "harness", "agent-result.json")); err != nil {
		t.Fatalf("agent result artifact: %v", err)
	}
	serialized := strings.Join(append(append([]string{}, fake.created.Config.Env...), fake.created.Config.Cmd...), "\n")
	for _, value := range fake.created.Config.Labels {
		serialized += "\n" + value
	}
	if strings.Contains(serialized, "model-secret") {
		t.Fatal("secret entered Docker config")
	}
	files := readArchive(t, fake.archive)
	for path, mode := range map[string]int64{
		"run/aries/openclaw.json": 0o600, "run/aries/model.key": 0o600, "run/aries/gateway.key": 0o600,
		"run/aries/launch": 0o555, "run/aries/gateway-proxy.js": 0o555, "run/aries/gateway-launcher": 0o555,
		"run/aries/ssh/id_ed25519": 0o600, "run/aries/ssh/known_hosts": 0o600,
		"opt/aries/bin/aries-ssh": 0o555,
	} {
		file, ok := files[path]
		if !ok || file.mode != mode || file.uid != 1000 || file.gid != 1000 {
			t.Fatalf("archive %q = %#v", path, file)
		}
	}
	if string(files["run/aries/model.key"].content) != "model-secret" {
		t.Fatal("model key was not staged")
	}
	for _, path := range result.LogPaths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact %q: %v", path, err)
		}
	}
	configPath := filepath.Join(manager.outputDir, "fix-git", "harness", "openclaw.json")
	configArtifact, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("retained OpenClaw config: %v", err)
	}
	if string(configArtifact) != string(files["run/aries/openclaw.json"].content) || bytes.Contains(configArtifact, []byte("model-secret")) {
		t.Fatal("retained OpenClaw config differs from the staged placeholder-only config")
	}
	if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("retained OpenClaw config mode = %v, %v", info, err)
	}
}

func TestAgentRunUsesGatewayOnceWithExactParameters(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	gateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.write"}}}
	manager.newGateway = func(rawURL string, token []byte) (gatewayConnection, error) {
		if rawURL != "ws://127.0.0.1:38089" || len(token) == 0 {
			t.Fatalf("gateway construction = %q token=%d", rawURL, len(token))
		}
		return gateway, nil
	}
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair git")
	if err != nil || result.FinalResponse != "first\nsecond" {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if gateway.agentCalls != 1 || gateway.request.Message != "repair git" || gateway.request.SessionKey != "agent:main:aries-fix-git" || gateway.request.IdempotencyKey == "" {
		t.Fatalf("agent calls=%d request=%#v", gateway.agentCalls, gateway.request)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayEventDispositionFollowsHarnessMode(t *testing.T) {
	if got := gatewayEventDisposition(ModeAgent); got != gatewayclient.EventDispositionResponseOnly {
		t.Fatalf("agent disposition = %v", got)
	}
	if got := gatewayEventDisposition(ModeRealtime); got != gatewayclient.EventDispositionDelivery {
		t.Fatalf("realtime disposition = %v", got)
	}
}

func TestAgentRunRejectsMissingWriteScopeBeforeSubmission(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	gateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read"}}}
	manager.newGateway = func(string, []byte) (gatewayConnection, error) { return gateway, nil }
	if err := manager.Start(context.Background(), core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "repair git")
	if err == nil || result.Status != core.StatusFailed || gateway.agentCalls != 0 || !strings.Contains(err.Error(), "operator.write") {
		t.Fatalf("Run = %#v, %v calls=%d", result, err, gateway.agentCalls)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayURLRejectsMissingDuplicateWildcardAndInvalidBindings(t *testing.T) {
	for name, bindings := range map[string][]network.PortBinding{
		"missing":   nil,
		"duplicate": {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "3001"}, {HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "3002"}},
		"wildcard":  {{HostIP: netip.IPv4Unspecified(), HostPort: "3001"}},
		"invalid":   {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "bad"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeDocker()
			fake.container = container.InspectResponse{ID: "id", NetworkSettings: &container.NetworkSettings{Ports: network.PortMap{gatewayPort: bindings}}}
			manager := newTestManager(t, fake, []byte("model-secret"))
			if _, err := manager.gatewayURL(context.Background(), &session{containerID: "id"}); err == nil {
				t.Fatal("invalid binding was accepted")
			}
		})
	}
}

func TestStartFailureRemovesOnlyContainerAndClearsSecret(t *testing.T) {
	fake := newFakeDocker()
	fake.copyToErr = errors.New("copy failed")
	secret := []byte("source-secret")
	manager := newTestManager(t, fake, secret)
	err := manager.Start(context.Background(), core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()})
	if err == nil || fake.removeCalls != 1 || manager.active != nil {
		t.Fatalf("Start = %v, remove=%d active=%v", err, fake.removeCalls, manager.active)
	}
	if !bytes.Equal(secret, make([]byte, len(secret))) {
		t.Fatalf("API lookup buffer was not cleared: %q", secret)
	}
}

func TestRealtimeModePublishesGatewayAndRunsRunner(t *testing.T) {
	fake := newFakeDocker()
	keys := map[string][]byte{"ARIES_FAKE_API_KEY": []byte("model-secret"), "OPENAI_API_KEY": []byte("speech-secret")}
	manager := newTestManager(t, fake, keys["ARIES_FAKE_API_KEY"])
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		value, ok := keys[name]
		return append([]byte(nil), value...), ok
	}
	manager.mode = ModeRealtime
	manager.realtime = RealtimeOptions{
		TTS:            RealtimeTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "tts-model", Voice: "alloy"},
		ChunkDuration:  25 * time.Millisecond,
		ListenDuration: 50 * time.Millisecond, QuietDuration: time.Millisecond,
		AgentWaitDuration: 40 * time.Millisecond, ToolCallTimeout: 30 * time.Millisecond,
		TrailingSilenceMillis: 300, Voice: "alloy", ReasoningEffort: "low", IncludeEvents: true,
	}
	var speechRequest audioinput.SpeechRequest
	manager.newSpeech = func(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		if string(options.APIKey) != "speech-secret" {
			t.Fatalf("speech key = %q", string(options.APIKey))
		}
		return stubSpeechSynthesizer{request: &speechRequest}, nil
	}
	var gatewayURL string
	var gatewayToken []byte
	manager.newGateway = func(rawURL string, token []byte) (gatewayConnection, error) {
		gatewayURL = rawURL
		gatewayToken = append([]byte(nil), token...)
		return &stubGateway{}, nil
	}
	var runnerOptions realtimeclient.Options
	manager.newRealtime = func(_ realtimeclient.Gateway, options realtimeclient.Options) (realtimeRunner, error) {
		runnerOptions = options
		return stubRunner{result: realtimeclient.Result{
			SchemaVersion:       realtimeclient.ResultSchemaVersion,
			Transcript:          "heard",
			OutputText:          "spoken",
			AgentOutputText:     "final answer",
			EventCounts:         map[string]int{"chat.final": 1},
			ConnectAuth:         map[string]any{},
			Errors:              []string{},
			Timings:             map[string]any{},
			AgentRunIDs:         []string{},
			TranscriptDoneParts: []string{},
		}}, nil
	}
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fake.created.Config.Cmd, append([]string{launcherPath}, gatewayLauncherCommand...)) {
		t.Fatalf("gateway command = %#v", fake.created.Config.Cmd)
	}
	if _, ok := fake.created.Config.ExposedPorts[gatewayPort]; !ok {
		t.Fatalf("exposed ports = %#v", fake.created.Config.ExposedPorts)
	}
	bindings := fake.created.HostConfig.PortBindings[gatewayPort]
	if len(bindings) != 1 || bindings[0].HostIP.String() != "127.0.0.1" {
		t.Fatalf("port bindings = %#v", bindings)
	}
	result, err := manager.Run(context.Background(), "voice task")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "final answer" {
		t.Fatalf("result = %#v", result)
	}
	if gatewayURL != "ws://127.0.0.1:38089" || len(gatewayToken) == 0 {
		t.Fatalf("gateway URL/token = %q/%d", gatewayURL, len(gatewayToken))
	}
	if runnerOptions.OriginalPrompt != "voice task" || runnerOptions.SessionKey != "agent:main:aries-fix-git" ||
		runnerOptions.ChunkDuration != 25*time.Millisecond || runnerOptions.Voice != "alloy" || runnerOptions.ReasoningEffort != "low" || !runnerOptions.IncludeEvents {
		t.Fatalf("runner options = %#v", runnerOptions)
	}
	if runnerOptions.AudioProvider == nil {
		t.Fatal("realtime runner did not receive an audio provider")
	}
	if speechRequest.Text != "voice task" || speechRequest.Model != "tts-model" || speechRequest.Voice != "alloy" || speechRequest.Format != "wav" {
		t.Fatalf("speech request = %#v", speechRequest)
	}
	audio, err := runnerOptions.AudioProvider(realtimeclient.SessionInfo{InputEncoding: "pcm16", InputSampleRateHz: 24000})
	if err != nil {
		t.Fatalf("audio provider returned error: %v", err)
	}
	if len(audio.Data) == 0 || audio.Rate != 24000 || audio.Encoding != "pcm16" {
		t.Fatalf("prepared audio = %#v", audio)
	}
	files := readArchive(t, fake.archive)
	if string(files["run/aries/realtime.key"].content) != "speech-secret" {
		t.Fatal("realtime key was not staged")
	}
	if !strings.Contains(string(files["run/aries/launch"].content), "export OPENAI_API_KEY=\"$realtime_key\"") {
		t.Fatalf("launcher does not export realtime key: %s", files["run/aries/launch"].content)
	}
	serialized := strings.Join(append(append([]string{}, fake.created.Config.Env...), fake.created.Config.Cmd...), "\n")
	for _, value := range fake.created.Config.Labels {
		serialized += "\n" + value
	}
	if strings.Contains(serialized, "speech-secret") {
		t.Fatal("realtime secret entered Docker config")
	}
	for _, name := range []string{"voice-instruction.txt", "voice-instruction.wav", "voice-instruction.wav.meta.json"} {
		if _, err := os.Stat(filepath.Join(manager.outputDir, "fix-git", "harness", name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	path := filepath.Join(manager.outputDir, "fix-git", "harness", "realtime-result.json")
	content, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(content, []byte(`"agent_output_text": "final answer"`)) {
		t.Fatalf("realtime result artifact = %s, %v", content, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("realtime result artifact mode = %v, %v", info, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func testWAVBytes(rate int, pcm []byte) []byte {
	var chunks bytes.Buffer
	writeChunk := func(id string, payload []byte) {
		chunks.WriteString(id)
		_ = binary.Write(&chunks, binary.LittleEndian, uint32(len(payload)))
		chunks.Write(payload)
		if len(payload)%2 != 0 {
			chunks.WriteByte(0)
		}
	}
	var fmtChunk bytes.Buffer
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(1))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(1))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint32(rate*2))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(2))
	_ = binary.Write(&fmtChunk, binary.LittleEndian, uint16(16))
	writeChunk("fmt ", fmtChunk.Bytes())
	writeChunk("data", pcm)
	var out bytes.Buffer
	out.WriteString("RIFF")
	_ = binary.Write(&out, binary.LittleEndian, uint32(4+chunks.Len()))
	out.WriteString("WAVE")
	out.Write(chunks.Bytes())
	return out.Bytes()
}

func TestArtifactCollectionFailureBelongsToRunNotStop(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.containerLogsErr = errors.New("gateway log unavailable")
	result, err := manager.Run(context.Background(), "repair git")
	if err == nil || result.Status != core.StatusFailed || !strings.Contains(err.Error(), "gateway log unavailable") {
		t.Fatalf("Run = %#v, %v", result, err)
	}
	if len(result.LogPaths) == 0 {
		t.Fatal("failed Run omitted collected artifact paths")
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop was contaminated by artifact collection: %v", err)
	}
	if fake.logsCalls != 1 || fake.removeCalls != 1 {
		t.Fatalf("artifact/lifecycle calls = logs %d remove %d", fake.logsCalls, fake.removeCalls)
	}
}

func TestStopSucceedsWhenForcedRemovalConfirmsAbsence(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	fake.stopErr = errors.New("graceful stop failed")
	fake.killErr = errors.New("kill failed")
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("Stop should trust confirmed final absence: %v", err)
	}
	if fake.stopCalls != 1 || fake.killCalls != 1 || fake.removeCalls != 1 {
		t.Fatalf("lifecycle calls = stop %d kill %d remove %d", fake.stopCalls, fake.killCalls, fake.removeCalls)
	}
}

func TestStopFailsUntilContainerAbsenceCanBeConfirmed(t *testing.T) {
	fake := newFakeDocker()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	active := manager.active
	fake.inspectErr = errors.New("daemon unavailable")
	fake.removeErr = errors.New("remove unavailable")
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("Stop succeeded without confirming container absence")
	}
	if manager.active != active || active.containerID == "" || len(active.apiKey) == 0 {
		t.Fatal("failed cleanup discarded retry state or secrets before confirmed removal")
	}
	fake.inspectErr = nil
	fake.removeErr = nil
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatalf("retry Stop: %v", err)
	}
	if manager.active != nil || active.containerID != "" || len(active.apiKey) != 0 {
		t.Fatal("successful retry did not clear lifecycle state and secrets")
	}
}

type archiveFile struct {
	content []byte
	mode    int64
	uid     int
	gid     int
}

func readArchive(t *testing.T, archive []byte) map[string]archiveFile {
	t.Helper()
	files := make(map[string]archiveFile)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = archiveFile{content: content, mode: header.Mode, uid: header.Uid, gid: header.Gid}
	}
}

func TestInputValidationAndSecretHelpers(t *testing.T) {
	for _, value := range [][]byte{nil, {}, []byte("bad\nkey"), bytes.Repeat([]byte{'x'}, maxAPIKeyBytes+1)} {
		if err := validateAPIKey(value); err == nil {
			t.Fatalf("validateAPIKey(%q) succeeded", value)
		}
	}
	for _, id := range []string{"", "-bad", "bad/name"} {
		if err := validateRunID(id); err == nil {
			t.Fatalf("validateRunID(%q) succeeded", id)
		}
	}
	if got := safeTaskID(" Fix / Git "); got != "fix---git" {
		t.Fatalf("safeTaskID = %q", got)
	}
	t.Setenv("ARIES_TEST_KEY", "value")
	first, _ := environmentAPIKeyLookup("ARIES_TEST_KEY")
	first[0] = 'X'
	second, _ := environmentAPIKeyLookup("ARIES_TEST_KEY")
	if string(second) != "value" {
		t.Fatalf("environment lookup aliased: %q", second)
	}
}
