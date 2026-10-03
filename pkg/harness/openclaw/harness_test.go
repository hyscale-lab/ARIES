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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	audioinput "github.com/hyscale-lab/aries/pkg/audio"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
	realtimeclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/realtime"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

const testOpenClawImage = "ghcr.io/openclaw/openclaw:2026.7.1"

type fakeDeployment struct {
	createErr, validateErr                                                 error
	validatedRequest                                                       deployment.Request
	validatedSecrets                                                       [][]byte
	created                                                                deployment.Request
	archive                                                                []byte
	removed                                                                bool
	copyToErr, containerLogsErr, copyFromErr, stopErr, closeErr            error
	startCalls, logsCalls, stopCalls, removeCalls, createCalls, closeCalls int
}

func (f *fakeDeployment) Close() error { f.closeCalls++; return f.closeErr }
func TestManagerCloseIsIdempotent(t *testing.T) {
	closeFailure := errors.New("close failed")
	fake := newFakeDeployment()
	fake.closeErr = closeFailure
	manager := &Manager{deployment: fake}
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
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	cpu, memory := 2.5, 1536
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), CPU: &cpu, MemoryMB: &memory}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if got := fake.created; got.CPU == nil || *got.CPU != cpu || got.MemoryMB == nil || *got.MemoryMB != memory {
		t.Fatalf("resources = %#v", got)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func newFakeDeployment() *fakeDeployment { return &fakeDeployment{} }
func (f *fakeDeployment) Create(_ context.Context, r deployment.Request) (string, error) {
	f.created = r
	f.createCalls++
	return "openclaw-id", f.createErr
}
func (f *fakeDeployment) Validate(_ context.Context, _ string, request deployment.Request, secrets [][]byte) error {
	f.validatedRequest = request
	f.validatedSecrets = secrets
	return f.validateErr
}
func (f *fakeDeployment) UploadArchive(_ context.Context, _, _ string, r io.Reader) error {
	if f.copyToErr != nil {
		return f.copyToErr
	}
	b, e := io.ReadAll(r)
	f.archive = b
	return e
}
func (f *fakeDeployment) Start(context.Context, string) error           { f.startCalls++; return nil }
func (f *fakeDeployment) Running(context.Context, string) (bool, error) { return !f.removed, nil }
func (f *fakeDeployment) Exec(ctx context.Context, _ string, _ core.Command) (core.CommandResult, error) {
	return core.CommandResult{}, ctx.Err()
}
func (f *fakeDeployment) Logs(context.Context, string, int) ([]byte, error) {
	f.logsCalls++
	return []byte("gateway ready\n"), f.containerLogsErr
}
func (f *fakeDeployment) Address(context.Context, string, int) (string, error) {
	return "127.0.0.1:38089", nil
}
func (f *fakeDeployment) Stop(context.Context, string) error {
	f.stopCalls++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.removeCalls++
	f.removed = true
	return nil
}
func (fake *fakeDeployment) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	if fake.copyFromErr != nil {
		return nil, deployment.FileInfo{}, fake.copyFromErr
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	content := []byte("{\"event\":\"tool\"}\n")
	_ = writer.WriteHeader(&tar.Header{Name: "sessions/run.trajectory.jsonl", Mode: 0o600, Size: int64(len(content))})
	_, _ = writer.Write(content)
	_ = writer.Close()
	return io.NopCloser(bytes.NewReader(archive.Bytes())), deployment.FileInfo{}, nil
}

func TestCollectTelemetryIgnoresOnlyTypedMissingPath(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	active := &session{containerID: "openclaw-id", artifactDir: t.TempDir()}

	fake.copyFromErr = fmt.Errorf("session path absent: %w", runner.ErrNotFound)
	if paths, err := manager.collectTelemetry(context.Background(), active); err != nil || len(paths) != 0 {
		t.Fatalf("typed missing telemetry = %v, %v", paths, err)
	}

	fake.copyFromErr = errors.New("daemon not found while unavailable")
	if _, err := manager.collectTelemetry(context.Background(), active); err == nil || !strings.Contains(err.Error(), "daemon not found") {
		t.Fatalf("untyped daemon failure was masked: %v", err)
	}
}

func newTestManager(t *testing.T, fake *fakeDeployment, secret []byte) *Manager {
	t.Helper()
	manager, err := New(Options{
		Deployment: fake,
		Image:      testOpenClawImage, OutputDir: t.TempDir(), StartTimeout: time.Second, AgentTimeout: time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return secret, true },
	})
	if err != nil {
		t.Fatal(err)
	}
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
	onAgent    func(context.Context)
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

func (gateway *recordingGateway) Agent(ctx context.Context, request gatewayclient.AgentRequest) (gatewayclient.AgentResult, error) {
	gateway.agentCalls++
	gateway.request = request
	if err := ctx.Err(); err != nil {
		return gatewayclient.AgentResult{}, err
	}
	if gateway.onAgent != nil {
		gateway.onAgent(ctx)
	}
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

type harnessGatewayTransport struct {
	in     chan []byte
	out    chan []byte
	closed chan struct{}
	once   sync.Once
}

func newHarnessGatewayTransport(initial ...gatewayclient.Frame) *harnessGatewayTransport {
	transport := &harnessGatewayTransport{in: make(chan []byte, 4096), out: make(chan []byte, 16), closed: make(chan struct{})}
	for _, frame := range initial {
		transport.deliver(frame)
	}
	return transport
}

func (transport *harnessGatewayTransport) Send(ctx context.Context, content []byte) error {
	select {
	case transport.out <- append([]byte(nil), content...):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-transport.closed:
		return context.Canceled
	}
}

func (transport *harnessGatewayTransport) Receive(ctx context.Context) ([]byte, error) {
	select {
	case content := <-transport.in:
		return content, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-transport.closed:
		return nil, context.Canceled
	}
}

func (transport *harnessGatewayTransport) Close() error {
	transport.once.Do(func() { close(transport.closed) })
	return nil
}

func (transport *harnessGatewayTransport) deliver(frame gatewayclient.Frame) {
	content, _ := json.Marshal(frame)
	transport.in <- content
}

func (transport *harnessGatewayTransport) nextSent(t *testing.T) gatewayclient.Frame {
	t.Helper()
	select {
	case content := <-transport.out:
		var frame gatewayclient.Frame
		if err := json.Unmarshal(content, &frame); err != nil {
			t.Fatal(err)
		}
		return frame
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for gateway send")
		return nil
	}
}

type stubRunner struct {
	result realtimeclient.Result
	err    error
	onRun  func(context.Context)
}

func (runner stubRunner) Run(ctx context.Context) (realtimeclient.Result, error) {
	if runner.onRun != nil {
		runner.onRun(ctx)
	}
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
			fake := newFakeDeployment()
			manager := newTestManager(t, fake, []byte("initial-model-key"))
			manager.newGateway = func(string, []byte) (gatewayConnection, error) { return test.gateway, nil }
			request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: time.Second}
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
	if manager, err := New(Options{Deployment: newFakeDeployment(), Image: testOpenClawImage, OutputDir: t.TempDir()}); err != nil {
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
		if _, err := New(Options{Deployment: newFakeDeployment(), Image: image, OutputDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "OpenClaw image") {
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
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries",
		ClientCommand: "/opt/aries/bin/aries-ssh", ClientSourceFile: write("aries-ssh", 0o555, "client"),
		IdentityFile: "/run/aries/ssh/id_ed25519", IdentitySourceFile: write("id_ed25519", 0o600, "identity"),
		KnownHostsFile: "/run/aries/ssh/known_hosts", KnownHostsSourceFile: write("known_hosts", 0o600, "known"),
	}
}

func TestHarnessUsesInjectedDeploymentAndPrivateArchive(t *testing.T) {
	fake := newFakeDeployment()
	secret := []byte("model-secret")
	manager := newTestManager(t, fake, secret)
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
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
	if fake.created.Name != "aries-openclaw-attempt" || len(fake.created.Args) == 0 {
		t.Fatalf("container create = %#v", fake.created)
	}
	if !equalStrings(fake.created.Args, []string{launcherPath, gatewayLauncherPath}) {
		t.Fatalf("agent gateway command = %#v", fake.created.Args)
	}
	if fake.created.ServicePort != 18789 {
		t.Fatalf("service port = %d", fake.created.ServicePort)
	}
	if _, err := os.Stat(filepath.Join(manager.outputDir, "fix-git", "harness", "agent-result.json")); err != nil {
		t.Fatalf("agent result artifact: %v", err)
	}
	serialized := strings.Join(append(append([]string{}, fake.created.Env...), fake.created.Args...), "\n")
	for _, value := range fake.created.Labels {
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

// When extract is configured, the Tavily key must be staged in its own file,
// exported under OpenClaw's fixed TAVILY_API_KEY name by the launcher,
// enable the tavily plugin entry in the retained config, and stay out of
// Docker metadata and the retained config — the same guarantees the model
// key already has.
func TestStartStagesExtractKeyWhenConfigured(t *testing.T) {
	modelSecret := []byte("model-secret")
	extractSecret := []byte("tvly-super-secret")
	fake := newFakeDeployment()
	manager, err := New(Options{
		Deployment: fake,
		Image:      testOpenClawImage, OutputDir: t.TempDir(), StartTimeout: time.Second, AgentTimeout: time.Second,
		WebSearchEnabled: true, ExtractAPIKeyEnv: "TAVILY_API_KEY",
		APIKeyLookup: func(name string) ([]byte, bool) {
			if name == "TAVILY_API_KEY" {
				return bytes.Clone(extractSecret), true
			}
			return bytes.Clone(modelSecret), true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }

	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	files := readArchive(t, fake.archive)
	extractFile, ok := files["run/aries/tavily.key"]
	if !ok || extractFile.mode != 0o600 || string(extractFile.content) != string(extractSecret) {
		t.Fatalf("staged tavily key = %#v", extractFile)
	}
	launchScript := string(files["run/aries/launch"].content)
	for _, required := range []string{"tavily_key=$(cat /run/aries/tavily.key)", "export TAVILY_API_KEY=\"$tavily_key\""} {
		if !strings.Contains(launchScript, required) {
			t.Fatalf("launcher missing %q: %s", required, launchScript)
		}
	}

	serialized := strings.Join(append(append([]string{}, fake.created.Env...), fake.created.Args...), "\n")
	for _, value := range fake.created.Labels {
		serialized += "\n" + value
	}
	if strings.Contains(serialized, string(extractSecret)) {
		t.Fatal("extract secret entered Docker config")
	}
	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "openclaw.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(retained, extractSecret) {
		t.Fatalf("extract secret leaked into the retained config:\n%s", retained)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(retained, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Tools.Web.Search.Provider != "searxng" {
		t.Fatalf("tools.web.search.provider = %q, want unchanged searxng", configuration.Tools.Web.Search.Provider)
	}
	tavilyEntry, ok := configuration.Plugins.Entries["tavily"]
	if !ok || !tavilyEntry.Enabled {
		t.Fatalf("plugins.entries.tavily = %#v", configuration.Plugins.Entries)
	}
}

// Unlike Hermes's identically-named ExtractAPIKeyEnv, a missing OpenClaw
// extract credential must not abort the run: Start should fall back to
// plain SearXNG-backed web_fetch (no tavily plugin entry at all) and log a
// warning rather than fail.
func TestStartFallsBackToNativeWebFetchWhenExtractCredentialMissing(t *testing.T) {
	fake := newFakeDeployment()
	logger, hook := test.NewNullLogger()
	manager, err := New(Options{
		Deployment: fake,
		Image:      testOpenClawImage, OutputDir: t.TempDir(), StartTimeout: time.Second, AgentTimeout: time.Second,
		WebSearchEnabled: true, ExtractAPIKeyEnv: "TAVILY_API_KEY", Logger: logger,
		APIKeyLookup: func(name string) ([]byte, bool) {
			if name == "TAVILY_API_KEY" {
				return nil, false
			}
			return []byte("model-secret"), true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }

	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatalf("expected fallback instead of rejection, got: %v", err)
	}
	defer manager.Stop(context.Background())
	if fake.created.Name == "" {
		t.Fatal("container was not created despite the intended fallback")
	}

	var warned bool
	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel && strings.Contains(entry.Message, "falling back") {
			warned = true
			break
		}
	}
	if !warned {
		t.Fatalf("expected a fallback warning log entry, got %#v", hook.AllEntries())
	}

	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "openclaw.json"))
	if err != nil {
		t.Fatal(err)
	}
	var configuration openClawConfig
	if err := json.Unmarshal(retained, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.Tools.Web == nil || configuration.Tools.Web.Search == nil || configuration.Tools.Web.Search.Provider != "searxng" {
		t.Fatalf("tools.web.search = %#v, want unchanged searxng", configuration.Tools.Web)
	}
	if _, ok := configuration.Plugins.Entries["searxng"]; !ok {
		t.Fatalf("plugins.entries = %#v, want searxng still present", configuration.Plugins.Entries)
	}
	if _, ok := configuration.Plugins.Entries["tavily"]; ok {
		t.Fatalf("plugins.entries = %#v, want no tavily entry on fallback", configuration.Plugins.Entries)
	}
	for _, tool := range configuration.Tools.Sandbox.Tools.AlsoAllow {
		if tool == "tavily_extract" {
			t.Fatalf("tools.sandbox.tools.alsoAllow = %#v, want tavily_extract absent on fallback", configuration.Tools.Sandbox.Tools.AlsoAllow)
		}
	}
}

func TestAgentRunUsesGatewayOnceWithExactParameters(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	gateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.write"}}}
	manager.newGateway = func(rawURL string, token []byte) (gatewayConnection, error) {
		if rawURL != "ws://127.0.0.1:38089" || len(token) == 0 {
			t.Fatalf("gateway construction = %q token=%d", rawURL, len(token))
		}
		return gateway, nil
	}
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel(), Timeout: 37 * time.Second}
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

func TestDeepSeekAgentRequestsDisableThinking(t *testing.T) {
	for _, modelID := range []string{"deepseek-flash", "deepseek-v4-flash", "deepseek-v4-pro"} {
		for _, mode := range []string{ModeAgent, ModeVoiceTranscribe} {
			t.Run(modelID+"/"+mode, func(t *testing.T) {
				manager := newTestManager(t, newFakeDeployment(), []byte("model-secret"))
				gateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.write"}}}
				manager.newGateway = func(string, []byte) (gatewayConnection, error) { return gateway, nil }
				model := testModel()
				model.BaseURL, model.Model = "https://api.deepseek.com", modelID
				request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: model}
				if err := manager.Start(context.Background(), request); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := manager.Stop(context.Background()); err != nil {
						t.Error(err)
					}
				})
				if mode == ModeAgent {
					if _, err := manager.Run(context.Background(), "repair git"); err != nil {
						t.Fatal(err)
					}
				} else {
					result := realtimeclient.Result{Transcript: "repair git"}
					if err := manager.runAgentWithTranscript(context.Background(), manager.active, gateway, &result); err != nil {
						t.Fatal(err)
					}
				}
				if gateway.agentCalls != 1 || gateway.request.Thinking != "off" {
					t.Fatalf("agent calls=%d thinking=%q; want one request with thinking off", gateway.agentCalls, gateway.request.Thinking)
				}
			})
		}
	}
}

func TestGatewayEventDispositionFollowsHarnessMode(t *testing.T) {
	if got := gatewayEventDisposition(ModeAgent); got != gatewayclient.EventDispositionResponseOnly {
		t.Fatalf("agent disposition = %v", got)
	}
	if got := gatewayEventDisposition(ModeRealtime); got != gatewayclient.EventDispositionDelivery {
		t.Fatalf("realtime disposition = %v", got)
	}
	if got := gatewayEventDisposition(ModeVoiceTranscribe); got != gatewayclient.EventDispositionDelivery {
		t.Fatalf("voice-transcribe disposition = %v", got)
	}
}

func TestAgentRunRejectsMissingWriteScopeBeforeSubmission(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	gateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read"}}}
	manager.newGateway = func(string, []byte) (gatewayConnection, error) { return gateway, nil }
	if err := manager.Start(context.Background(), core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}); err != nil {
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

func TestStartFailureRemovesOnlyContainerAndClearsSecret(t *testing.T) {
	fake := newFakeDeployment()
	fake.copyToErr = errors.New("copy failed")
	secret := []byte("source-secret")
	manager := newTestManager(t, fake, secret)
	err := manager.Start(context.Background(), core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()})
	if err == nil || fake.removeCalls != 1 || manager.active != nil {
		t.Fatalf("Start = %v, remove=%d active=%v", err, fake.removeCalls, manager.active)
	}
	if !bytes.Equal(secret, make([]byte, len(secret))) {
		t.Fatalf("API lookup buffer was not cleared: %q", secret)
	}
}

func TestRealtimeModePublishesGatewayAndRunsRunner(t *testing.T) {
	fake := newFakeDeployment()
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
			EventCounts:         map[string]int{"chat.final": 1},
			ConnectAuth:         map[string]any{},
			Errors:              []string{},
			AgentRunIDs:         []string{},
			TranscriptDoneParts: []string{},
		}}, nil
	}
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(fake.created.Args, []string{launcherPath, gatewayLauncherPath}) {
		t.Fatalf("gateway command = %#v", fake.created.Args)
	}

	if fake.created.ServicePort != 18789 {
		t.Fatalf("service port = %d", fake.created.ServicePort)
	}
	result, err := manager.Run(context.Background(), "voice task")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "spoken" {
		t.Fatalf("result = %#v", result)
	}
	if gatewayURL != "ws://127.0.0.1:38089" || len(gatewayToken) == 0 {
		t.Fatalf("gateway URL/token = %q/%d", gatewayURL, len(gatewayToken))
	}
	if runnerOptions.OriginalPrompt != "voice task" || runnerOptions.SessionMode != ModeRealtime || runnerOptions.SessionKey != "agent:main:aries-fix-git" ||
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
	serialized := strings.Join(append(append([]string{}, fake.created.Env...), fake.created.Args...), "\n")
	for _, value := range fake.created.Labels {
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
	if err != nil || !bytes.Contains(content, []byte(`"output_text": "spoken"`)) {
		t.Fatalf("realtime result artifact = %s, %v", content, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("realtime result artifact mode = %v, %v", info, err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeTranscribeModeSendsTranscriptToAgent(t *testing.T) {
	fake := newFakeDeployment()
	keys := map[string][]byte{"ARIES_FAKE_API_KEY": []byte("model-secret"), "OPENAI_API_KEY": []byte("speech-secret")}
	manager := newTestManager(t, fake, keys["ARIES_FAKE_API_KEY"])
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		value, ok := keys[name]
		return append([]byte(nil), value...), ok
	}
	manager.mode = ModeVoiceTranscribe
	manager.realtime = RealtimeOptions{
		TTS: RealtimeTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "tts-model", Voice: "alloy"},
	}
	var speechRequest audioinput.SpeechRequest
	manager.newSpeech = func(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		return stubSpeechSynthesizer{request: &speechRequest}, nil
	}
	gateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}}
	manager.newGateway = func(string, []byte) (gatewayConnection, error) {
		return &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}}, nil
	}
	manager.newAgentGateway = func(string, []byte) (gatewayConnection, error) {
		return gateway, nil
	}
	var runnerOptions realtimeclient.Options
	manager.newRealtime = func(_ realtimeclient.Gateway, options realtimeclient.Options) (realtimeRunner, error) {
		runnerOptions = options
		return stubRunner{result: realtimeclient.Result{
			SchemaVersion:       realtimeclient.ResultSchemaVersion,
			Transcript:          "then cherry-pick it",
			TranscriptDone:      "then cherry-pick it",
			TranscriptDoneParts: []string{"repair git from transcript", "then cherry-pick it"},
			EventCounts:         map[string]int{"transcript.done": 2},
			ConnectAuth:         map[string]any{},
			Errors:              []string{},
			AgentRunIDs:         []string{},
		}}, nil
	}
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "voice task")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if runnerOptions.SessionMode != ModeVoiceTranscribe {
		t.Fatalf("runner options = %#v", runnerOptions)
	}
	if gateway.agentCalls != 1 || gateway.request.Message != "repair git from transcript\nthen cherry-pick it" || gateway.request.SessionKey != "agent:main:aries-fix-git" || gateway.request.IdempotencyKey == "" {
		t.Fatalf("agent calls=%d request=%#v", gateway.agentCalls, gateway.request)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "first\nsecond" {
		t.Fatalf("result = %#v", result)
	}
	path := filepath.Join(manager.outputDir, "fix-git", "harness", "realtime-result.json")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"transcript": "then cherry-pick it"`, `"agent_question_used": "repair git from transcript\nthen cherry-pick it"`, `"output_text": "first\nsecond"`, `"agent_consult_ok": true`} {
		if !bytes.Contains(content, []byte(want)) {
			t.Fatalf("realtime result missing %s: %s", want, content)
		}
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeTranscribeErrorsDoNotStartAgent(t *testing.T) {
	fake := newFakeDeployment()
	keys := map[string][]byte{"ARIES_FAKE_API_KEY": []byte("model-secret"), "OPENAI_API_KEY": []byte("speech-secret")}
	manager := newTestManager(t, fake, keys["ARIES_FAKE_API_KEY"])
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		value, ok := keys[name]
		return append([]byte(nil), value...), ok
	}
	manager.mode = ModeVoiceTranscribe
	manager.realtime = RealtimeOptions{
		TTS: RealtimeTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "tts-model", Voice: "alloy"},
	}
	var speechRequest audioinput.SpeechRequest
	manager.newSpeech = func(audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		return stubSpeechSynthesizer{request: &speechRequest}, nil
	}
	manager.newGateway = func(string, []byte) (gatewayConnection, error) {
		return &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}}, nil
	}
	agentGatewayCreated := false
	agentGateway := &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.write"}}}
	manager.newAgentGateway = func(string, []byte) (gatewayConnection, error) {
		agentGatewayCreated = true
		return agentGateway, nil
	}
	manager.newRealtime = func(_ realtimeclient.Gateway, _ realtimeclient.Options) (realtimeRunner, error) {
		return stubRunner{result: realtimeclient.Result{
			SchemaVersion:       realtimeclient.ResultSchemaVersion,
			Transcript:          "partial request",
			TranscriptDone:      "partial request",
			TranscriptDoneParts: []string{"partial request"},
			EventCounts:         map[string]int{"transcript.done": 1, "session.error": 1},
			ConnectAuth:         map[string]any{},
			Errors:              []string{"session.error: provider failed after final transcript"},
			AgentRunIDs:         []string{},
		}}, nil
	}
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "voice task")
	if err == nil || !strings.Contains(err.Error(), "session.error") {
		t.Fatalf("Run error = %v", err)
	}
	if result.Status != core.StatusFailed {
		t.Fatalf("result = %#v", result)
	}
	if agentGatewayCreated || agentGateway.agentCalls != 0 {
		t.Fatalf("agent was called despite transcription errors: created=%v calls=%d", agentGatewayCreated, agentGateway.agentCalls)
	}
	path := filepath.Join(manager.outputDir, "fix-git", "harness", "realtime-result.json")
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Contains(content, []byte(`"errors": [`)) || !bytes.Contains(content, []byte(`"agent_question_used": ""`)) || !bytes.Contains(content, []byte(`"output_text": ""`)) {
		t.Fatalf("realtime result = %s", content)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeTranscribeUsesSeparateSTTTimeoutAndFreshAgentTimeout(t *testing.T) {
	fake := newFakeDeployment()
	keys := map[string][]byte{"ARIES_FAKE_API_KEY": []byte("model-secret"), "OPENAI_API_KEY": []byte("speech-secret")}
	manager := newTestManager(t, fake, keys["ARIES_FAKE_API_KEY"])
	manager.agentTimeout = 20 * time.Millisecond
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		value, ok := keys[name]
		return append([]byte(nil), value...), ok
	}
	manager.mode = ModeVoiceTranscribe
	manager.realtime = RealtimeOptions{
		TTS: RealtimeTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "tts-model", Voice: "alloy"},
	}
	var speechRequest audioinput.SpeechRequest
	manager.newSpeech = func(audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		return stubSpeechSynthesizer{request: &speechRequest}, nil
	}
	manager.newGateway = func(string, []byte) (gatewayConnection, error) {
		return &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}}, nil
	}
	transcribeContextChecked := false
	agentContextChecked := false
	manager.newAgentGateway = func(string, []byte) (gatewayConnection, error) {
		return &recordingGateway{
			summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.write"}},
			onAgent: func(ctx context.Context) {
				agentContextChecked = true
				if err := ctx.Err(); err != nil {
					t.Fatalf("agent context was already expired: %v", err)
				}
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.After(time.Now()) {
					t.Fatalf("agent context deadline = %v, ok=%v", deadline, ok)
				}
			},
		}, nil
	}
	manager.newRealtime = func(_ realtimeclient.Gateway, _ realtimeclient.Options) (realtimeRunner, error) {
		return stubRunner{
			onRun: func(ctx context.Context) {
				transcribeContextChecked = true
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("transcribe context has no deadline")
				}
				remaining := time.Until(deadline)
				if remaining <= time.Minute {
					t.Fatalf("transcribe context deadline is too short: %v", remaining)
				}
			},
			result: realtimeclient.Result{
				SchemaVersion:       realtimeclient.ResultSchemaVersion,
				Transcript:          "repair git",
				TranscriptDone:      "repair git",
				TranscriptDoneParts: []string{"repair git"},
				EventCounts:         map[string]int{"transcript.done": 1},
				ConnectAuth:         map[string]any{},
				Errors:              []string{},
				AgentRunIDs:         []string{},
			},
		}, nil
	}
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "voice task")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if !transcribeContextChecked {
		t.Fatal("transcribe context was not checked")
	}
	if !agentContextChecked {
		t.Fatal("agent context was not checked")
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "first\nsecond" {
		t.Fatalf("result = %#v", result)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRealtimeTranscribeAgentUsesResponseOnlyGateway(t *testing.T) {
	fake := newFakeDeployment()
	keys := map[string][]byte{"ARIES_FAKE_API_KEY": []byte("model-secret"), "OPENAI_API_KEY": []byte("speech-secret")}
	manager := newTestManager(t, fake, keys["ARIES_FAKE_API_KEY"])
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		value, ok := keys[name]
		return append([]byte(nil), value...), ok
	}
	manager.mode = ModeVoiceTranscribe
	manager.realtime = RealtimeOptions{
		TTS: RealtimeTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "tts-model", Voice: "alloy"},
	}
	var speechRequest audioinput.SpeechRequest
	manager.newSpeech = func(audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		return stubSpeechSynthesizer{request: &speechRequest}, nil
	}
	manager.newGateway = func(string, []byte) (gatewayConnection, error) {
		return &recordingGateway{summary: gatewayclient.ConnectSummary{Role: "operator", Scopes: []string{"operator.read", "operator.write"}}}, nil
	}
	manager.newRealtime = func(_ realtimeclient.Gateway, _ realtimeclient.Options) (realtimeRunner, error) {
		return stubRunner{result: realtimeclient.Result{
			SchemaVersion:       realtimeclient.ResultSchemaVersion,
			Transcript:          "repair git",
			TranscriptDone:      "repair git",
			TranscriptDoneParts: []string{"repair git"},
			EventCounts:         map[string]int{"transcript.done": 1},
			ConnectAuth:         map[string]any{},
			Errors:              []string{},
			AgentRunIDs:         []string{},
		}}, nil
	}
	transport := newHarnessGatewayTransport(gatewayclient.Frame{"type": "event", "event": "connect.challenge", "payload": map[string]any{"nonce": "n-1"}})
	manager.newAgentGateway = func(string, []byte) (gatewayConnection, error) {
		return gatewayclient.New(func(context.Context) (gatewayclient.Transport, error) { return transport, nil }, gatewayclient.Options{
			Token: "gateway-token", Scopes: []string{"operator.write"}, EventDisposition: gatewayclient.EventDispositionResponseOnly,
		})
	}
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		connect := transport.nextSent(t)
		transport.deliver(gatewayclient.Frame{"type": "res", "id": connect.String("id"), "ok": true, "payload": map[string]any{"auth": map[string]any{"scopes": []any{"operator.write"}}}})
		agent := transport.nextSent(t)
		transport.deliver(gatewayclient.Frame{"type": "res", "id": agent.String("id"), "ok": true, "payload": map[string]any{"status": "accepted", "runId": "run-high-volume"}})
		for index := 0; index < 2049; index++ {
			transport.deliver(gatewayclient.Frame{"type": "event", "event": "agent.progress", "payload": map[string]any{"index": index}})
		}
		transport.deliver(gatewayclient.Frame{"type": "res", "id": agent.String("id"), "ok": true, "payload": map[string]any{
			"status": "ok", "runId": "run-high-volume",
			"result": map[string]any{"payloads": []any{map[string]any{"text": "complete"}}},
		}})
	}()
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "voice task")
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	<-agentDone
	if result.Status != core.StatusSucceeded || result.FinalResponse != "complete" {
		t.Fatalf("result = %#v", result)
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
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
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

func TestStopFailsUntilContainerAbsenceCanBeConfirmed(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	active := manager.active
	fake.stopErr = errors.New("remove unavailable")
	if err := manager.Stop(context.Background()); err == nil {
		t.Fatal("Stop succeeded without confirming container absence")
	}
	if manager.active != active || active.containerID == "" || len(active.apiKey) == 0 {
		t.Fatal("failed cleanup discarded retry state or secrets before confirmed removal")
	}
	fake.stopErr = nil
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

func TestStartRollsBackDeploymentValidationFailure(t *testing.T) {
	fake := newFakeDeployment()
	fake.validateErr = errors.New("unsafe deployment")
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); !errors.Is(err, fake.validateErr) {
		t.Fatalf("Start() = %v", err)
	}
	if !fake.removed || fake.startCalls != 0 {
		t.Fatalf("removed=%v starts=%d", fake.removed, fake.startCalls)
	}
}
func TestStartRollsBackPartialDeploymentCreate(t *testing.T) {
	fake := newFakeDeployment()
	fake.createErr = errors.New("partial create")
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	if err := manager.Start(context.Background(), request); !errors.Is(err, fake.createErr) {
		t.Fatalf("Start() = %v", err)
	}
	if !fake.removed || manager.active != nil {
		t.Fatal("partial deployment was not cleaned up")
	}
}
func TestDeploymentReceivesRuntimeConstraintsAndSecretValidation(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	manager.extractAPIKeyEnv = "EXTRACT_KEY"
	manager.webSearchEnabled = true
	manager.apiKeyLookup = func(name string) ([]byte, bool) {
		if name == "EXTRACT_KEY" {
			return []byte("extract-secret"), true
		}
		return []byte("model-secret"), true
	}
	request := core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	cpu, memory := 2.5, 1536
	request.CPU, request.MemoryMB = &cpu, &memory
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	got := fake.validatedRequest
	if got.Name != fake.created.Name || got.Image != testOpenClawImage || got.Network != request.Network || got.ServicePort != 18789 || len(got.ImageVolumes) != 0 || got.CPU == nil || *got.CPU != cpu || got.MemoryMB == nil || *got.MemoryMB != memory {
		t.Fatalf("validation request = %#v", got)
	}
	for _, want := range []string{"model-secret", "extract-secret", string(manager.active.gatewayToken)} {
		found := false
		for _, secret := range fake.validatedSecrets {
			if string(secret) == want {
				found = true
			}
		}
		if !found || want == "" {
			t.Fatal("deployment validation omitted a session secret")
		}
	}
}

func (*fakeDeployment) ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error) {
	return core.CommandResult{}, errors.New("unexpected harness streaming call")
}
func (*fakeDeployment) LogsStream(context.Context, string, io.Writer, io.Writer) error {
	return errors.New("unexpected harness streaming logs call")
}

func TestStartRejectsMissingAttachmentBeforeAllocation(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := core.HarnessRequest{RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: testModel()}
	request.Network = " "
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("missing network = %v", err)
	}
	if fake.created.Name != "" {
		t.Fatal("allocated runtime without network attachment")
	}
}
