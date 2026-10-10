package hermes

import (
	harnesscommon "github.com/hyscale-lab/aries/pkg/harness"

	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	audioinput "github.com/hyscale-lab/aries/pkg/audio"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

const testHermesImage = "docker.io/nousresearch/hermes-agent:v2026.5.29.2"

type fakeDeployment struct {
	validatedRequest         deployment.Request
	createErr                error
	mu                       sync.Mutex
	created                  deployment.Request
	archive                  []byte
	execs                    []core.Command
	agentStdout              string
	agentStderr              string
	sttStdout                string
	sttStderr                string
	spansStdout, spansStderr string
	spansExit                int
	sessionsStdout           string
	sttExit, sessionsExit    int
	gatewayStatus            string
	gatewayError             string
	gatewayToken             string
	gatewayServer            *httptest.Server
	gatewayInputs            []string
	gatewaySession           string
	gatewayCancelStatus      int
	gatewayCancelCalls       int
	removed                  bool
	copyToErr                error
	logsErr                  error
	closeErr                 error
	validateErr              error
	stopErr                  error
	createCalls              int
	startCalls               int
	removeCalls              int
	closeCalls               int
	validatedSecrets         [][]byte
}

func newFakeDeployment(tests ...*testing.T) *fakeDeployment {
	f := &fakeDeployment{gatewayStatus: "completed", agentStdout: "the task is complete\n", agentStderr: "hermes diagnostic\n", sessionsStdout: "{\"role\":\"user\",\"content\":\"do the task\"}\n"}
	f.spansStdout = "{\"name\":\"tool.terminal\",\"start_time_unix_nano\":1,\"end_time_unix_nano\":2}\n"
	for _, t := range tests {
		t.Cleanup(f.closeGateway)
	}
	return f
}
func (f *fakeDeployment) Create(_ context.Context, r deployment.Request) (string, error) {
	if strings.TrimSpace(r.Placement.AttachmentID) == "" {
		return "", errors.New("missing task network placement")
	}
	f.created = r
	f.createCalls++
	return "hermes-id", f.createErr
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
func (f *fakeDeployment) DownloadArchive(context.Context, string, string) (io.ReadCloser, deployment.FileInfo, error) {
	return nil, deployment.FileInfo{}, errors.New("unexpected download")
}
func (f *fakeDeployment) Start(context.Context, string) error           { f.startCalls++; return nil }
func (f *fakeDeployment) Running(context.Context, string) (bool, error) { return !f.removed, nil }
func (f *fakeDeployment) Exec(ctx context.Context, _ string, c core.Command) (core.CommandResult, error) {
	if err := ctx.Err(); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	f.execs = append(f.execs, c)
	switch c.Path {
	case "hermes":
		return core.CommandResult{Stdout: f.sessionsStdout, ExitCode: f.sessionsExit}, nil
	case "python3":
		return core.CommandResult{Stdout: f.spansStdout, Stderr: f.spansStderr, ExitCode: f.spansExit}, nil
	case "/bin/sh":
		if len(c.Args) > 2 && c.Args[2] == "aries-hermes-stt" {
			return core.CommandResult{Stdout: f.sttStdout, Stderr: f.sttStderr, ExitCode: f.sttExit}, nil
		}
	}
	return core.CommandResult{}, nil
}
func (f *fakeDeployment) Logs(context.Context, string, int) ([]byte, error) {
	return []byte("container ready\n"), f.logsErr
}
func (f *fakeDeployment) Address(context.Context, string, int) (string, error) {
	if f.gatewayServer == nil {
		// Authentication comes from the staged private Gateway credential, not model metadata.
		reader := tar.NewReader(bytes.NewReader(f.archive))
		for {
			header, err := reader.Next()
			if err != nil {
				break
			}
			if header.Name == strings.TrimPrefix(gatewayKeyPath, "/") {
				content, _ := io.ReadAll(reader)
				f.gatewayToken = string(content)
			}
		}
		f.gatewayServer = httptest.NewServer(http.HandlerFunc(f.serveGateway))
	}
	return strings.TrimPrefix(f.gatewayServer.URL, "http://"), nil
}
func (f *fakeDeployment) closeGateway() {
	if f.gatewayServer != nil {
		f.gatewayServer.Close()
	}
}
func (f *fakeDeployment) serveGateway(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.gatewayToken == "" || r.Header.Get("Authorization") != "Bearer "+f.gatewayToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/v1/models":
		_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
	case r.Method == "POST" && r.URL.Path == "/v1/runs":
		var request struct {
			Input     string `json:"input"`
			SessionID string `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		f.gatewayInputs = append(f.gatewayInputs, request.Input)
		f.gatewaySession = request.SessionID
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"run_id":"test-run","status":"started"}`)
	case r.Method == "GET" && r.URL.Path == "/v1/runs/test-run":
		_ = json.NewEncoder(w).Encode(map[string]string{"run_id": "test-run", "session_id": f.gatewaySession, "status": f.gatewayStatus, "output": f.agentStdout, "error": f.gatewayError})
	case r.Method == "POST" && r.URL.Path == "/v1/runs/test-run/stop":
		f.gatewayCancelCalls++
		if f.gatewayCancelStatus != 0 {
			w.WriteHeader(f.gatewayCancelStatus)
			return
		}
		_, _ = io.WriteString(w, `{"run_id":"test-run","status":"stopping"}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDeployment) Stop(context.Context, string) error {
	f.removeCalls++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.removed = true
	return nil
}
func (f *fakeDeployment) Close() error { f.closeCalls++; return f.closeErr }

func newTestManager(t *testing.T, fake *fakeDeployment, secret []byte) *Manager {
	t.Helper()
	t.Cleanup(fake.closeGateway)
	// Start clears the returned key buffer; retain the test's own copy.
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:   fake,
		Image:        testHermesImage,
		OutputDir:    t.TempDir(),
		StartTimeout: 2 * time.Second,
		AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(string) ([]byte, bool) { return bytes.Clone(secret), true },
	}})

	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }
	return manager
}

type stubSpeechSynthesizer struct {
	request *audioinput.SpeechRequest
}

func (stub stubSpeechSynthesizer) Synthesize(_ context.Context, request audioinput.SpeechRequest) (audioinput.SpeechResult, error) {
	if stub.request != nil {
		*stub.request = request
	}
	return audioinput.SpeechResult{
		Audio: []byte("fake-wav-audio"),
		Model: request.Model, Voice: request.Voice, Format: request.Format,
		TextSHA256: strings.Repeat("a", 64),
	}, nil
}

func (stubSpeechSynthesizer) Close() {}

func endpointFiles(t *testing.T) core.ToolEndpoint {
	t.Helper()
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries", Workdir: "/app",
	}
}

func testRequest(t *testing.T) core.HarnessRequest {
	t.Helper()
	return core.HarnessRequest{Connectivity: core.HarnessConnectivity{SearchURL: "http://search.example:8123", Placement: core.RuntimePlacement{AttachmentID: "aries-net-test"}}, RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: validModel()}
}

func archiveEntries(t *testing.T, archive []byte) map[string]*tar.Header {
	t.Helper()
	entries := make(map[string]*tar.Header)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		copied := *header
		entries[header.Name] = &copied
	}
	return entries
}

func archiveContents(t *testing.T, archive []byte) map[string][]byte {
	t.Helper()
	entries := make(map[string][]byte)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
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
		entries[header.Name] = content
	}
	return entries
}

func TestManagerCloseIsIdempotent(t *testing.T) {
	failure := errors.New("close failed")
	fake := newFakeDeployment(t)
	fake.closeErr = failure
	manager := &Manager{runtime: &harnesscommon.Runtime{Options: harnesscommon.RuntimeOptions{Deployment: fake}}}
	if err := manager.Close(); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if err := manager.Close(); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
	if fake.closeCalls != 1 {
		t.Fatalf("close calls = %d", fake.closeCalls)
	}
}

func TestStartStagesPrivateRuntimeAndPinsGatewayService(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	config := fake.created
	if !slices.Equal(config.Entrypoint, gatewayEntrypoint) || !slices.Equal(config.Args, gatewayCommand) {
		t.Fatalf("container is not pinned to the Gateway command: %v %v", config.Entrypoint, config.Args)
	}
	if config.Labels["aries.component"] != "harness" || config.Labels["aries.kind"] != "hermes-harness" || config.Labels["aries.managed"] != "true" {
		t.Fatalf("labels = %v", config.Labels)
	}
	if string(fake.created.Placement.AttachmentID) != "aries-net-test" {
		t.Fatalf("network = %v", fake.created.Placement.AttachmentID)
	}

	entries := archiveEntries(t, fake.archive)
	for name := range entries {
		if strings.HasPrefix(name, "run/aries/ssh/") {
			t.Fatalf("staged obsolete SSH credential: %s", name)
		}
	}
	for name, mode := range map[string]int64{
		strings.TrimPrefix(configContainerPath, "/"): 0o600,
		strings.TrimPrefix(gatewayKeyPath, "/"):      0o600,
		strings.TrimPrefix(modelKeyPath, "/"):        0o600,
		strings.TrimPrefix(gatewayLauncherPath, "/"): 0o555,
	} {
		header, ok := entries[name]
		if !ok {
			t.Fatalf("staged archive is missing %s", name)
		}
		if header.Mode != mode {
			t.Fatalf("%s mode = %o, want %o", name, header.Mode, mode)
		}
	}
	if fake.gatewayToken == "model-secret" || len(fake.gatewayToken) < 32 {
		t.Fatal("Gateway must have an independent private credential")
	}
	for _, value := range append(append([]string(nil), config.Env...), config.Args...) {
		if strings.Contains(value, fake.gatewayToken) {
			t.Fatal("Gateway credential leaked into runtime metadata")
		}
	}
	// Every staged entry must be owned by the image's unprivileged `hermes`
	// user. The PATH shim drops root to that UID before exec'ing the real
	// binary, so root-owned staging leaves Hermes unable to read its own
	// configuration — and the failure appears only once the agent runs.
	for name, header := range entries {
		if header.Uid != runtimeUID || header.Gid != runtimeGID {
			t.Fatalf("%s owned by %d:%d, want %d:%d", name, header.Uid, header.Gid, runtimeUID, runtimeGID)
		}
	}
}

// Hermes must run agent commands where the bridge does: the rendered terminal
// section and TERMINAL_CWD both name the endpoint's workdir.
func TestStartNamesTheBridgeWorkdirToTheTerminal(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	retained, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("\nterminal:\n  backend: \"ssh\"\n  cwd: \"/app\"\n  timeout: %d\n", manager.options.TerminalTimeout)
	if !strings.Contains(string(retained), want) {
		t.Fatalf("config.yaml lacks the terminal section %q:\n%s", want, retained)
	}
	if !slices.Contains(fake.created.Env, "TERMINAL_CWD=/app") {
		t.Fatalf("TERMINAL_CWD does not name the bridge's workdir: %v", fake.created.Env)
	}
}

func TestStartRefusesEndpointWithoutWorkdir(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	request.Endpoint.Workdir = ""
	err := manager.Start(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "workdir") {
		t.Fatalf("Start without an endpoint workdir: %v", err)
	}
}

// The credential may live only in the staged key file, never in Docker's
// container configuration, environment, labels, or the retained config.
func TestStartKeepsCredentialOutOfDockerMetadataAndArtifacts(t *testing.T) {
	secret := []byte("sk-super-secret-value")
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, secret)
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	config := fake.created
	for _, value := range append(append([]string(nil), config.Env...), append(config.Args, config.Entrypoint...)...) {
		if strings.Contains(value, string(secret)) {
			t.Fatalf("secret leaked into Docker configuration: %q", value)
		}
	}
	for name, value := range config.Labels {
		if strings.Contains(value, string(secret)) {
			t.Fatalf("secret leaked into label %s", name)
		}
	}
	retained, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(retained, secret) {
		t.Fatalf("secret leaked into the retained config:\n%s", retained)
	}
	if !bytes.Contains(retained, []byte("${DEEPSEEK_API_KEY}")) {
		t.Fatalf("retained config lost its credential reference:\n%s", retained)
	}
	entries := archiveEntries(t, fake.archive)
	if entries[strings.TrimPrefix(modelKeyPath, "/")].Size != int64(len(secret)) {
		t.Fatal("staged key file does not carry the credential")
	}
}

// When extract is configured, the Tavily key must be staged in its own file,
// exported under Hermes's fixed name by the launcher, rendered into
// config.yaml's extract_backend, and kept out of Docker metadata and the
// retained config — the same guarantees the model key already has.
func TestStartStagesExtractKeyWhenConfigured(t *testing.T) {
	modelSecret := []byte("model-secret")
	extractSecret := []byte("tvly-super-secret")
	fake := newFakeDeployment(t)
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:   fake,
		Image:        testHermesImage,
		OutputDir:    t.TempDir(),
		StartTimeout: 2 * time.Second,
		AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(name string) ([]byte, bool) {
			if name == "TAVILY_API_KEY" {
				return bytes.Clone(extractSecret), true
			}
			return bytes.Clone(modelSecret), true
		},
	}, Common: harnesscommon.Options{WebSearchEnabled: true, ExtractAPIKeyEnv: "TAVILY_API_KEY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }

	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	entries := archiveEntries(t, fake.archive)
	extractEntry, ok := entries[strings.TrimPrefix(extractKeyPath, "/")]
	if !ok {
		t.Fatal("staged archive is missing the extract key file")
	}
	if extractEntry.Mode != 0o600 || extractEntry.Size != int64(len(extractSecret)) {
		t.Fatalf("extract key entry = %+v", extractEntry)
	}

	config := fake.created
	for _, value := range append(append([]string(nil), config.Env...), append(config.Args, config.Entrypoint...)...) {
		if strings.Contains(value, string(extractSecret)) {
			t.Fatalf("extract secret leaked into Docker configuration: %q", value)
		}
	}
	retained, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(retained, extractSecret) {
		t.Fatalf("extract secret leaked into the retained config:\n%s", retained)
	}
	if !bytes.Contains(retained, []byte(`extract_backend: "tavily"`)) {
		t.Fatalf("retained config is missing extract_backend:\n%s", retained)
	}
}

// Missing extract credentials must fail Start and leave no partial container,
// the same contract the model key already has.
func TestStartRequiresPresentExtractCredential(t *testing.T) {
	fake := newFakeDeployment(t)
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:   fake,
		Image:        testHermesImage,
		OutputDir:    t.TempDir(),
		StartTimeout: 2 * time.Second,
		AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(name string) ([]byte, bool) {
			if name == "TAVILY_API_KEY" {
				return nil, false
			}
			return []byte("model-secret"), true
		},
	}, Common: harnesscommon.Options{WebSearchEnabled: true, ExtractAPIKeyEnv: "TAVILY_API_KEY"},
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil {
		t.Fatal("missing extract credential was accepted")
	}
	if fake.createCalls != 0 {
		t.Fatalf("container was created despite the missing extract credential: %d calls", fake.createCalls)
	}
}

func TestRunReturnsFinalResponseAndArtifacts(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Run(context.Background(), "fix the git repository")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "the task is complete" {
		t.Fatalf("result = %#v", result)
	}
	artifacts := filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness")
	for _, name := range []string{"config.yaml", "gateway-output.txt", "container.log", "telemetry.index.json", filepath.Join("telemetry", "sessions.jsonl"), filepath.Join("telemetry", "otel-spans.jsonl")} {
		if _, err := os.Stat(filepath.Join(artifacts, name)); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
	if len(result.LogPaths) == 0 {
		t.Fatal("result carries no log paths")
	}
	// Native submission preserves the instruction and task session exactly once.
	if !slices.Equal(fake.gatewayInputs, []string{"fix the git repository"}) || fake.gatewaySession != "aries-attempt" {
		t.Fatalf("Gateway input/session = %#v %q", fake.gatewayInputs, fake.gatewaySession)
	}
	for _, command := range fake.execs {
		if command.Path == gatewayLauncherPath {
			t.Fatal("agent task was submitted through exec")
		}
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceTranscribeSynthesizesAudioAndRunsTranscriptAsAgentMessage(t *testing.T) {
	modelSecret := []byte("model-secret")
	voiceSecret := []byte("voice-secret")
	fake := newFakeDeployment(t)
	fake.sttStdout = `{"transcript":"fix the repository from speech","signature":"transcribe_recording(wav_path, provider=None, model=None)"}` + "\n"
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

		Deployment:   fake,
		Image:        testHermesImage,
		OutputDir:    t.TempDir(),
		StartTimeout: 2 * time.Second,
		AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(name string) ([]byte, bool) {
			switch name {
			case "OPENAI_API_KEY":
				return bytes.Clone(voiceSecret), true
			default:
				return bytes.Clone(modelSecret), true
			}
		},
	},

		VoiceTranscribe: VoiceTranscribeOptions{
			TTS: harnesscommon.TTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4o-mini-tts", Voice: "alloy", Timeout: time.Second},
			STT: VoiceSTTOptions{Provider: "openai", Model: "gpt-4o-mini-transcribe", Language: "en", Timeout: time.Second},
		}, Common: harnesscommon.Options{Mode: ModeVoiceTranscribe},
	})
	if err != nil {
		t.Fatal(err)
	}
	var speechRequest audioinput.SpeechRequest
	manager.newID = func() (string, error) { return "attempt", nil }
	manager.newSpeech = func(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
		if string(options.APIKey) != string(voiceSecret) || options.Timeout != time.Second {
			t.Fatalf("speech options = %#v", options)
		}
		return stubSpeechSynthesizer{request: &speechRequest}, nil
	}

	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	runtimeArchive := bytes.Clone(fake.archive)
	result, err := manager.Run(context.Background(), "fix the git repository by voice")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != core.StatusSucceeded || result.FinalResponse != "the task is complete" {
		t.Fatalf("result = %#v", result)
	}
	if speechRequest.Text != "fix the git repository by voice" || speechRequest.Model != "gpt-4o-mini-tts" || speechRequest.Voice != "alloy" || speechRequest.Format != "wav" {
		t.Fatalf("speech request = %#v", speechRequest)
	}
	retainedConfig, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(retainedConfig, []byte("provider: \"openai\"")) || !bytes.Contains(retainedConfig, []byte("model: \"gpt-4o-mini-transcribe\"")) {
		t.Fatalf("config is missing voice STT settings:\n%s", retainedConfig)
	}

	runtimeEntries := archiveContents(t, runtimeArchive)
	if !bytes.Equal(runtimeEntries[strings.TrimPrefix(voiceKeyPath, "/")], voiceSecret) {
		t.Fatal("runtime archive did not stage the voice API key")
	}
	stagedEntries := archiveContents(t, fake.archive)
	if !bytes.Equal(stagedEntries[strings.TrimPrefix(voiceWAVPath, "/")], []byte("fake-wav-audio")) {
		t.Fatal("voice WAV was not staged into the container")
	}
	stagedHeaders := archiveEntries(t, fake.archive)
	if len(stagedHeaders) != 1 {
		t.Fatalf("voice WAV archive rewrote runtime paths: %#v", stagedHeaders)
	}

	var sttCmd []string
	for _, options := range fake.execs {
		if len(options.Args) > 2 && options.Args[2] == "aries-hermes-stt" {
			sttCmd = append([]string{options.Path}, options.Args...)
		}
	}
	if len(sttCmd) != 9 || sttCmd[5] != voiceWAVPath || sttCmd[6] != "gpt-4o-mini-transcribe" || sttCmd[7] != "openai" || sttCmd[8] != "en" {
		t.Fatalf("stt exec argv = %#v", sttCmd)
	}
	if !slices.Equal(fake.gatewayInputs, []string{"fix the repository from speech"}) {
		t.Fatalf("Gateway input = %#v", fake.gatewayInputs)
	}

	artifacts := filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness")
	for _, name := range []string{"voice-instruction.txt", "voice-instruction.wav", "voice-instruction.wav.meta.json", "voice-transcript.txt", "voice-result.json"} {
		if _, err := os.Stat(filepath.Join(artifacts, name)); err != nil {
			t.Fatalf("missing voice artifact %s: %v", name, err)
		}
	}
	voiceResult, err := os.ReadFile(filepath.Join(artifacts, "voice-result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(voiceResult, []byte(`"agent_question_used": "fix the repository from speech"`)) || !bytes.Contains(voiceResult, []byte(`"agent_output_text": "the task is complete"`)) {
		t.Fatalf("voice result = %s", voiceResult)
	}
	config := fake.created
	for _, value := range append(append([]string(nil), config.Env...), append(config.Args, config.Entrypoint...)...) {
		if strings.Contains(value, string(voiceSecret)) {
			t.Fatalf("voice secret leaked into Docker configuration: %q", value)
		}
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceTranscribeMapsOpenAICompatibleAgentProvider(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		baseURL  string
	}{
		{name: "openai", provider: "openai", baseURL: "http://openai-compatible.local:8000/v1"},
		{name: "sglang", provider: "sglang", baseURL: "http://sglang.local:30000/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeDeployment(t)
			fake.sttStdout = `{"transcript":"fix from speech"}` + "\n"
			manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{

				Deployment:   fake,
				Image:        testHermesImage,
				OutputDir:    t.TempDir(),
				StartTimeout: 2 * time.Second,
				AgentTimeout: 2 * time.Second,
				APIKeyLookup: func(string) ([]byte, bool) { return []byte("secret"), true },
			},

				VoiceTranscribe: VoiceTranscribeOptions{
					TTS: harnesscommon.TTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4o-mini-tts", Voice: "alloy", Timeout: time.Second},
					STT: VoiceSTTOptions{Provider: "openai", Model: "gpt-4o-mini-transcribe", Language: "en", Timeout: time.Second},
				}, Common: harnesscommon.Options{Mode: ModeVoiceTranscribe},
			})
			if err != nil {
				t.Fatal(err)
			}
			manager.newID = func() (string, error) { return "attempt", nil }
			manager.newSpeech = func(audioinput.SpeechClientOptions) (speechSynthesizer, error) {
				return stubSpeechSynthesizer{}, nil
			}
			request := testRequest(t)
			request.Model.Provider = tc.provider
			request.Model.BaseURL = tc.baseURL
			request.Model.APIKeyEnv = strings.ToUpper(tc.provider) + "_API_KEY"
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Run(context.Background(), "fix by voice"); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(fake.gatewayInputs, []string{"fix from speech"}) {
				t.Fatalf("Gateway input = %#v", fake.gatewayInputs)
			}
			config, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(config, []byte(`provider: "custom"`)) {
				t.Fatalf("Gateway model provider not mapped: %s", config)
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunAcceptsExactlyOneInstruction(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if _, err := manager.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "second"); err == nil {
		t.Fatal("second instruction was accepted")
	}
	if !slices.Equal(fake.gatewayInputs, []string{"first"}) {
		t.Fatalf("submission repeated: %#v", fake.gatewayInputs)
	}
}

func TestRunRejectsInvalidInstructionAndUnstartedHarness(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if _, err := manager.Run(context.Background(), "task"); err == nil {
		t.Fatal("run before start was accepted")
	}
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	for _, instruction := range []string{"", "   ", "has\x00nul"} {
		if _, err := manager.Run(context.Background(), instruction); err == nil {
			t.Fatalf("instruction %q was accepted", instruction)
		}
	}
}

// Native run failure retains diagnostic evidence.
func TestRunReportsNativeFailureAndStillRetainsArtifacts(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.gatewayStatus = "failed"
	fake.gatewayError = "agent failed"
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "agent failed") {
		t.Fatalf("err = %v", err)
	}
	if result.Status != core.StatusFailed {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "session-outcome.json")); statErr != nil {
		t.Fatalf("native outcome artifact missing after failure: %v", statErr)
	}
}

func TestRunCancellationIsReportedAsCanceled(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := manager.Run(ctx, "task")
	if err == nil {
		t.Fatal("canceled run reported success")
	}
	if result.Status != core.StatusCanceled {
		t.Fatalf("status = %q, want %q", result.Status, core.StatusCanceled)
	}
}

// A run that never reached the model produces no session; that must not be
// reported as a harness failure.
func TestEmptySessionExportIsNotAFailure(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.sessionsStdout = ""
	fake.sessionsExit = 1
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("empty session export failed the run: %v", err)
	}
	if result.Status != core.StatusSucceeded {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "telemetry", "sessions.jsonl")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected sessions artifact: %v", statErr)
	}
}

// An image without the hermes_otel plugin leaves no span store; the dump
// exits 3 and the run must still succeed without a spans artifact.
func TestMissingSpanStoreIsNotAFailure(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.spansStdout = ""
	fake.spansExit = 3
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("missing span store failed the run: %v", err)
	}
	if result.Status != core.StatusSucceeded {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "telemetry", "otel-spans.jsonl")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected spans artifact: %v", statErr)
	}
}

// A store that exists but cannot be read is a dump failure, not "no spans":
// the run reports it instead of silently omitting the artifact.
func TestBrokenSpanStoreFailsTheRun(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.spansStdout = ""
	fake.spansStderr = "sqlite3.DatabaseError: file is not a database\n"
	fake.spansExit = 1
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "exited with code 1") || !strings.Contains(err.Error(), "file is not a database") {
		t.Fatalf("Run error = %v", err)
	}
	if result.Status != core.StatusFailed {
		t.Fatalf("status = %q", result.Status)
	}
}

// A dump that stopped at its byte limit keeps the spans that fit and does not
// fail the run.
func TestTruncatedSpanDumpKeepsWhatFit(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.spansStderr = "kept 1 of 2 spans\n"
	fake.spansExit = spansTruncated
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if _, err := manager.Run(context.Background(), "task"); err != nil {
		t.Fatalf("truncated dump failed the run: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "telemetry", "otel-spans.jsonl"))
	if err != nil || string(content) != fake.spansStdout {
		t.Fatalf("spans artifact = %q, %v", content, err)
	}
}

// The span dump is a fixed argv: the store path is its own element, never
// spliced into the script.
func TestSpanDumpUsesFixedArgv(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if _, err := manager.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	for _, command := range fake.execs {
		if command.Path == "python3" {
			if !slices.Equal(command.Args, []string{"-c", spanDumpScript, otelStorePath, strconv.Itoa(spanDumpLimit)}) {
				t.Fatalf("span dump argv = %q", command.Args)
			}
			return
		}
	}
	t.Fatal("no span dump exec")
}

func TestStopIsIdempotentAndConfirmsAbsence(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	for attempt := range 3 {
		if err := manager.Stop(context.Background()); err != nil {
			t.Fatalf("stop %d: %v", attempt, err)
		}
	}
	if fake.removeCalls != 1 {
		t.Fatalf("remove calls = %d, want 1", fake.removeCalls)
	}
	if !fake.removed {
		t.Fatal("container was not removed")
	}
	if manager.active != nil {
		t.Fatal("manager still holds an active session after stop")
	}
}

// Positive absence: if the container is still inspectable after removal, Stop
// must fail rather than report a clean teardown.
func TestStopFailsWhenContainerRemains(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.removeCalls = 0
	fake.mu.Unlock()
	fake.stopErr = errors.New("runtime remains after removal")
	if err := manager.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "remains after removal") {
		t.Fatalf("err = %v", err)
	}
	if manager.active == nil {
		t.Fatal("failed stop must retain session")
	}
	fake.stopErr = nil
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestStartRollsBackAndClearsArtifactsOnFailure(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.copyToErr = errors.New("copy refused")
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "copy private Hermes runtime") {
		t.Fatalf("err = %v", err)
	}
	if !fake.removed {
		t.Fatal("partial container was not removed")
	}
	if manager.active != nil {
		t.Fatal("manager kept an active session after a clean rollback")
	}
	if _, err := os.Stat(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact directory survived rollback: %v", err)
	}
}

func TestStartRejectsSecondSession(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if err := manager.Start(context.Background(), testRequest(t)); err == nil {
		t.Fatal("second Start was accepted")
	}
}

func TestStartValidatesIdentifiersAndTimeout(t *testing.T) {
	for _, test := range []struct {
		name string
		set  func(*core.HarnessRequest)
		want string
	}{
		{"run id", func(r *core.HarnessRequest) { r.RunID = "-bad" }, "Hermes run ID contains an unsafe character"},
		{"task id", func(r *core.HarnessRequest) { r.TaskID = "" }, "Hermes task ID is invalid"},
		{"timeout", func(r *core.HarnessRequest) { r.Timeout = -1 }, "Hermes task timeout must not be negative"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeDeployment(t)
			manager := newTestManager(t, fake, []byte("model-secret"))
			request := testRequest(t)
			test.set(&request)
			if err := manager.Start(context.Background(), request); err == nil || err.Error() != test.want {
				t.Fatalf("err = %v, want exactly %q", err, test.want)
			}
			if fake.createCalls != 0 {
				t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
			}
		})
	}
}

func TestStartRequiresPresentCredential(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, nil)
	manager.runtime.Options.APIKeyLookup = func(string) ([]byte, bool) { return nil, false }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v", err)
	}
	manager.runtime.Options.APIKeyLookup = func(string) ([]byte, bool) { return []byte("has\nnewline"), true }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil || !strings.Contains(err.Error(), "NUL or a line break") {
		t.Fatalf("err = %v", err)
	}
	if fake.createCalls != 0 {
		t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
	}
}

func TestNewRejectsUnpinnedImage(t *testing.T) {
	for _, image := range []string{"", "nousresearch/hermes-agent", "nousresearch/hermes-agent:latest"} {
		if _, err := New(Options{Runtime: harnesscommon.RuntimeOptions{Image: image, OutputDir: t.TempDir()}}); err == nil {
			t.Fatalf("image %q was accepted", image)
		}
	}
}

func TestRunOutcomeRecordsNativeTerminalState(t *testing.T) {
	for _, tc := range []struct {
		native string
		status string
		reason string
	}{
		{"completed", core.StatusSucceeded, "completed"},
		{"failed", core.StatusFailed, "failed"},
		{"cancelled", core.StatusCanceled, "cancelled"},
		{"interrupted", core.StatusCanceled, "interrupted"},
		{"running", core.StatusCanceled, "deadline_exceeded"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			fake := newFakeDeployment(t)
			fake.gatewayStatus = tc.native
			manager := newTestManager(t, fake, []byte("model-secret"))
			request := testRequest(t)
			if err := manager.Start(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			defer manager.Stop(context.Background())
			ctx := context.Background()
			if tc.native == "running" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer cancel()
			}
			result, runErr := manager.Run(ctx, "task")
			if result.Status != tc.status || (runErr == nil) != (tc.status == core.StatusSucceeded) {
				t.Fatalf("result=%#v error=%v", result, runErr)
			}
			if tc.reason == "deadline_exceeded" && !errors.Is(runErr, context.DeadlineExceeded) {
				t.Fatalf("deadline classification lost: %v", runErr)
			}
			if (tc.native == "cancelled" || tc.native == "interrupted") && !errors.Is(runErr, context.Canceled) {
				t.Fatalf("native cancellation classification lost: %v", runErr)
			}
			content, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "session-outcome.json"))
			if err != nil {
				t.Fatal(err)
			}
			var outcome runOutcome
			if err := json.Unmarshal(content, &outcome); err != nil {
				t.Fatal(err)
			}
			if outcome.Status != string(tc.status) || outcome.EndReason != tc.reason || outcome.RunID != "test-run" || outcome.SessionID != "aries-attempt" {
				t.Fatalf("outcome = %+v", outcome)
			}
			if outcome.StartedAt == "" || outcome.EndedAt < outcome.StartedAt || bytes.Contains(content, []byte("exit_code")) {
				t.Fatalf("invalid Gateway timing/meaning: %s", content)
			}
			if len(fake.gatewayInputs) != 1 {
				t.Fatalf("submissions = %d", len(fake.gatewayInputs))
			}
		})
	}
}

func TestGatewayCancelFailureDoesNotPreventRuntimeRemoval(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.gatewayCancelStatus = http.StatusServiceUnavailable
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "task"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.gatewayCancelCalls != 1 || !fake.removed {
		t.Fatalf("cancel calls=%d removed=%v", fake.gatewayCancelCalls, fake.removed)
	}
}

func TestStartRequiresDeployment(t *testing.T) {
	if _, err := New(Options{Runtime: harnesscommon.RuntimeOptions{Image: testHermesImage, OutputDir: t.TempDir()}}); err == nil || !strings.Contains(err.Error(), "deployment is required") {
		t.Fatalf("New() = %v", err)
	}
}
func TestStartRollsBackDeploymentValidationFailure(t *testing.T) {
	fake := newFakeDeployment(t)
	fake.validateErr = errors.New("unsafe deployment")
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); !errors.Is(err, fake.validateErr) {
		t.Fatalf("Start() = %v", err)
	}
	if !fake.removed || fake.startCalls != 0 {
		t.Fatalf("removed=%v starts=%d", fake.removed, fake.startCalls)
	}
}

func TestDeploymentReceivesRuntimeConstraintsAndSecretValidation(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	cpu, memory := 2.5, 1536
	request.CPU, request.MemoryMB = &cpu, &memory
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	if fake.created.CPU == nil || *fake.created.CPU != cpu || fake.created.MemoryMB == nil || *fake.created.MemoryMB != memory {
		t.Fatalf("resources = %#v", fake.created)
	}
	if got := fake.validatedRequest; got.Name != fake.created.Name || got.Image != testHermesImage || got.Placement.AttachmentID != request.Connectivity.Placement.AttachmentID || !slices.Equal(got.Args, gatewayCommand) || !slices.Equal(got.Entrypoint, gatewayEntrypoint) {
		t.Fatalf("validation request = %#v", got)
	}
	if !slices.Equal(fake.created.ImageVolumes, []string{imageDeclaredVolume}) {
		t.Fatalf("image volumes = %v", fake.created.ImageVolumes)
	}
	if len(fake.validatedSecrets) == 0 || string(fake.validatedSecrets[0]) != "model-secret" {
		t.Fatal("deployment validation did not receive model credential")
	}
}

func (*fakeDeployment) ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error) {
	return core.CommandResult{}, errors.New("unexpected harness streaming call")
}
func (*fakeDeployment) LogsStream(context.Context, string, io.Writer, io.Writer) error {
	return errors.New("unexpected harness streaming logs call")
}

func TestStartRejectsMissingAttachmentBeforeAllocation(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	request.Connectivity.Placement = core.RuntimePlacement{}
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("missing network = %v", err)
	}
	if fake.created.Name != "" {
		t.Fatal("allocated runtime without network attachment")
	}
}

// A benchmark's credentials that reach the sandbox are never given to Hermes,
// but the agent can read them there and repeat them. The values RedactEnv
// names, and the parts of a key file, are scrubbed from every artifact the
// harness saves; an unset variable is skipped.
func TestRunScrubsRedactEnvValuesFromSavedArtifacts(t *testing.T) {
	fake := newFakeDeployment(t)
	token := "ghp_benchmarktoken123"
	clientEmail := "agent@project.iam.example.com"
	keyFile := `{"client_email": "` + clientEmail + `", "type": "service_account"}`
	fake.sessionsStdout = `{"role":"tool","content":"github_token = \"` + token + `\""}` + "\n" +
		`{"role":"tool","content":"client ` + clientEmail + `"}` + "\n"
	fake.agentStdout = "used " + token + "\n"
	manager, err := New(Options{Runtime: harnesscommon.RuntimeOptions{
		Deployment:   fake,
		Image:        testHermesImage,
		OutputDir:    t.TempDir(),
		StartTimeout: 2 * time.Second,
		AgentTimeout: 2 * time.Second,
		APIKeyLookup: func(name string) ([]byte, bool) {
			switch name {
			case "TOOLATHLON_GITHUB_TOKEN":
				return []byte(token), true
			case "TOOLATHLON_GOOGLE_KEY":
				return []byte(keyFile), true
			case "UNSET_TOKEN":
				return nil, false
			}
			return []byte("model-secret"), true
		},
	}, Common: harnesscommon.Options{RedactEnv: []string{"TOOLATHLON_GITHUB_TOKEN", "TOOLATHLON_GOOGLE_KEY", "UNSET_TOKEN"}}})
	if err != nil {
		t.Fatal(err)
	}
	manager.newID = func() (string, error) { return "attempt", nil }
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Run(context.Background(), "fix the git repository"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	artifacts := filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness")
	scrubbed := false
	err = filepath.WalkDir(artifacts, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte(token)) || bytes.Contains(content, []byte(clientEmail)) {
			t.Errorf("%s kept a benchmark credential:\n%s", path, content)
		}
		scrubbed = scrubbed || bytes.Contains(content, []byte("[REDACTED]"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !scrubbed {
		t.Fatal("no artifact carries the placeholder")
	}
}

// The task's in-sandbox MCP servers, resolved by the deployment, are
// rendered ahead of the profile's; a clash with a profile server's name
// fails Start before anything is created.
func TestStartRendersTheTaskMCPServers(t *testing.T) {
	fake := newFakeDeployment(t)
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	request.Connectivity.MCPServers = []core.MCPServerConfig{{Name: "toolathlon", URL: "http://sandbox-a:10086/sse", Transport: "sse", TimeoutSeconds: 1200}}
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	retained, err := os.ReadFile(filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "  toolathlon:\n    url: \"http://sandbox-a:10086/sse\"\n    transport: \"sse\"\n    timeout: 1200\n"; !strings.Contains(string(retained), want) {
		t.Fatalf("config.yaml lacks the task's server %q:\n%s", want, retained)
	}

	clash := newFakeDeployment(t)
	other := newTestManager(t, clash, []byte("model-secret"))
	other.options.Common.MCPServers = []core.MCPServerConfig{{Name: "toolathlon", URL: "https://example.invalid/mcp"}}
	if err := other.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "toolathlon") {
		t.Fatalf("a name clash was accepted: %v", err)
	}
	if clash.createCalls != 0 {
		t.Fatalf("a runtime was created despite the clash: %d", clash.createCalls)
	}
}
