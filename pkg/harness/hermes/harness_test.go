package hermes

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	validatedRequest                 deployment.Request
	createErr                        error
	mu                               sync.Mutex
	created                          deployment.Request
	archive                          []byte
	execs                            []core.Command
	agentStdout                      string
	agentStderr                      string
	sttStdout                        string
	sttStderr                        string
	sessionsStdout                   string
	agentExit, sttExit, sessionsExit int
	removed                          bool
	copyToErr                        error
	logsErr                          error
	closeErr                         error
	validateErr                      error
	stopErr                          error
	createCalls                      int
	startCalls                       int
	removeCalls                      int
	closeCalls                       int
	validatedSecrets                 [][]byte
}

func newFakeDeployment() *fakeDeployment {
	return &fakeDeployment{agentStdout: "the task is complete\n", agentStderr: "hermes diagnostic\n", sessionsStdout: "{\"role\":\"user\",\"content\":\"do the task\"}\n"}
}
func (f *fakeDeployment) Create(_ context.Context, r deployment.Request) (string, error) {
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
	case agentWrapperPath:
		return core.CommandResult{Stdout: f.agentStdout, Stderr: f.agentStderr, ExitCode: f.agentExit}, nil
	case "hermes":
		return core.CommandResult{Stdout: f.sessionsStdout, ExitCode: f.sessionsExit}, nil
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
	return "", errors.New("unexpected address")
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
	manager, err := New(Options{
		Deployment: fake,
		Image:      testHermesImage, OutputDir: t.TempDir(), StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
		// Start clears the buffer the lookup hands it, so the test keeps its
		// own copy for later assertions.
		APIKeyLookup: func(string) ([]byte, bool) { return bytes.Clone(secret), true },
	})
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
	root := t.TempDir()
	path := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(path, []byte("identity"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return core.ToolEndpoint{
		Protocol: "ssh", Address: "172.22.0.1:39425", Username: "aries",
		IdentityFile: identityContainerFS, IdentitySourceFile: path, Workdir: "/app",
	}
}

func testRequest(t *testing.T) core.HarnessRequest {
	t.Helper()
	return core.HarnessRequest{Network: "aries-net-test", RunID: "run-1", TaskID: "fix-git", Endpoint: endpointFiles(t), Model: validModel()}
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
	fake := newFakeDeployment()
	fake.closeErr = failure
	manager := &Manager{deployment: fake}
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

func TestStartStagesPrivateRuntimeAndPinsIdleContainer(t *testing.T) {
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	config := fake.created
	if !slices.Equal(config.Entrypoint, idleEntrypoint) || !slices.Equal(config.Args, idleCommand) {
		t.Fatalf("container is not pinned to the idle command: %v %v", config.Entrypoint, config.Args)
	}
	if config.Labels["aries.component"] != "harness" || config.Labels["aries.kind"] != "hermes-harness" || config.Labels["aries.managed"] != "true" {
		t.Fatalf("labels = %v", config.Labels)
	}
	if string(fake.created.Network) != "aries-net-test" {
		t.Fatalf("network = %v", fake.created.Network)
	}

	entries := archiveEntries(t, fake.archive)
	for name, mode := range map[string]int64{
		strings.TrimPrefix(configContainerPath, "/"): 0o600,
		strings.TrimPrefix(modelKeyPath, "/"):        0o600,
		strings.TrimPrefix(identityContainerFS, "/"): 0o600,
		strings.TrimPrefix(agentWrapperPath, "/"):    0o555,
	} {
		header, ok := entries[name]
		if !ok {
			t.Fatalf("staged archive is missing %s", name)
		}
		if header.Mode != mode {
			t.Fatalf("%s mode = %o, want %o", name, header.Mode, mode)
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
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())

	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("\nterminal:\n  backend: \"ssh\"\n  cwd: \"/app\"\n  timeout: %d\n", manager.terminalTimeout)
	if !strings.Contains(string(retained), want) {
		t.Fatalf("config.yaml lacks the terminal section %q:\n%s", want, retained)
	}
	if !slices.Contains(fake.created.Env, "TERMINAL_CWD=/app") {
		t.Fatalf("TERMINAL_CWD does not name the bridge's workdir: %v", fake.created.Env)
	}
}

func TestStartRefusesEndpointWithoutWorkdir(t *testing.T) {
	fake := newFakeDeployment()
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
	fake := newFakeDeployment()
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
	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "config.yaml"))
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
// exported under Hermes's fixed name by the wrapper, rendered into
// config.yaml's extract_backend, and kept out of Docker metadata and the
// retained config — the same guarantees the model key already has.
func TestStartStagesExtractKeyWhenConfigured(t *testing.T) {
	modelSecret := []byte("model-secret")
	extractSecret := []byte("tvly-super-secret")
	fake := newFakeDeployment()
	manager, err := New(Options{
		Deployment: fake,
		Image:      testHermesImage, OutputDir: t.TempDir(), StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
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
	retained, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "config.yaml"))
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
	fake := newFakeDeployment()
	manager, err := New(Options{
		Deployment: fake,
		Image:      testHermesImage, OutputDir: t.TempDir(), StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
		WebSearchEnabled: true, ExtractAPIKeyEnv: "TAVILY_API_KEY",
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
	if err := manager.Start(context.Background(), testRequest(t)); err == nil {
		t.Fatal("missing extract credential was accepted")
	}
	if fake.createCalls != 0 {
		t.Fatalf("container was created despite the missing extract credential: %d calls", fake.createCalls)
	}
}

func TestRunReturnsFinalResponseAndArtifacts(t *testing.T) {
	fake := newFakeDeployment()
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
	artifacts := filepath.Join(manager.outputDir, request.TaskID, "harness")
	for _, name := range []string{"config.yaml", "hermes_stdout.log", "hermes_stderr.log", "container.log", "telemetry.index.json", filepath.Join("telemetry", "sessions.jsonl")} {
		if _, err := os.Stat(filepath.Join(artifacts, name)); err != nil {
			t.Fatalf("missing artifact %s: %v", name, err)
		}
	}
	if len(result.LogPaths) == 0 {
		t.Fatal("result carries no log paths")
	}
	// The instruction must reach Hermes as one argv element, not a shell string.
	var agentCmd []string
	for _, options := range fake.execs {
		if options.Path == agentWrapperPath {
			agentCmd = append([]string{options.Path}, options.Args...)
		}
	}
	if len(agentCmd) != 4 || agentCmd[1] != "deepseek-v4-flash" || agentCmd[2] != "deepseek" || agentCmd[3] != "fix the git repository" {
		t.Fatalf("agent exec argv = %#v", agentCmd)
	}
	if err := manager.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceTranscribeSynthesizesAudioAndRunsTranscriptAsAgentMessage(t *testing.T) {
	modelSecret := []byte("model-secret")
	voiceSecret := []byte("voice-secret")
	fake := newFakeDeployment()
	fake.sttStdout = `{"transcript":"fix the repository from speech","signature":"transcribe_recording(wav_path, provider=None, model=None)"}` + "\n"
	manager, err := New(Options{
		Deployment: fake,
		Image:      testHermesImage, OutputDir: t.TempDir(), StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
		Mode: ModeVoiceTranscribe,
		VoiceTranscribe: VoiceTranscribeOptions{
			TTS: VoiceTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4o-mini-tts", Voice: "alloy", Timeout: time.Second},
			STT: VoiceSTTOptions{Provider: "openai", Model: "gpt-4o-mini-transcribe", Language: "en", Timeout: time.Second},
		},
		APIKeyLookup: func(name string) ([]byte, bool) {
			switch name {
			case "OPENAI_API_KEY":
				return bytes.Clone(voiceSecret), true
			default:
				return bytes.Clone(modelSecret), true
			}
		},
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
	retainedConfig, err := os.ReadFile(filepath.Join(manager.outputDir, request.TaskID, "harness", "config.yaml"))
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

	var sttCmd, agentCmd []string
	for _, options := range fake.execs {
		if len(options.Args) > 2 && options.Args[2] == "aries-hermes-stt" {
			sttCmd = append([]string{options.Path}, options.Args...)
		}
		if options.Path == agentWrapperPath {
			agentCmd = append([]string{options.Path}, options.Args...)
		}
	}
	if len(sttCmd) != 9 || sttCmd[5] != voiceWAVPath || sttCmd[6] != "gpt-4o-mini-transcribe" || sttCmd[7] != "openai" || sttCmd[8] != "en" {
		t.Fatalf("stt exec argv = %#v", sttCmd)
	}
	if len(agentCmd) != 4 || agentCmd[3] != "fix the repository from speech" {
		t.Fatalf("agent exec argv = %#v", agentCmd)
	}

	artifacts := filepath.Join(manager.outputDir, request.TaskID, "harness")
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
			fake := newFakeDeployment()
			fake.sttStdout = `{"transcript":"fix from speech"}` + "\n"
			manager, err := New(Options{
				Deployment: fake,
				Image:      testHermesImage, OutputDir: t.TempDir(), StartTimeout: 2 * time.Second, AgentTimeout: 2 * time.Second,
				Mode: ModeVoiceTranscribe,
				VoiceTranscribe: VoiceTranscribeOptions{
					TTS: VoiceTTSOptions{Provider: "openai", APIKeyEnv: "OPENAI_API_KEY", Model: "gpt-4o-mini-tts", Voice: "alloy", Timeout: time.Second},
					STT: VoiceSTTOptions{Provider: "openai", Model: "gpt-4o-mini-transcribe", Language: "en", Timeout: time.Second},
				},
				APIKeyLookup: func(string) ([]byte, bool) { return []byte("secret"), true },
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
			var agentCmd []string
			for _, options := range fake.execs {
				if options.Path == agentWrapperPath {
					agentCmd = append([]string{options.Path}, options.Args...)
				}
			}
			if len(agentCmd) != 4 || agentCmd[2] != "custom" || agentCmd[3] != "fix from speech" {
				t.Fatalf("agent exec argv = %#v", agentCmd)
			}
			if err := manager.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunAcceptsExactlyOneInstruction(t *testing.T) {
	fake := newFakeDeployment()
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
}

func TestRunRejectsInvalidInstructionAndUnstartedHarness(t *testing.T) {
	fake := newFakeDeployment()
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

// A non-zero Hermes exit is a harness failure, but the artifacts collected on
// the way out must still be retained for diagnosis.
func TestRunReportsNonZeroExitAndStillRetainsArtifacts(t *testing.T) {
	fake := newFakeDeployment()
	fake.agentExit = 1
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	if err := manager.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background())
	result, err := manager.Run(context.Background(), "task")
	if err == nil || !strings.Contains(err.Error(), "exited with status 1") {
		t.Fatalf("err = %v", err)
	}
	if result.Status != core.StatusFailed {
		t.Fatalf("status = %q", result.Status)
	}
	if _, statErr := os.Stat(filepath.Join(manager.outputDir, request.TaskID, "harness", "hermes_stderr.log")); statErr != nil {
		t.Fatalf("stderr artifact missing after failure: %v", statErr)
	}
}

func TestRunCancellationIsReportedAsCanceled(t *testing.T) {
	fake := newFakeDeployment()
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
	fake := newFakeDeployment()
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
	if _, statErr := os.Stat(filepath.Join(manager.outputDir, request.TaskID, "harness", "telemetry", "sessions.jsonl")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unexpected sessions artifact: %v", statErr)
	}
}

func TestStopIsIdempotentAndConfirmsAbsence(t *testing.T) {
	fake := newFakeDeployment()
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
	fake := newFakeDeployment()
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
	fake := newFakeDeployment()
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
	if _, err := os.Stat(filepath.Join(manager.outputDir, request.TaskID, "harness")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact directory survived rollback: %v", err)
	}
}

func TestStartRejectsSecondSession(t *testing.T) {
	fake := newFakeDeployment()
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
			fake := newFakeDeployment()
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
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, nil)
	manager.apiKeyLookup = func(string) ([]byte, bool) { return nil, false }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil || !strings.Contains(err.Error(), "is not set") {
		t.Fatalf("err = %v", err)
	}
	manager.apiKeyLookup = func(string) ([]byte, bool) { return []byte("has\nnewline"), true }
	if err := manager.Start(context.Background(), testRequest(t)); err == nil || !strings.Contains(err.Error(), "NUL or a line break") {
		t.Fatalf("err = %v", err)
	}
	if fake.createCalls != 0 {
		t.Fatalf("ContainerCreate calls = %d, want zero", fake.createCalls)
	}
}

func TestNewRejectsUnpinnedImage(t *testing.T) {
	for _, image := range []string{"", "nousresearch/hermes-agent", "nousresearch/hermes-agent:latest"} {
		if _, err := New(Options{Image: image, OutputDir: t.TempDir()}); err == nil {
			t.Fatalf("image %q was accepted", image)
		}
	}
}

// TestRunOutcomeRecordsTerminalState pins the artifact that closes the gap in
// Hermes's own export: its session store leaves ended_at and end_reason null
// for one-shot runs, so the exit trailer is the only positive terminal record.
func TestRunOutcomeRecordsTerminalState(t *testing.T) {
	started := time.Now().Add(-2 * time.Second)
	for _, testCase := range []struct {
		name      string
		exitCode  int
		runErr    error
		status    string
		endReason string
	}{
		{"completed", 0, nil, string(core.StatusSucceeded), "completed"},
		{"nonzero exit", 3, nil, string(core.StatusFailed), "nonzero_exit"},
		{"deadline", -1, context.DeadlineExceeded, string(core.StatusCanceled), "deadline_exceeded"},
		{"canceled", -1, context.Canceled, string(core.StatusCanceled), "canceled"},
		{"exec error", -1, errors.New("attach failed"), string(core.StatusFailed), "exec_error"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			outcome := newRunOutcome(started, testCase.exitCode, testCase.runErr)
			if outcome.Status != testCase.status || outcome.EndReason != testCase.endReason {
				t.Fatalf("outcome = %+v, want status %q reason %q", outcome, testCase.status, testCase.endReason)
			}
			if outcome.ExitCode != testCase.exitCode {
				t.Fatalf("exit code = %d, want %d", outcome.ExitCode, testCase.exitCode)
			}
			// The fields Hermes leaves null must both be populated and ordered.
			if outcome.StartedAt == "" || outcome.EndedAt == "" || outcome.DurationMS <= 0 {
				t.Fatalf("timings = %+v", outcome)
			}
			if outcome.EndedAt < outcome.StartedAt {
				t.Fatalf("ended_at %q precedes started_at %q", outcome.EndedAt, outcome.StartedAt)
			}
		})
	}
}

func TestStartRequiresDeployment(t *testing.T) {
	if _, err := New(Options{Image: testHermesImage, OutputDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "deployment is required") {
		t.Fatalf("New() = %v", err)
	}
}
func TestStartRollsBackDeploymentValidationFailure(t *testing.T) {
	fake := newFakeDeployment()
	fake.validateErr = errors.New("unsafe deployment")
	manager := newTestManager(t, fake, []byte("model-secret"))
	if err := manager.Start(context.Background(), testRequest(t)); !errors.Is(err, fake.validateErr) {
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
	if err := manager.Start(context.Background(), testRequest(t)); !errors.Is(err, fake.createErr) {
		t.Fatalf("Start() = %v", err)
	}
	if !fake.removed || manager.active != nil {
		t.Fatal("partial deployment was not cleaned up")
	}
}

func TestDeploymentReceivesRuntimeConstraintsAndSecretValidation(t *testing.T) {
	fake := newFakeDeployment()
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
	if got := fake.validatedRequest; got.Name != fake.created.Name || got.Image != testHermesImage || got.Network != request.Network || !slices.Equal(got.Args, idleCommand) || !slices.Equal(got.Entrypoint, idleEntrypoint) {
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
	fake := newFakeDeployment()
	manager := newTestManager(t, fake, []byte("model-secret"))
	request := testRequest(t)
	request.Network = " "
	if err := manager.Start(context.Background(), request); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("missing network = %v", err)
	}
	if fake.created.Name != "" {
		t.Fatal("allocated runtime without network attachment")
	}
}
