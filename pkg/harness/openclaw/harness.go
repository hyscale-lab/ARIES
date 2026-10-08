package openclaw

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	audioinput "github.com/hyscale-lab/aries/pkg/audio"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/harness"
	"github.com/hyscale-lab/aries/pkg/harness/internal/privatefiles"
	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
	realtimeclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/realtime"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultVoiceSTTTimeout = 5 * time.Minute
	maxDockerOutput        = 16 << 20
	gatewayListenPort      = "18789"
	gatewayLauncherPath    = "/run/aries/gateway-launcher"
	upstreamGatewayPort    = "18790"
)

const (
	ModeAgent           = "agent"
	ModeRealtime        = "realtime"
	ModeVoiceTranscribe = "voice-transcribe"
)

func isRealtimeMode(mode string) bool {
	return mode == ModeRealtime || mode == ModeVoiceTranscribe
}

// Options are the host-local inputs to one upstream OpenClaw container.
type Options struct {
	Runtime  harness.RuntimeOptions
	Common   harness.Options
	Realtime RealtimeOptions
}

type RealtimeOptions struct {
	AgentQuestionTemplate string
	TTS                   harness.TTSOptions
	ChunkDuration         time.Duration
	ListenDuration        time.Duration
	QuietDuration         time.Duration
	AgentWaitDuration     time.Duration
	ToolCallTimeout       time.Duration
	TrailingSilenceMillis int
	Provider              string
	Model                 string
	Voice                 string
	ReasoningEffort       string
	IncludeEvents         bool
}

type Manager struct {
	options         Options
	runtime         *harness.Runtime
	newID           func() (string, error)
	newGateway      func(string, []byte) (gatewayConnection, error)
	newAgentGateway func(string, []byte) (gatewayConnection, error)
	newRealtime     func(realtimeclient.Gateway, realtimeclient.Options) (realtimeRunner, error)
	newSpeech       func(audioinput.SpeechClientOptions) (speechSynthesizer, error)

	mu     sync.Mutex
	active *session
}

type realtimeRunner interface {
	Run(context.Context) (realtimeclient.Result, error)
}

type gatewayConnection interface {
	realtimeclient.Gateway
	Agent(context.Context, gatewayclient.AgentRequest) (gatewayclient.AgentResult, error)
}

type speechSynthesizer interface {
	Synthesize(context.Context, audioinput.SpeechRequest) (audioinput.SpeechResult, error)
	Close()
}

// Close releases deployment transport after lifecycle cleanup.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	return manager.runtime.Close()
}

type session struct {
	*harness.Occurrence
	safeTaskID       string
	gatewayURL       string
	agentIdempotency string
}

var _ runner.AgentHarness = (*Manager)(nil)

// New constructs a harness using the supplied deployment capability.
func New(options Options) (*Manager, error) {
	runtime, err := harness.NewRuntime("OpenClaw", options.Runtime)
	if err != nil {
		return nil, err
	}
	if options.Common.Mode == "" {
		options.Common.Mode = ModeAgent
	}
	switch options.Common.Mode {
	case ModeAgent:
		if options.Realtime != (RealtimeOptions{}) {
			return nil, errors.New("OpenClaw realtime options require realtime mode")
		}
	case ModeRealtime, ModeVoiceTranscribe:
		if options.Realtime.TrailingSilenceMillis < 0 {
			return nil, errors.New("OpenClaw realtime options are invalid")
		}
		if options.Realtime.TTS.Provider == "" {
			options.Realtime.TTS.Provider = "openai"
		}
		if options.Realtime.TTS.Provider != "openai" {
			return nil, errors.New("OpenClaw realtime TTS provider must be openai")
		}
		if options.Realtime.TTS.APIKeyEnv == "" {
			options.Realtime.TTS.APIKeyEnv = "OPENAI_API_KEY"
		}
	default:
		return nil, errors.New("OpenClaw mode must be agent, realtime, or voice-transcribe")
	}
	for _, server := range options.Common.MCPServers {
		if err := core.ValidateMCPServer(server); err != nil {
			return nil, err
		}
	}
	options.Runtime = runtime.Options
	return &Manager{runtime: runtime, options: options, newID: privatefiles.RandomID,
		newGateway: func(rawURL string, token []byte) (gatewayConnection, error) {
			return newGatewayClientWithDisposition(rawURL, token, gatewayScopes(options.Common.Mode), gatewayEventDisposition(options.Common.Mode))
		},
		newAgentGateway: func(rawURL string, token []byte) (gatewayConnection, error) {
			return newGatewayClientWithDisposition(rawURL, token, gatewayScopes(ModeAgent), gatewayclient.EventDispositionResponseOnly)
		},
		newRealtime: newRealtimeRunner, newSpeech: newSpeechClient,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.runtime.InUse() {
		return errors.New("OpenClaw harness is already active")
	}
	if err := harness.ValidateRunID("OpenClaw", request.RunID); err != nil {
		return err
	}
	if err := harness.ValidateTaskID("OpenClaw", request.TaskID); err != nil {
		return err
	}
	if err := harness.ValidateSearch(request.Connectivity.SearchURL, manager.options.Common.WebSearchEnabled); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("OpenClaw task timeout must not be negative")
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.runtime.Options.AgentTimeout
	}
	extractRequested := manager.options.Common.WebSearchEnabled && manager.options.Common.ExtractAPIKeyEnv != ""
	credentials := harness.NewCredentials("OpenClaw")
	defer func() {
		if credentials != nil {
			credentials.Clear()
		}
	}()
	extractEnabled := false
	if extractRequested {
		found, err := credentials.Load("extract", manager.options.Common.ExtractAPIKeyEnv, manager.runtime.Options.APIKeyLookup)
		if err != nil {
			return fmt.Errorf("OpenClaw extract API key: %w", err)
		}
		extractEnabled = found
		if !found {
			manager.runtime.Options.Logger.WithContext(ctx).WithFields(logrus.Fields{
				"task_id": request.TaskID, "extract_api_key_env": manager.options.Common.ExtractAPIKeyEnv,
			}).Warn("OpenClaw extract API-key environment is not set; falling back to native web_fetch without Tavily extraction")
		}
	}
	configuration, err := renderConfig(request.Model, request.Endpoint, manager.options.Common.Mode, request.Connectivity.SearchURL, manager.options.Common.WebSearchEnabled, extractEnabled, manager.options.Common.SubagentsEnabled, manager.options.Common.MaxConcurrentSubagents, MCPOptions{
		Servers: manager.options.Common.MCPServers,
	})
	if err != nil {
		return err
	}
	if ok, err := credentials.Load("model", request.Model.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("OpenClaw API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	if isRealtimeMode(manager.options.Common.Mode) {
		if ok, err := credentials.Load("voice", manager.options.Realtime.TTS.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
			return fmt.Errorf("OpenClaw realtime API key: %w", err)
		} else if !ok {
			return fmt.Errorf("OpenClaw realtime API-key environment %q is not set", manager.options.Realtime.TTS.APIKeyEnv)
		}
	}
	if value := credentials.Get("model"); len(value) != 0 && bytes.Contains(configuration, value) {
		return errors.New("rendered OpenClaw config contains the API-key value")
	}
	if value := credentials.Get("voice"); len(value) != 0 && bytes.Contains(configuration, value) {
		return errors.New("rendered OpenClaw config contains the realtime API-key value")
	}
	if value := credentials.Get("extract"); len(value) != 0 && bytes.Contains(configuration, value) {
		return errors.New("rendered OpenClaw config contains the extract API-key value")
	}
	if err := credentials.LoadMCP(manager.options.Common.MCPServers, configuration, manager.runtime.Options.APIKeyLookup); err != nil {
		return err
	}
	id, err := manager.newID()
	if err != nil {
		return fmt.Errorf("generate OpenClaw harness ID: %w", err)
	}
	gatewayToken, err := privatefiles.RandomSecret(32)
	if err != nil {
		return fmt.Errorf("generate OpenClaw gateway token: %w", err)
	}
	credentials.Set("gateway", gatewayToken)
	clear(gatewayToken)
	agentIdempotency, err := privatefiles.RandomID()
	if err != nil {
		return fmt.Errorf("generate OpenClaw agent idempotency key: %w", err)
	}
	active := &session{
		Occurrence: &harness.Occurrence{RunID: request.RunID, TaskID: request.TaskID, AttemptID: id, Name: "aries-openclaw-" + id, ArtifactDir: filepath.Join(manager.runtime.Options.OutputDir, request.TaskID, "harness"), Endpoint: request.Endpoint, Model: request.Model, AgentTimeout: agentTimeout},
		safeTaskID: safeTaskID(request.TaskID),

		agentIdempotency: agentIdempotency,
	}
	active.Credentials = credentials
	active.Artifacts = &harness.Artifacts{Directory: active.ArtifactDir}
	credentials = nil

	if err := manager.runtime.Own(active.Occurrence); err != nil {
		active.Credentials.Clear()
		return err
	}
	active.DeploymentRequest = deployment.Request{
		Name: active.Name, Image: manager.runtime.Options.Image,
		Env: []string{"OPENCLAW_CONFIG_PATH=" + configContainerPath}, Args: []string{launcherPath, gatewayLauncherPath},
		Labels:    map[string]string{"aries.managed": "true", "aries.kind": "openclaw-harness", "aries.component": "harness", "aries.run": request.RunID, "aries.task": request.TaskID, "aries.attempt": active.AttemptID},
		Placement: request.Connectivity.Placement, CPU: request.CPU, MemoryMB: request.MemoryMB, ServicePort: 18789,
	}
	fail := func(primary error) error {
		err := manager.runtime.Rollback(ctx, active.Occurrence, primary)
		if manager.runtime.InUse() {
			manager.active = active
		}
		return err
	}
	if err := privatefiles.EnsureDirectory(active.ArtifactDir); err != nil {
		return fail(fmt.Errorf("create OpenClaw artifact directory: %w", err))
	}
	if err := active.Artifacts.Write("openclaw.json", configuration); err != nil {
		return fail(fmt.Errorf("retain rendered OpenClaw config: %w", err))
	}
	archive, err := manager.runtimeArchive(active, configuration)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)
	secrets := active.Credentials.Secrets()
	if err := manager.runtime.Start(ctx, active.Occurrence, active.DeploymentRequest, archive, secrets); err != nil {
		return fail(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.runtime.Options.StartTimeout)
	err = manager.waitReady(readyCtx, active)
	cancel()
	if err != nil {
		return fail(err)
	}
	gatewayURL, err := manager.gatewayURL(ctx, active)
	if err != nil {
		return fail(err)
	}
	active.gatewayURL = gatewayURL
	manager.active = active
	manager.runtime.Ready()
	manager.runtime.Options.Logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.TaskID, "container": active.Name}).Info("OpenClaw harness started")
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

	if isRealtimeMode(manager.options.Common.Mode) {
		return manager.runRealtime(ctx, active, instruction, started)
	}
	runCtx, cancel := context.WithTimeout(ctx, active.AgentTimeout)
	client, err := manager.newGateway(active.gatewayURL, active.Credentials.Get("gateway"))
	if err != nil {
		cancel()
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	defer client.Close()
	connectSummary, err := client.Connect(runCtx, gatewayclient.ConnectOptions{})
	var agentResult gatewayclient.AgentResult
	if err == nil && !connectSummary.HasScope("operator.write") {
		err = errors.New("OpenClaw agent gateway requires operator.write scope")
	}
	if err == nil {
		thinking := requestedThinking(active.Model)
		agentResult, err = client.Agent(runCtx, gatewayclient.AgentRequest{
			Message: instruction, SessionKey: "agent:main:aries-" + active.safeTaskID,
			IdempotencyKey: active.agentIdempotency, Thinking: thinking,
		})
	}
	cancel()
	connectSummary = redactConnectSummary(connectSummary, active)
	agentResult = redactAgentResult(agentResult, active)
	err = active.Credentials.RedactErr(err)
	resultPath, writeErr := manager.writeAgentResult(active, connectSummary, agentResult, err)
	if writeErr == nil {
		active.Artifacts.Add(resultPath)
	}
	err = errors.Join(err, writeErr)
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		return active.FailedResult(started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: agentResult.Text, Duration: time.Since(started),
		LogPaths: active.Artifacts.Paths(),
	}, nil
}

func (manager *Manager) runRealtime(ctx context.Context, active *session, instruction string, started time.Time) (core.HarnessResult, error) {
	audioPath, speechPaths, synthErr := manager.synthesizeVoiceInstruction(ctx, active, instruction)
	if len(speechPaths) != 0 {
		active.Artifacts.Add(speechPaths...)
	}
	if synthErr != nil {
		synthErr = active.Credentials.RedactErr(synthErr)
		return active.FailedResult(started, synthErr), synthErr
	}
	client, err := manager.newGateway(active.gatewayURL, active.Credentials.Get("gateway"))
	if err != nil {
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	closeGatewayInRunner := manager.options.Common.Mode == ModeRealtime
	runner, err := manager.newRealtime(client, realtimeclient.Options{
		OriginalPrompt:        instruction,
		SessionMode:           manager.options.Common.Mode,
		SessionKey:            "agent:main:aries-" + active.safeTaskID,
		Provider:              manager.options.Realtime.Provider,
		Model:                 manager.options.Realtime.Model,
		Voice:                 manager.options.Realtime.Voice,
		ReasoningEffort:       manager.options.Realtime.ReasoningEffort,
		AudioProvider:         manager.realtimeAudioProvider(audioPath),
		ChunkDuration:         manager.options.Realtime.ChunkDuration,
		ListenDuration:        durationOrDefault(manager.options.Realtime.ListenDuration, active.AgentTimeout),
		QuietDuration:         manager.options.Realtime.QuietDuration,
		AgentWaitDuration:     durationOrDefault(manager.options.Realtime.AgentWaitDuration, active.AgentTimeout),
		ToolCallTimeout:       manager.options.Realtime.ToolCallTimeout,
		AgentQuestionTemplate: manager.options.Realtime.AgentQuestionTemplate,
		IncludeEvents:         manager.options.Realtime.IncludeEvents,
		CloseGateway:          closeGatewayInRunner,
	})
	if err != nil {
		_ = client.Close()
		err = active.Credentials.RedactErr(err)
		return active.FailedResult(started, err), err
	}
	var realtimeResult realtimeclient.Result
	if manager.options.Common.Mode == ModeVoiceTranscribe {
		transcribeCtx, transcribeCancel := context.WithTimeout(ctx, defaultVoiceSTTTimeout)
		realtimeResult, err = runner.Run(transcribeCtx)
		transcribeCancel()
		closeErr := client.Close()
		if err == nil {
			if len(realtimeResult.Errors) != 0 {
				err = errors.New(strings.Join(realtimeResult.Errors, "; "))
			}
		}
		if err == nil {
			agentClient, agentErr := manager.newAgentGateway(active.gatewayURL, active.Credentials.Get("gateway"))
			if agentErr != nil {
				err = agentErr
			} else {
				agentCtx, agentCancel := context.WithTimeout(ctx, active.AgentTimeout)
				err = manager.runAgentWithTranscript(agentCtx, active, agentClient, &realtimeResult)
				agentCancel()
				err = errors.Join(err, agentClient.Close())
			}
		}
		err = errors.Join(err, closeErr)
	} else {
		runCtx, cancel := context.WithTimeout(ctx, active.AgentTimeout)
		realtimeResult, err = runner.Run(runCtx)
		cancel()
	}
	realtimeResult = redactRealtimeResult(realtimeResult, active)
	err = active.Credentials.RedactErr(err)
	resultPath, writeErr := manager.writeRealtimeResult(active, realtimeResult)
	if writeErr == nil {
		active.Artifacts.Add(resultPath)
	}
	err = errors.Join(err, writeErr)
	if len(realtimeResult.Errors) != 0 && manager.options.Common.Mode != ModeVoiceTranscribe {
		err = errors.Join(err, errors.New(strings.Join(realtimeResult.Errors, "; ")))
	}
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.runtime.Options.CleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		return active.FailedResult(started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: realtimeResult.FinalText(), Duration: time.Since(started),
		LogPaths: active.Artifacts.Paths(),
	}, nil
}

func (manager *Manager) runAgentWithTranscript(ctx context.Context, active *session, client gatewayConnection, result *realtimeclient.Result) error {
	transcript := strings.TrimSpace(result.Transcript)
	if len(result.TranscriptDoneParts) != 0 {
		parts := make([]string, 0, len(result.TranscriptDoneParts))
		for _, part := range result.TranscriptDoneParts {
			if text := strings.TrimSpace(part); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) != 0 {
			transcript = strings.Join(parts, "\n")
		}
	}
	if transcript == "" {
		err := errors.New("missing_transcript: OpenClaw voice-transcribe mode returned no text")
		result.AppendError(err.Error())
		return err
	}
	thinking := requestedThinking(active.Model)
	connectSummary, err := client.Connect(ctx, gatewayclient.ConnectOptions{})
	if err == nil && !connectSummary.HasScope("operator.write") {
		err = errors.New("OpenClaw agent gateway requires operator.write scope")
	}
	if err != nil {
		result.AppendError(err.Error())
		return err
	}
	agentResult, err := client.Agent(ctx, gatewayclient.AgentRequest{
		Message: transcript, SessionKey: "agent:main:aries-" + active.safeTaskID,
		IdempotencyKey: active.agentIdempotency, Thinking: thinking,
	})
	if agentResult.RunID != "" {
		result.AgentRunIDs = append(result.AgentRunIDs, agentResult.RunID)
	}
	result.AgentQuestionUsed = transcript
	result.OutputText = agentResult.Text
	if err != nil {
		result.AppendError(err.Error())
		return err
	}
	result.AgentConsultOK = true
	return nil
}

func newGatewayClientWithDisposition(rawURL string, token []byte, scopes []string, disposition gatewayclient.EventDisposition) (gatewayConnection, error) {
	dialer, err := gatewayclient.NewWebSocketDialer(gatewayclient.WebSocketOptions{URL: rawURL})
	if err != nil {
		return nil, err
	}
	return gatewayclient.New(dialer, gatewayclient.Options{Token: string(token), Scopes: scopes, EventDisposition: disposition})
}

func gatewayEventDisposition(mode string) gatewayclient.EventDisposition {
	if mode == ModeAgent {
		return gatewayclient.EventDispositionResponseOnly
	}
	return gatewayclient.EventDispositionDelivery
}

func gatewayScopes(mode string) []string {
	if isRealtimeMode(mode) {
		return []string{"operator.read", "operator.write"}
	}
	return []string{"operator.write"}
}

func newRealtimeRunner(gateway realtimeclient.Gateway, options realtimeclient.Options) (realtimeRunner, error) {
	return realtimeclient.New(gateway, options)
}

func newSpeechClient(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
	return audioinput.NewSpeechClient(options)
}

// Keep the Gateway's task-level choice consistent with native model params.
// The custom provider exposes xhigh; extra_body translates DeepSeek max exactly.
func requestedThinking(model core.ModelConfig) string {
	switch model.ReasoningEffort {
	case "off", "none":
		return "off"
	case "max":
		return "xhigh"
	case "":
		if model.BaseURL == "https://api.deepseek.com" && (model.Model == "deepseek-flash" || model.Model == "deepseek-v4-flash" || model.Model == "deepseek-v4-pro") {
			return "off"
		}
		return ""
	default:
		return model.ReasoningEffort
	}
}

func (manager *Manager) gatewayURL(ctx context.Context, active *session) (string, error) {
	address, err := manager.runtime.Options.Deployment.Address(ctx, active.ID, 18789)
	if err != nil {
		return "", fmt.Errorf("resolve OpenClaw gateway: %w", err)
	}
	return "ws://" + address, nil
}

func (manager *Manager) synthesizeVoiceInstruction(ctx context.Context, active *session, instruction string) (string, []string, error) {
	instructionPath := filepath.Join(active.ArtifactDir, audioinput.VoiceInstructionTextFile)
	if err := privatefiles.WriteArtifact(instructionPath, []byte(instruction)); err != nil {
		return "", nil, fmt.Errorf("write realtime voice instruction: %w", err)
	}
	voiceCredentials := harness.NewCredentials("OpenClaw")
	defer voiceCredentials.Clear()
	if ok, err := voiceCredentials.Load("voice", manager.options.Realtime.TTS.APIKeyEnv, manager.runtime.Options.APIKeyLookup); err != nil {
		return "", []string{instructionPath}, fmt.Errorf("OpenClaw realtime TTS API key: %w", err)
	} else if !ok {
		return "", []string{instructionPath}, fmt.Errorf("OpenClaw realtime TTS API-key environment %q is not set", manager.options.Realtime.TTS.APIKeyEnv)
	}
	apiKey := voiceCredentials.Get("voice")
	return audioinput.SynthesizeVoiceInstruction(ctx, instruction, audioinput.VoiceInstructionOptions{
		ArtifactDir:     active.ArtifactDir,
		InstructionPath: instructionPath,
		ErrorLabel:      "realtime",
		TTSErrorLabel:   "realtime",
		Provider:        manager.options.Realtime.TTS.Provider,
		BaseURL:         manager.options.Realtime.TTS.BaseURL,
		APIKey:          apiKey,
		Model:           manager.options.Realtime.TTS.Model,
		Voice:           manager.options.Realtime.TTS.Voice,
		Instructions:    manager.options.Realtime.TTS.Instructions,
		Speed:           manager.options.Realtime.TTS.Speed,
		Timeout:         manager.options.Realtime.TTS.Timeout,
		NewSpeech: func(options audioinput.SpeechClientOptions) (audioinput.SpeechSynthesizer, error) {
			synthesizer, err := manager.newSpeech(options)
			voiceCredentials.Clear()
			return synthesizer, err
		},
		WriteArtifact: privatefiles.WriteArtifact,
	})
}

func (manager *Manager) realtimeAudioProvider(audioPath string) realtimeclient.AudioProvider {
	return func(session realtimeclient.SessionInfo) (realtimeclient.Audio, error) {
		pcm, sourceRate, err := audioinput.ReadWAVFilePCM16Mono(audioPath)
		if err != nil {
			return realtimeclient.Audio{}, fmt.Errorf("read realtime audio: %w", err)
		}
		if manager.options.Realtime.TrailingSilenceMillis > 0 {
			silence, err := audioinput.SilencePCM16(sourceRate, manager.options.Realtime.TrailingSilenceMillis)
			if err != nil {
				return realtimeclient.Audio{}, err
			}
			if len(silence) > audioinput.MaxPCM16Bytes-len(pcm) {
				return realtimeclient.Audio{}, errors.New("realtime audio with trailing silence exceeds size bound")
			}
			pcm = append(pcm, silence...)
		}
		encoding := session.InputEncoding
		if encoding == "" {
			encoding = realtimeclient.DefaultInputEncoding
		}
		rate := session.InputSampleRateHz
		if rate <= 0 {
			rate = realtimeclient.DefaultInputSampleRate
		}
		prepared, err := audioinput.PrepareAudio(pcm, sourceRate, encoding, rate)
		if err != nil {
			return realtimeclient.Audio{}, err
		}
		return realtimeclient.Audio{
			Data: prepared.Data, Rate: prepared.Rate,
			BytesPerSample: prepared.BytesPerSample, Encoding: prepared.Encoding,
		}, nil
	}
}

func (manager *Manager) writeRealtimeResult(active *session, result realtimeclient.Result) (string, error) {
	result = redactRealtimeResult(result, active)
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode realtime result: %w", err)
	}
	content = append(content, '\n')
	content = active.Credentials.Redact(content)
	path := filepath.Join(active.ArtifactDir, "realtime-result.json")
	if err := privatefiles.WriteArtifact(path, content); err != nil {
		return "", fmt.Errorf("write realtime result: %w", err)
	}
	return path, nil
}

func (manager *Manager) writeAgentResult(active *session, summary gatewayclient.ConnectSummary, result gatewayclient.AgentResult, runErr error) (string, error) {
	summary = redactConnectSummary(summary, active)
	result = redactAgentResult(result, active)
	runErr = active.Credentials.RedactErr(runErr)
	errorText := ""
	if runErr != nil {
		errorText = runErr.Error()
	}
	content, err := json.MarshalIndent(struct {
		Role     string   `json:"role"`
		Scopes   []string `json:"scopes"`
		RunID    string   `json:"run_id"`
		Response string   `json:"response"`
		Error    string   `json:"error,omitempty"`
	}{Role: summary.Role, Scopes: append([]string(nil), summary.Scopes...), RunID: result.RunID, Response: result.Text, Error: errorText}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode OpenClaw agent result: %w", err)
	}
	content = append(content, '\n')
	content = active.Credentials.Redact(content)
	path := filepath.Join(active.ArtifactDir, "agent-result.json")
	if err := privatefiles.WriteArtifact(path, content); err != nil {
		return "", fmt.Errorf("write OpenClaw agent result: %w", err)
	}
	return path, nil
}

func durationOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
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

func (manager *Manager) waitReady(ctx context.Context, active *session) error {
	probe := `const http=require("http");const port=Number(process.argv[1]);const r=http.get({host:"127.0.0.1",port,path:"/readyz",timeout:1000},s=>{let b="";s.on("data",c=>b+=c);s.on("end",()=>{try{const j=JSON.parse(b);process.exit(s.statusCode===200&&j.ready===true&&process.getuid()===1000?0:1)}catch{process.exit(1)}})});r.on("timeout",()=>r.destroy());r.on("error",()=>process.exit(1));`
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		result, err := manager.runtime.Options.Deployment.Exec(probeCtx, active.ID, core.Command{Path: "node", Args: []string{"-e", probe, upstreamGatewayPort}, Dir: "/app"})
		cancel()
		if err == nil && result.ExitCode == 0 {
			return nil
		}
		running, inspectErr := manager.runtime.Options.Deployment.Running(ctx, active.ID)
		if inspectErr != nil {
			return fmt.Errorf("inspect OpenClaw readiness: %w", inspectErr)
		}
		if !running {
			return errors.New("OpenClaw gateway exited before readiness")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await OpenClaw readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) runtimeArchive(active *session, configuration []byte) ([]byte, error) {
	clientBytes, err := privatefiles.Read(active.Endpoint.ClientSourceFile, 0o555)
	if err != nil {
		return nil, fmt.Errorf("read OpenClaw SSH client: %w", err)
	}
	defer clear(clientBytes)
	mcpHostVars := make([]string, 0, len(active.Credentials.MCPFiles()))
	for hostVar := range active.Credentials.MCPFiles() {
		mcpHostVars = append(mcpHostVars, hostVar)
	}
	sort.Strings(mcpHostVars)
	files := map[string]harness.ArchiveFile{
		"run/aries/openclaw.json":        {Content: configuration, Mode: 0o600},
		"run/aries/model.key":            {Content: active.Credentials.Get("model"), Mode: 0o600},
		"run/aries/gateway.key":          {Content: active.Credentials.Get("gateway"), Mode: 0o600},
		"run/aries/launch":               {Content: launcherScript(active.Model.APIKeyEnv, manager.realtimeAPIKeyEnv(active), len(active.Credentials.Get("extract")) != 0, mcpHostVars...), Mode: 0o555},
		"run/aries/gateway-proxy.js":     {Content: gatewayProxyScript(), Mode: 0o555},
		"run/aries/gateway-launcher":     {Content: gatewayLauncherScript(), Mode: 0o555},
		"opt/aries/bin/aries-ssh-client": {Content: clientBytes, Mode: 0o555},
	}
	if len(active.Credentials.Get("voice")) != 0 {
		files["run/aries/realtime.key"] = harness.ArchiveFile{Content: active.Credentials.Get("voice"), Mode: 0o600}
	}
	if len(active.Credentials.Get("extract")) != 0 {
		files["run/aries/tavily.key"] = harness.ArchiveFile{Content: active.Credentials.Get("extract"), Mode: 0o600}
	}
	for _, hostVar := range mcpHostVars {
		files["run/aries/mcp_"+hostVar+".key"] = harness.ArchiveFile{Content: active.Credentials.MCPFiles()[hostVar], Mode: 0o600}
	}
	for name, file := range files {
		file.UID, file.GID = 1000, 1000
		files[name] = file
	}
	directories := []harness.ArchiveDirectory{
		{Name: "run/aries", Mode: 0o700, UID: 1000, GID: 1000},
		{Name: "run/aries/ssh", Mode: 0o700, UID: 1000, GID: 1000},
		{Name: "opt/aries", Mode: 0o755, UID: 1000, GID: 1000},
		{Name: "opt/aries/bin", Mode: 0o755, UID: 1000, GID: 1000},
		{Name: "home/node/.openclaw", Mode: 0o700, UID: 1000, GID: 1000},
		{Name: "home/node/.openclaw/.aries", Mode: 0o700, UID: 1000, GID: 1000},
	}
	return harness.StageArchive(directories, files)
}

func (manager *Manager) realtimeAPIKeyEnv(active *session) string {
	if !isRealtimeMode(manager.options.Common.Mode) || len(active.Credentials.Get("voice")) == 0 {
		return ""
	}
	return manager.options.Realtime.TTS.APIKeyEnv
}

func gatewayLauncherScript() []byte {
	return []byte(`#!/bin/sh
set -eu
node /run/aries/gateway-proxy.js &
proxy_pid=$!
cleanup() {
  kill "$proxy_pid" 2>/dev/null || true
  wait "$proxy_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM
set +e
openclaw gateway run --port ` + upstreamGatewayPort + ` --auth token --bind loopback
status=$?
set -e
cleanup
exit "$status"
`)
}

func gatewayProxyScript() []byte {
	return []byte(`#!/usr/bin/env node
const net = require("net");
const listenHost = "0.0.0.0";
const listenPort = ` + gatewayListenPort + `;
const targetHost = "127.0.0.1";
const targetPort = ` + upstreamGatewayPort + `;
const server = net.createServer((client) => {
  const upstream = net.connect({host: targetHost, port: targetPort});
  const close = () => {
    client.destroy();
    upstream.destroy();
  };
  client.on("error", close);
  upstream.on("error", close);
  client.pipe(upstream);
  upstream.pipe(client);
});
server.on("error", (error) => {
  console.error("[aries-gateway-proxy] " + error.message);
  process.exit(1);
});
server.listen(listenPort, listenHost, () => {
  console.error("[aries-gateway-proxy] listening " + listenHost + ":" + listenPort + " -> " + targetHost + ":" + targetPort);
});
`)
}

func (manager *Manager) stopSession(ctx context.Context, active *session) error {
	if active == nil {
		return nil
	}
	if err := manager.runtime.Remove(ctx, active.Occurrence); err != nil {
		return err
	}
	return nil
}

func (manager *Manager) collectArtifacts(ctx context.Context, active *session) error {
	var errs []error
	logs, err := manager.runtime.Options.Deployment.Logs(ctx, active.ID, maxDockerOutput)
	if err != nil {
		errs = append(errs, fmt.Errorf("collect OpenClaw gateway logs: %w", err))
	} else {
		content := privatefiles.FilterLogs(logs, maxDockerOutput, active.Credentials.Secrets()...)
		path := filepath.Join(active.ArtifactDir, "gateway.log")
		if err := privatefiles.WriteArtifact(path, content); err != nil {
			errs = append(errs, err)
		} else {
			active.Artifacts.Add(path)
		}
	}
	telemetryPaths, telemetryErr := manager.collectTelemetry(ctx, active)
	if telemetryErr != nil {
		errs = append(errs, telemetryErr)
	} else {
		active.Artifacts.Add(telemetryPaths...)
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
		errs = append(errs, fmt.Errorf("write OpenClaw telemetry index: %w", err))
	}
	return errors.Join(errs...)
}

func (manager *Manager) collectTelemetry(ctx context.Context, active *session) ([]string, error) {
	archiveReader, _, err := manager.runtime.Options.Deployment.DownloadArchive(ctx, active.ID, stateContainerPath+"/agents/main/sessions")
	if err != nil {
		if errors.Is(err, runner.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("collect OpenClaw telemetry: %w", err)
	}
	defer archiveReader.Close()
	archive, err := io.ReadAll(io.LimitReader(archiveReader, maxDockerOutput+1))
	if err != nil || len(archive) > maxDockerOutput {
		return nil, errors.New("OpenClaw telemetry archive exceeded its bound")
	}
	return extractTelemetry(active.ArtifactDir, archive, active.Credentials.Secrets()...)
}

func extractTelemetry(artifactDir string, archive []byte, secrets ...[]byte) ([]string, error) {
	usedNames := make(map[string]bool)
	reader := tar.NewReader(bytes.NewReader(archive))
	var paths []string
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return paths, err
		}
		base := filepath.Base(filepath.Clean(header.Name))
		if (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) || header.Size < 0 || header.Size > maxDockerOutput ||
			(base != "sessions.json" && !strings.HasSuffix(base, ".jsonl") && !strings.Contains(strings.ToLower(base), "trajectory")) {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil || int64(len(content)) != header.Size {
			return paths, errors.New("OpenClaw telemetry archive is truncated")
		}
		name := base
		for index := 1; usedNames[name]; index++ {
			name = fmt.Sprintf("%d-%s", index, base)
		}
		usedNames[name] = true
		path := filepath.Join(artifactDir, "telemetry", name)
		if err := privatefiles.WriteArtifact(path, privatefiles.RedactSecrets(content, secrets...)); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func safeTaskID(value string) string {
	var output strings.Builder
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			output.WriteRune(r)
		} else {
			output.WriteByte('-')
		}
		if output.Len() >= 48 {
			break
		}
	}
	result := strings.Trim(output.String(), "-.")
	if result == "" {
		return "task"
	}
	return result
}

func redactRealtimeResult(result realtimeclient.Result, active *session) realtimeclient.Result {
	content, err := json.Marshal(result)
	if err != nil {
		return realtimeclient.Result{SchemaVersion: realtimeclient.ResultSchemaVersion, Errors: []string{"realtime result could not be safely encoded"}}
	}
	var tree any
	if err := json.Unmarshal(content, &tree); err != nil {
		return realtimeclient.Result{SchemaVersion: realtimeclient.ResultSchemaVersion, Errors: []string{"realtime result could not be safely decoded"}}
	}
	tree = redactRealtimeJSON(tree, active)
	content, err = json.Marshal(tree)
	if err != nil {
		return realtimeclient.Result{SchemaVersion: realtimeclient.ResultSchemaVersion, Errors: []string{"redacted realtime result could not be safely encoded"}}
	}
	var redacted realtimeclient.Result
	if err := json.Unmarshal(content, &redacted); err != nil {
		return realtimeclient.Result{SchemaVersion: realtimeclient.ResultSchemaVersion, Errors: []string{"redacted realtime result could not be safely decoded"}}
	}
	return redacted
}

func redactConnectSummary(summary gatewayclient.ConnectSummary, active *session) gatewayclient.ConnectSummary {
	summary.Role = string(active.Credentials.Redact([]byte(summary.Role)))
	for index := range summary.Scopes {
		summary.Scopes[index] = string(active.Credentials.Redact([]byte(summary.Scopes[index])))
	}
	return summary
}

func redactAgentResult(result gatewayclient.AgentResult, active *session) gatewayclient.AgentResult {
	result.RunID = string(active.Credentials.Redact([]byte(result.RunID)))
	result.Text = string(active.Credentials.Redact([]byte(result.Text)))
	return result
}

func redactRealtimeJSON(value any, active *session) any {
	switch typed := value.(type) {
	case string:
		return string(active.Credentials.Redact([]byte(typed)))
	case []any:
		for index := range typed {
			typed[index] = redactRealtimeJSON(typed[index], active)
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redactedKey := string(active.Credentials.Redact([]byte(key)))
			redacted[redactedKey] = redactRealtimeJSON(item, active)
		}
		return redacted
	default:
		return value
	}
}
