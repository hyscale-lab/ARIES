package hermes

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	audioinput "github.com/hyscale-lab/aries/pkg/audio"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultVoiceSTTTimeout = 5 * time.Minute
	defaultMaxTurns        = 90
	defaultTerminalTimeout = 180
	maxDockerOutput        = 16 << 20

	// imageDeclaredVolume is the upstream image's own VOLUME. ARIES does not
	// use it — HERMES_HOME is relocated to a staged private directory — but
	// Docker creates it regardless.
	imageDeclaredVolume = "/opt/data"

	// runtimeUID is the unprivileged `hermes` user baked into the upstream
	// image. `/opt/hermes/bin/hermes` sits earliest on PATH and is a
	// privilege-drop shim: invoked as root it re-execs the real binary under
	// this UID via s6-setuidgid, so anything ARIES stages under HERMES_HOME
	// must be owned by it. Staging as root instead leaves the agent unable to
	// read its own configuration, and the failure surfaces far from its cause
	// as a PermissionError inside Hermes's dotenv loader.
	runtimeUID = 10000
	runtimeGID = 10000

	ModeAgent           = "agent"
	ModeVoiceTranscribe = "voice-transcribe"
)

// gatewayCommand replaces the upstream entrypoint so ARIES owns when the agent
// starts. It first aligns the staged runtime with the unprivileged `hermes`
// UID: the archive already carries that ownership, but the Engine's copy API
// resets it to root, and the start command is the one place that runs as root
// after the copy and before readiness.
var (
	gatewayEntrypoint = []string{"/bin/sh"}
	gatewayCommand    = []string{"-c", fmt.Sprintf("chown -R %d:%d %s && exec "+gatewayLauncherPath, runtimeUID, runtimeGID, stagedRoot)}
)

// Options are the host-local inputs to one upstream Hermes container.
type Options struct {
	Runtime         harness.RuntimeOptions
	Common          harness.Options
	MaxTurns        int
	TerminalTimeout int
	VoiceTranscribe VoiceTranscribeOptions
	// Compaction and ExtraBody render optional native config blocks.
	Compaction *CompactionSettings
	ExtraBody  []byte
}

type VoiceTranscribeOptions struct {
	TTS harness.TTSOptions
	STT VoiceSTTOptions
}

type VoiceSTTOptions struct {
	Provider string
	Model    string
	Language string
	Timeout  time.Duration
}

type Manager struct {
	options   Options
	runtime   *harness.Runtime
	newSpeech func(audioinput.SpeechClientOptions) (speechSynthesizer, error)
	newID     func() (string, error)

	mu     sync.Mutex
	active *session
}

type session struct {
	*harness.Occurrence
	gateway *gatewayExecution
}

type speechSynthesizer interface {
	Synthesize(context.Context, audioinput.SpeechRequest) (audioinput.SpeechResult, error)
	Close()
}

var _ runner.AgentHarness = (*Manager)(nil)

// Close releases the deployment transport after lifecycle cleanup.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	return manager.runtime.Close()
}

// New constructs a harness without starting its deployment.
func New(options Options) (*Manager, error) {
	runtime, err := harness.NewRuntime("Hermes", options.Runtime)
	if err != nil {
		return nil, err
	}
	if options.MaxTurns <= 0 {
		options.MaxTurns = defaultMaxTurns
	}
	if options.TerminalTimeout <= 0 {
		options.TerminalTimeout = defaultTerminalTimeout
	}
	if options.Common.Mode == "" {
		options.Common.Mode = ModeAgent
	}
	switch options.Common.Mode {
	case ModeAgent:
		if options.VoiceTranscribe != (VoiceTranscribeOptions{}) {
			return nil, errors.New("Hermes voice options require voice-transcribe mode")
		}
	case ModeVoiceTranscribe:
		if options.VoiceTranscribe.TTS.Provider == "" {
			options.VoiceTranscribe.TTS.Provider = "openai"
		}
		if options.VoiceTranscribe.TTS.Provider != "openai" {
			return nil, errors.New("Hermes voice TTS provider must be openai")
		}
		if options.VoiceTranscribe.TTS.APIKeyEnv == "" {
			options.VoiceTranscribe.TTS.APIKeyEnv = "OPENAI_API_KEY"
		}
		if options.VoiceTranscribe.STT.Provider == "" {
			options.VoiceTranscribe.STT.Provider = "openai"
		}
		if options.VoiceTranscribe.STT.Provider != "openai" && options.VoiceTranscribe.STT.Provider != "local" {
			return nil, errors.New("Hermes voice STT provider must be openai or local")
		}
		if options.VoiceTranscribe.STT.Model == "" {
			if options.VoiceTranscribe.STT.Provider == "local" {
				options.VoiceTranscribe.STT.Model = "base"
			} else {
				options.VoiceTranscribe.STT.Model = "gpt-4o-mini-transcribe"
			}
		}
	default:
		return nil, errors.New("Hermes mode must be agent or voice-transcribe")
	}
	for _, server := range options.Common.MCPServers {
		if err := core.ValidateMCPServer(server); err != nil {
			return nil, fmt.Errorf("Hermes MCP server: %w", err)
		}
	}
	options.Runtime = runtime.Options
	options.ExtraBody = bytes.Clone(options.ExtraBody)
	options.Common.MCPServers = append([]core.MCPServerConfig(nil), options.Common.MCPServers...)
	return &Manager{runtime: runtime, options: options, newSpeech: newSpeechClient, newID: privatefiles.RandomID}, nil
}

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtime.InUse() {
		return errors.New("Hermes harness is already active")
	}
	if err := harness.ValidateRunID("Hermes", request.RunID); err != nil {
		return err
	}
	if err := harness.ValidateTaskID("Hermes", request.TaskID); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("Hermes task timeout must not be negative")
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.runtime.Options.AgentTimeout
	}
	extractEnabled := manager.options.Common.WebSearchEnabled && manager.options.Common.ExtractAPIKeyEnv != ""
	var voiceSTT *VoiceSTTOptions
	if manager.options.Common.Mode == ModeVoiceTranscribe {
		voiceSTT = &manager.options.VoiceTranscribe.STT
	}

	configuration, err := renderConfig(request.Model, renderSettings{
		maxTurns: manager.options.MaxTurns, webSearchEnabled: manager.options.Common.WebSearchEnabled, extractEnabled: extractEnabled,
		subagentsEnabled: manager.options.Common.SubagentsEnabled, maxConcurrentSubagents: manager.options.Common.MaxConcurrentSubagents,
		compaction: manager.options.Compaction, extraBody: manager.options.ExtraBody, mcpServers: manager.options.Common.MCPServers,
	}, voiceSTT)
	if err != nil {
		return err
	}
	terminal, err := renderTerminal(request.Endpoint.Workdir, manager.options.TerminalTimeout)
	if err != nil {
		return err
	}
	configuration = append(configuration, terminal...)
	environment, err := containerEnvironment(request.Endpoint, request.Endpoint.Workdir, manager.options.TerminalTimeout, manager.options.Common.WebSearchEnabled, request.Connectivity.SearchURL, request.RunID, request.TaskID)
	if err != nil {
		return err
	}
	credentials := harness.NewCredentials("Hermes")
	defer func() {
		if credentials != nil {
			credentials.Clear()
		}
	}()
	if ok, err := credentials.Load("model", request.Model.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("Hermes API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	if bytes.Contains(configuration, credentials.Get("model")) {
		return errors.New("rendered Hermes config contains the API-key value")
	}
	if extractEnabled {
		if ok, err := credentials.Load("extract", manager.options.Common.ExtractAPIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
			return err
		} else if !ok {
			return fmt.Errorf("Hermes extract API-key environment %q is not set", manager.options.Common.ExtractAPIKeyEnv)
		}
		if bytes.Contains(configuration, credentials.Get("extract")) {
			return errors.New("rendered Hermes config contains the extract API-key value")
		}
	}
	if manager.options.Common.Mode == ModeVoiceTranscribe {
		if ok, err := credentials.Load("voice", manager.options.VoiceTranscribe.TTS.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
			return fmt.Errorf("Hermes voice API key: %w", err)
		} else if !ok {
			return fmt.Errorf("Hermes voice API-key environment %q is not set", manager.options.VoiceTranscribe.TTS.APIKeyEnv)
		}
		if bytes.Contains(configuration, credentials.Get("voice")) {
			return errors.New("rendered Hermes config contains the voice API-key value")
		}
	}
	if err := credentials.LoadMCP(manager.options.Common.MCPServers, configuration, manager.runtime.Options.APIKeyLookup); err != nil {
		return err
	}
	id, err := manager.newID()
	if err != nil {
		return fmt.Errorf("generate Hermes harness ID: %w", err)
	}
	deploymentRequest := deployment.Request{
		Name: "aries-hermes-" + id, Placement: request.Connectivity.Placement, CPU: request.CPU, MemoryMB: request.MemoryMB, ServicePort: gatewayPort, ImageVolumes: []string{imageDeclaredVolume},
		Image:      manager.runtime.Options.Image,
		Workdir:    workspaceRoot,
		Env:        environment,
		Entrypoint: append([]string(nil), gatewayEntrypoint...),
		Args:       append([]string(nil), gatewayCommand...),
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "hermes-harness",
			"aries.component": "harness",
			"aries.run":       request.RunID, "aries.task": request.TaskID,
			"aries.attempt": id,
		},
	}
	active := &session{
		Occurrence: &harness.Occurrence{RunID: request.RunID, TaskID: request.TaskID, AttemptID: id, Name: "aries-hermes-" + id, DeploymentRequest: deploymentRequest, ArtifactDir: filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness"), Endpoint: request.Endpoint, Model: request.Model, AgentTimeout: agentTimeout},
	}
	active.Credentials = credentials
	active.Artifacts = &harness.Artifacts{Directory: active.ArtifactDir}
	credentials = nil

	if err := manager.runtime.Own(active.Occurrence); err != nil {
		active.Credentials.Clear()
		return err
	}
	fail := func(primary error) error {
		if active.gateway != nil {
			active.gateway.client.Close()
		}
		err := manager.runtime.Rollback(ctx, active.Occurrence, primary)
		if manager.runtime.InUse() {
			manager.active = active
		}
		return err
	}
	encodedToken, err := privatefiles.RandomSecret(32)
	if err != nil {
		return fail(fmt.Errorf("generate Hermes Gateway credential: %w", err))
	}
	active.Credentials.Set("gateway", encodedToken)
	clear(encodedToken)
	if err := privatefiles.EnsureDirectory(active.ArtifactDir); err != nil {
		return fail(fmt.Errorf("create Hermes artifact directory: %w", err))
	}
	if err := active.Artifacts.Write("config.yaml", active.Credentials.Redact(configuration)); err != nil {
		return fail(fmt.Errorf("retain rendered Hermes config: %w", err))
	}
	archive, err := manager.runtimeArchive(active, configuration)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)
	secrets := active.Credentials.Secrets()
	if err := manager.runtime.Start(ctx, active.Occurrence, deploymentRequest, archive, secrets); err != nil {
		return fail(err)
	}
	address, err := manager.runtime.Options.Deployment.Address(ctx, active.ID, gatewayPort)
	if err != nil {
		return fail(fmt.Errorf("resolve Hermes Gateway: %w", err))
	}
	active.gateway, err = newGatewayExecution("http://"+address, active.Credentials.Get("gateway"))
	if err != nil {
		return fail(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.runtime.Options.StartTimeout)
	err = manager.waitReady(readyCtx, active)
	cancel()
	if err != nil {
		return fail(err)
	}
	manager.active = active
	manager.runtime.Ready()
	manager.runtime.Options.Logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.TaskID, "container": active.Name}).Info("Hermes harness started")
	return nil
}

func (manager *Manager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	if err := manager.runtime.AdmitRun(instruction); err != nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, err
	}
	active := manager.active
	active = snapshotSession(active)
	manager.mu.Unlock()
	defer active.Credentials.Clear()

	if manager.options.Common.Mode == ModeVoiceTranscribe {
		return manager.runVoiceTranscribe(ctx, active, instruction, started)
	}
	output, outcome, err := manager.runGateway(ctx, active, instruction)
	stdout := active.Credentials.Redact([]byte(output))
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active, stdout, outcome)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: strings.TrimRight(string(stdout), "\n"),
		Duration: time.Since(started), LogPaths: active.Artifacts.Paths(),
	}, nil
}

func (manager *Manager) runVoiceTranscribe(ctx context.Context, active *session, instruction string, started time.Time) (core.HarnessResult, error) {
	audioPath, voicePaths, err := manager.synthesizeVoiceInstruction(ctx, active, instruction)
	if len(voicePaths) != 0 {
		active.Artifacts.Add(voicePaths...)
	}
	if err != nil {
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	if err := manager.stageVoiceWAV(ctx, active, audioPath); err != nil {
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	sttResult := manager.transcribeVoice(ctx, active)
	if !sttResult.OK {
		if err := manager.writeVoiceResult(active, instruction, sttResult, ""); err != nil {
			err = active.Credentials.RedactErr(err)
			return active.FailedResult(started, err), err
		}
		err := errors.New(cmp.Or(sttResult.Error, "Hermes voice STT failed"))
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	if strings.TrimSpace(sttResult.Transcript) == "" || strings.ContainsRune(sttResult.Transcript, 0) {
		err := active.Credentials.RedactErr(errors.New("Hermes voice transcript is invalid"))
		return active.FailedResult(started, err), err
	}
	output, outcome, runErr := manager.runGateway(ctx, active, sttResult.Transcript)
	stdout := active.Credentials.Redact([]byte(output))
	err = runErr
	agentOutput := strings.TrimRight(string(stdout), "\n")
	if writeErr := manager.writeVoiceResult(active, instruction, sttResult, agentOutput); writeErr != nil {
		err = errors.Join(err, active.Credentials.RedactErr(writeErr))
		return active.FailedResult(started, err), err
	}
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active, stdout, outcome)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: agentOutput,
		Duration: time.Since(started), LogPaths: active.Artifacts.Paths(),
	}, nil
}

type voiceSTTResult struct {
	OK            bool           `json:"ok"`
	Transcript    string         `json:"transcript"`
	RawTranscript string         `json:"raw_transcript"`
	ReturnCode    int            `json:"returncode"`
	RawResult     map[string]any `json:"raw_result,omitempty"`
	Stdout        string         `json:"stdout"`
	Stderr        string         `json:"stderr"`
	Signature     string         `json:"signature,omitempty"`
	Error         string         `json:"error,omitempty"`
}

func (manager *Manager) synthesizeVoiceInstruction(ctx context.Context, active *session, instruction string) (string, []string, error) {
	return audioinput.SynthesizeVoiceInstruction(ctx, instruction, audioinput.VoiceInstructionOptions{
		ArtifactDir:  active.ArtifactDir,
		ErrorLabel:   "Hermes",
		Provider:     manager.options.VoiceTranscribe.TTS.Provider,
		BaseURL:      manager.options.VoiceTranscribe.TTS.BaseURL,
		APIKey:       active.Credentials.Get("voice"),
		Model:        manager.options.VoiceTranscribe.TTS.Model,
		Voice:        manager.options.VoiceTranscribe.TTS.Voice,
		Instructions: manager.options.VoiceTranscribe.TTS.Instructions,
		Speed:        manager.options.VoiceTranscribe.TTS.Speed,
		Timeout:      manager.options.VoiceTranscribe.TTS.Timeout,
		NewSpeech: func(options audioinput.SpeechClientOptions) (audioinput.SpeechSynthesizer, error) {
			return manager.newSpeech(options)
		},
		WriteArtifact: privatefiles.WriteArtifact,
	})
}

func (manager *Manager) stageVoiceWAV(ctx context.Context, active *session, audioPath string) error {
	audio, err := os.ReadFile(audioPath)
	if err != nil {
		return fmt.Errorf("read Hermes voice audio artifact: %w", err)
	}
	defer clear(audio)
	archive, err := harness.StageArchive(nil, map[string]harness.ArchiveFile{strings.TrimPrefix(voiceWAVPath, "/"): {Content: audio, Mode: 0o600, UID: runtimeUID, GID: runtimeGID}})
	if err != nil {
		return fmt.Errorf("stage Hermes voice audio archive: %w", err)
	}
	defer clear(archive)
	if err := manager.runtime.Options.Deployment.UploadArchive(ctx, active.ID, "/", bytes.NewReader(archive)); err != nil {
		return fmt.Errorf("copy Hermes voice audio: %w", err)
	}
	return nil
}

func (manager *Manager) transcribeVoice(ctx context.Context, active *session) voiceSTTResult {
	timeout := manager.options.VoiceTranscribe.STT.Timeout
	if timeout <= 0 {
		timeout = defaultVoiceSTTTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := hermesSTTCommand(manager.options.VoiceTranscribe.STT)
	result, err := manager.runtime.Options.Deployment.Exec(runCtx, active.ID, core.Command{Path: command[0], Args: command[1:], Dir: workspaceRoot, OutputLimitBytes: maxDockerOutput})
	stt := voiceSTTResult{
		ReturnCode: result.ExitCode,
		Stdout:     tailString(active.Credentials.Redact([]byte(result.Stdout)), 20000),
		Stderr:     tailString(active.Credentials.Redact([]byte(result.Stderr)), 20000),
	}
	if err != nil {
		stt.Error = err.Error()
		return stt
	}
	if result.ExitCode != 0 {
		stt.Error = cmp.Or(strings.TrimSpace(stt.Stderr), strings.TrimSpace(stt.Stdout), fmt.Sprintf("stt_exit_%d", result.ExitCode))
		return stt
	}
	payload, ok := lastJSONObject(stt.Stdout)
	if !ok {
		stt.Error = "stt_no_json_result"
		return stt
	}
	stt.RawResult = payload
	if success, ok := payload["success"].(bool); ok && !success {
		stt.Error = cmp.Or(normalizeTranscript(payload["error"]), "stt_failed")
		return stt
	}
	stt.Transcript = strings.TrimSpace(normalizeTranscript(payload["transcript"]))
	stt.RawTranscript = stt.Transcript
	stt.Signature, _ = payload["signature"].(string)
	if stt.Transcript == "" {
		stt.Error = "stt_empty_transcript"
		return stt
	}
	stt.OK = true
	return stt
}

func hermesSTTCommand(options VoiceSTTOptions) []string {
	provider := options.Provider
	if provider == "" {
		provider = "openai"
	}
	model := options.Model
	if model == "" {
		if provider == "local" {
			model = "base"
		} else {
			model = "gpt-4o-mini-transcribe"
		}
	}
	script := `set -eu
if [ -f ` + voiceKeyPath + ` ]; then
  OPENAI_API_KEY="$(cat ` + voiceKeyPath + `)"
  export OPENAI_API_KEY
fi
exec python3 -c "$1" "$2" "$3" "$4" "$5"
`
	return []string{"/bin/sh", "-c", script, "aries-hermes-stt", hermesSTTScript, voiceWAVPath, model, provider, options.Language}
}

const hermesSTTScript = `
import inspect
import json
import os
import sys
from tools.voice_mode import transcribe_recording
wav_path = sys.argv[1]
model = sys.argv[2]
provider = sys.argv[3] if len(sys.argv) > 3 else os.environ.get("HERMES_STT_PROVIDER", "openai")
language = sys.argv[4] if len(sys.argv) > 4 else os.environ.get("HERMES_STT_LANGUAGE", "")
os.environ["HERMES_STT_PROVIDER"] = provider
os.environ["HERMES_STT_MODEL"] = model
os.environ["OPENAI_STT_MODEL"] = model
os.environ["OPENAI_TRANSCRIBE_MODEL"] = model
if language:
    os.environ["HERMES_STT_LANGUAGE"] = language
sig = inspect.signature(transcribe_recording)
kwargs = {}
params = list(sig.parameters.values())
if not params:
    result = transcribe_recording()
else:
    first = params[0].name
    kwargs[first] = wav_path
    if "provider" in sig.parameters:
        kwargs["provider"] = provider
    if "model" in sig.parameters:
        kwargs["model"] = model
    if "language" in sig.parameters and language:
        kwargs["language"] = language
    result = transcribe_recording(**kwargs)
if isinstance(result, str):
    payload = {"transcript": result}
elif isinstance(result, dict):
    payload = result
else:
    payload = {"transcript": getattr(result, "text", None) or getattr(result, "transcript", None) or str(result)}
payload.setdefault("signature", str(sig))
print(json.dumps(payload, ensure_ascii=False))
`

func (manager *Manager) writeVoiceResult(active *session, originalPrompt string, stt voiceSTTResult, agentOutput string) error {
	payload := map[string]any{
		"ok":                  stt.OK,
		"original_prompt":     originalPrompt,
		"transcript":          stt.Transcript,
		"agent_question_used": stt.Transcript,
		"agent_output_text":   agentOutput,
		"stt":                 stt,
		"container_wav_path":  voiceWAVPath,
	}
	content, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Hermes voice result: %w", err)
	}
	content = append(content, '\n')
	content = active.Credentials.Redact(content)
	path := filepath.Join(active.ArtifactDir, "voice-result.json")
	if err := privatefiles.WriteArtifact(path, content); err != nil {
		return fmt.Errorf("write Hermes voice result: %w", err)
	}
	active.Artifacts.Add(path)
	transcriptPath := filepath.Join(active.ArtifactDir, "voice-transcript.txt")
	if err := privatefiles.WriteArtifact(transcriptPath, []byte(stt.Transcript)); err != nil {
		return fmt.Errorf("write Hermes voice transcript: %w", err)
	}
	active.Artifacts.Add(transcriptPath)
	return nil
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
	attempt, ownsCleanup := manager.runtime.BeginStop()
	if !ownsCleanup {
		manager.mu.Unlock()
		return attempt.Wait(ctx)
	}
	active := manager.active
	manager.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	err := manager.stopSession(cleanupCtx, active)
	cancel()

	manager.mu.Lock()
	if active == nil || active.ID == "" {
		manager.active = nil
	}
	manager.runtime.FinishStop(err)
	manager.mu.Unlock()
	return err
}

func newSpeechClient(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
	return audioinput.NewSpeechClient(options)
}

func (manager *Manager) waitReady(ctx context.Context, active *session) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := active.gateway.client.Ready(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		running, inspectErr := manager.runtime.Options.Deployment.Running(ctx, active.ID)
		if inspectErr != nil {
			return fmt.Errorf("inspect Hermes readiness: %w", inspectErr)
		}
		if !running {
			return errors.New("Hermes container exited before readiness")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await Hermes readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) runtimeArchive(active *session, configuration []byte) ([]byte, error) {
	identity, err := privatefiles.Read(active.Endpoint.IdentitySourceFile, 0o600)
	if err != nil {
		return nil, fmt.Errorf("read Hermes SSH identity: %w", err)
	}
	defer clear(identity)
	extractEnabled := len(active.Credentials.Get("extract")) != 0
	mcpHostVars := make([]string, 0, len(active.Credentials.MCPFiles()))
	for hostVar := range active.Credentials.MCPFiles() {
		mcpHostVars = append(mcpHostVars, hostVar)
	}
	slices.Sort(mcpHostVars)
	files := map[string]harness.ArchiveFile{
		strings.TrimPrefix(configContainerPath, "/"): {Content: configuration, Mode: 0o600},
		strings.TrimPrefix(modelKeyPath, "/"):        {Content: active.Credentials.Get("model"), Mode: 0o600},
		strings.TrimPrefix(identityContainerFS, "/"): {Content: identity, Mode: 0o600},
		strings.TrimPrefix(gatewayKeyPath, "/"):      {Content: active.Credentials.Get("gateway"), Mode: 0o600},
		strings.TrimPrefix(gatewayLauncherPath, "/"): {Content: gatewayLauncherScript(active.Model.APIKeyEnv, extractEnabled, mcpHostVars...), Mode: 0o555},
	}
	if extractEnabled {
		files[strings.TrimPrefix(extractKeyPath, "/")] = harness.ArchiveFile{Content: active.Credentials.Get("extract"), Mode: 0o600}
	}
	if len(active.Credentials.Get("voice")) != 0 {
		files[strings.TrimPrefix(voiceKeyPath, "/")] = harness.ArchiveFile{Content: active.Credentials.Get("voice"), Mode: 0o600}
	}
	for _, hostVar := range mcpHostVars {
		files[strings.TrimPrefix(stateContainerPath+"/mcp_"+hostVar+".key", "/")] = harness.ArchiveFile{
			Content: active.Credentials.MCPFiles()[hostVar],
			Mode:    0o600,
		}
	}
	for name, file := range files {
		file.UID, file.GID = runtimeUID, runtimeGID
		files[name] = file
	}
	directories := []harness.ArchiveDirectory{
		{Name: "run/aries", Mode: 0o700, UID: runtimeUID, GID: runtimeGID},
		{Name: "run/aries/hermes", Mode: 0o700, UID: runtimeUID, GID: runtimeGID},
		{Name: "run/aries/ssh", Mode: 0o700, UID: runtimeUID, GID: runtimeGID},
		{Name: "run/aries/workspace", Mode: 0o755, UID: runtimeUID, GID: runtimeGID},
	}
	return harness.StageArchive(directories, files)
}

func (manager *Manager) stopSession(ctx context.Context, active *session) error {
	if active == nil {
		return nil
	}
	if active.gateway != nil {
		active.gateway.cancelRun(ctx)
	}
	if err := manager.runtime.Remove(ctx, active.Occurrence); err != nil {
		return err
	}
	if active.gateway != nil {
		active.gateway.client.Close()
	}
	return nil
}

func (manager *Manager) collectArtifacts(ctx context.Context, active *session, output []byte, outcome runOutcome) error {
	var errs []error
	if encoded, err := json.MarshalIndent(outcome, "", "  "); err != nil {
		errs = append(errs, fmt.Errorf("encode Hermes session outcome: %w", err))
	} else {
		path := filepath.Join(active.ArtifactDir, "session-outcome.json")
		if err := privatefiles.WriteArtifact(path, append(encoded, '\n')); err != nil {
			errs = append(errs, fmt.Errorf("retain Hermes session outcome: %w", err))
		} else {
			active.Artifacts.Add(path)
		}
	}
	for _, item := range []struct {
		name    string
		content []byte
	}{{"gateway-output.txt", output}} {
		path := filepath.Join(active.ArtifactDir, item.name)
		if err := privatefiles.WriteArtifact(path, item.content); err != nil {
			errs = append(errs, fmt.Errorf("retain %s: %w", item.name, err))
			continue
		}
		active.Artifacts.Add(path)
	}
	if logs, err := manager.runtime.Options.Deployment.Logs(ctx, active.ID, maxDockerOutput); err != nil {
		errs = append(errs, fmt.Errorf("collect Hermes container logs: %w", err))
	} else {
		content := privatefiles.FilterLogs(logs, maxDockerOutput, active.Credentials.Secrets()...)
		path := filepath.Join(active.ArtifactDir, "container.log")
		if err := privatefiles.WriteArtifact(path, content); err != nil {
			errs = append(errs, err)
		} else {
			active.Artifacts.Add(path)
		}
	}
	for _, collect := range []func(context.Context, *session) ([]string, error){manager.collectSessions, manager.collectSpans} {
		paths, err := collect(ctx, active)
		if err != nil {
			errs = append(errs, err)
		} else {
			active.Artifacts.Add(paths...)
		}
	}
	index, err := json.MarshalIndent(struct {
		Paths []string `json:"paths"`
	}{Paths: active.Artifacts.TelemetryPaths()}, "", "  ")
	if err == nil {
		index = append(index, '\n')
		path := filepath.Join(active.ArtifactDir, "telemetry.index.json")
		err = privatefiles.WriteArtifact(path, index)
		if err == nil {
			active.Artifacts.Add(path)
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("write Hermes telemetry index: %w", err))
	}
	return errors.Join(errs...)
}

// collectSessions exports Hermes's own SQLite session store as the
// message-level trajectory. Writing to stdout avoids depending on a path
// inside the container that the export may or may not have produced.
func (manager *Manager) collectSessions(ctx context.Context, active *session) ([]string, error) {
	result, err := manager.runtime.Options.Deployment.Exec(ctx, active.ID, core.Command{Path: "hermes", Args: []string{"sessions", "export", "-"}, Dir: workspaceRoot, OutputLimitBytes: maxDockerOutput})
	if err != nil {
		return nil, fmt.Errorf("export Hermes sessions: %w", err)
	}
	if result.ExitCode != 0 || len(bytes.TrimSpace([]byte(result.Stdout))) == 0 {
		// A run that never reached the model leaves no session; that is not a
		// harness failure and must not mask the real result.
		manager.runtime.Options.Logger.WithContext(ctx).WithField("task_id", active.TaskID).Debug("Hermes produced no session export")
		return nil, nil
	}
	path := filepath.Join(active.ArtifactDir, "telemetry", "sessions.jsonl")
	if err := privatefiles.WriteArtifact(path, active.Credentials.Redact([]byte(result.Stdout))); err != nil {
		return nil, fmt.Errorf("retain Hermes sessions: %w", err)
	}
	return []string{path}, nil
}

const (
	spanStoreMissing = 3
	spansTruncated   = 4
	spanDumpLimit    = maxDockerOutput - 1<<20
)

// spanDumpScript prints each span the hermes_otel plugin stored, one JSON
// object per line in the order they ended. The store is opened read-only so
// the dump never creates it. Output stops before the byte limit in argv[2]
// rather than cut a line; the dump then reports how many spans it kept and
// exits spansTruncated.
const spanDumpScript = `import os, sqlite3, sys
path, limit = sys.argv[1], int(sys.argv[2])
if not os.path.isfile(path):
    sys.exit(3)
db = sqlite3.connect("file:" + path + "?mode=ro", uri=True)
written = kept = 0
rows = db.execute("SELECT data FROM events WHERE kind = 'span' ORDER BY seq").fetchall()
for (data,) in rows:
    line = (data + "\n").encode()
    if written + len(line) > limit:
        sys.stderr.write("kept %d of %d spans\n" % (kept, len(rows)))
        sys.exit(4)
    sys.stdout.buffer.write(line)
    written += len(line)
    kept += 1
`

// collectSpans retains the hermes_otel spans as telemetry/otel-spans.jsonl.
// Each tool, model, and API span carries the wall-clock time its call started
// and ended, which Hermes's own session export does not record. A missing
// store or an empty one is not a failure; a dump over spanDumpLimit keeps the
// spans that fit and logs a warning; any other dump failure is an error.
func (manager *Manager) collectSpans(ctx context.Context, active *session) ([]string, error) {
	result, err := manager.runtime.Options.Deployment.Exec(ctx, active.ID, core.Command{
		Path: "python3", Args: []string{"-c", spanDumpScript, otelStorePath, strconv.Itoa(spanDumpLimit)},
		Dir: workspaceRoot, OutputLimitBytes: maxDockerOutput,
	})
	if err != nil {
		return nil, fmt.Errorf("dump Hermes spans: %w", err)
	}
	logger := manager.runtime.Options.Logger.WithContext(ctx).WithField("task_id", active.TaskID)
	switch result.ExitCode {
	case 0:
	case spanStoreMissing:
		logger.Debug("Hermes has no span store")
		return nil, nil
	case spansTruncated:
		logger.WithField("detail", strings.TrimSpace(string(active.Credentials.Redact([]byte(result.Stderr))))).Warn("Hermes spans exceeded the dump limit; later spans were dropped")
	default:
		return nil, fmt.Errorf("dump Hermes spans exited with code %d: %s", result.ExitCode, bytes.TrimSpace(active.Credentials.Redact([]byte(result.Stderr))))
	}
	if len(bytes.TrimSpace([]byte(result.Stdout))) == 0 {
		logger.Debug("Hermes produced no spans")
		return nil, nil
	}
	path := filepath.Join(active.ArtifactDir, "telemetry", "otel-spans.jsonl")
	if err := privatefiles.WriteArtifact(path, active.Credentials.Redact([]byte(result.Stdout))); err != nil {
		return nil, fmt.Errorf("retain Hermes spans: %w", err)
	}
	return []string{path}, nil
}

func lastJSONObject(text string) (map[string]any, bool) {
	lines := strings.Split(text, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err == nil {
			return payload, true
		}
	}
	return nil, false
}

func normalizeTranscript(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case map[string]any:
		for _, key := range []string{"text", "transcript", "output_text"} {
			if text, ok := typed[key].(string); ok {
				return text
			}
		}
	}
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func tailString(content []byte, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(content) > limit {
		content = content[len(content)-limit:]
	}
	return string(content)
}
