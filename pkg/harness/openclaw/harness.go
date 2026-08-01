package openclaw

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	audioinput "github.com/hyscale-lab/aries/pkg/audio"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	gatewayclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/gateway"
	realtimeclient "github.com/hyscale-lab/aries/pkg/harness/openclaw/realtime"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const (
	defaultDockerSocket   = "/var/run/docker.sock"
	defaultCleanupTimeout = 30 * time.Second
	defaultStartTimeout   = 45 * time.Second
	defaultAgentTimeout   = 20 * time.Minute
	maxDockerOutput       = 16 << 20
	maxAPIKeyBytes        = 16 << 10
	gracefulStopSeconds   = 5
	execTrailerKeep       = 256
	gatewayListenPort     = "18789"
	gatewayPortSpec       = gatewayListenPort + "/tcp"
	realtimeGatewayPort   = "18790"
)

const execShell = `token=$1
shift
"$@"
status=$?
printf '\036ARIES_OPENCLAW_EXIT_%s=%s\037' "$token" "$status" >&2
exit "$status"`

var gatewayPort = network.MustParsePort(gatewayPortSpec)

var realtimeGatewayCommand = []string{"/run/aries/realtime-gateway"}

const (
	ModeAgent    = "agent"
	ModeRealtime = "realtime"
)

// Options are the host-local inputs to one upstream OpenClaw container.
type Options struct {
	Image          string
	OutputDir      string
	DockerSocket   string
	APIKeyLookup   func(string) ([]byte, bool)
	Mode           string
	Realtime       RealtimeOptions
	CleanupTimeout time.Duration
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
	Logger         *logrus.Logger
}

type RealtimeOptions struct {
	AgentQuestionTemplate string
	TTS                   RealtimeTTSOptions
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

type RealtimeTTSOptions struct {
	Provider     string
	BaseURL      string
	APIKeyEnv    string
	Model        string
	Voice        string
	Instructions string
	Speed        *float64
	Timeout      time.Duration
}

// dockerClient is the small official Engine SDK surface used by the harness.
// Tests replace it with a typed fake; production uses *client.Client directly.
type dockerClient interface {
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	CopyToContainer(context.Context, string, client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerTop(context.Context, string, client.ContainerTopOptions) (client.ContainerTopResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Manager struct {
	client         dockerClient
	image          string
	outputDir      string
	cleanupTimeout time.Duration
	startTimeout   time.Duration
	agentTimeout   time.Duration
	logger         *logrus.Logger
	apiKeyLookup   func(string) ([]byte, bool)
	mode           string
	realtime       RealtimeOptions
	newID          func() (string, error)
	newGateway     func(string, []byte) (gatewayConnection, error)
	newRealtime    func(realtimeclient.RealtimeGateway, realtimeclient.RealtimeRunnerOptions) (realtimeRunner, error)
	newSpeech      func(audioinput.SpeechClientOptions) (speechSynthesizer, error)

	mu        sync.Mutex
	active    *session
	stopping  bool
	stopDone  chan struct{}
	stopErr   error
	closeOnce sync.Once
	closeErr  error
}

type realtimeRunner interface {
	Run(context.Context) (realtimeclient.RealtimeResult, error)
}

type gatewayConnection interface {
	realtimeclient.RealtimeGateway
	Agent(context.Context, gatewayclient.AgentRequest) (gatewayclient.AgentResult, error)
}

type speechSynthesizer interface {
	Synthesize(context.Context, audioinput.SpeechRequest) (audioinput.SpeechResult, error)
	Close()
}

// Close releases the manager's Docker SDK transport after lifecycle cleanup.
func (manager *Manager) Close() error {
	if manager == nil {
		return nil
	}
	manager.closeOnce.Do(func() {
		if closer, ok := manager.client.(interface{ Close() error }); ok {
			manager.closeErr = closer.Close()
		}
	})
	return manager.closeErr
}

type session struct {
	runID            string
	taskID           string
	safeTaskID       string
	attemptID        string
	containerName    string
	containerID      string
	artifactDir      string
	endpoint         core.ToolEndpoint
	model            core.ModelConfig
	agentTimeout     time.Duration
	apiKey           []byte
	realtimeAPIKey   []byte
	gatewayToken     []byte
	gatewayURL       string
	agentIdempotency string
	runAttempted     bool
	logPaths         []string
}

var _ runner.AgentHarness = (*Manager)(nil)

// New constructs a harness without contacting Docker.
func New(options Options) (*Manager, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("OpenClaw image: %w", err)
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("OpenClaw output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenClaw output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare OpenClaw output directory: %w", err)
	}
	if options.DockerSocket == "" {
		options.DockerSocket = defaultDockerSocket
	}
	host := options.DockerSocket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-openclaw/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = defaultStartTimeout
	}
	if options.AgentTimeout <= 0 {
		options.AgentTimeout = defaultAgentTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = environmentAPIKeyLookup
	}
	if options.Mode == "" {
		options.Mode = ModeAgent
	}
	switch options.Mode {
	case ModeAgent:
		if options.Realtime != (RealtimeOptions{}) {
			return nil, errors.New("OpenClaw realtime options require realtime mode")
		}
	case ModeRealtime:
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
		return nil, errors.New("OpenClaw mode must be agent or realtime")
	}
	return &Manager{
		client: api, image: options.Image, outputDir: outputDir,
		cleanupTimeout: options.CleanupTimeout, startTimeout: options.StartTimeout,
		agentTimeout: options.AgentTimeout, logger: options.Logger,
		apiKeyLookup: options.APIKeyLookup, mode: options.Mode, realtime: options.Realtime, newID: randomID,
		newGateway: func(rawURL string, token []byte) (gatewayConnection, error) {
			return newGatewayClient(rawURL, token, gatewayScopes(options.Mode))
		},
		newRealtime: newRealtimeRunner, newSpeech: newSpeechClient,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return errors.New("OpenClaw harness is already active")
	}
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateTaskID(request.TaskID); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("OpenClaw task timeout must not be negative")
	}
	resources, err := harnessResources(request)
	if err != nil {
		return err
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.agentTimeout
	}
	configuration, err := renderConfig(request.Model, request.Endpoint)
	if err != nil {
		return err
	}
	apiKeySource, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	if !ok {
		clear(apiKeySource)
		return fmt.Errorf("OpenClaw API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeySource)
	clear(apiKeySource)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return err
	}
	var realtimeAPIKey []byte
	if manager.mode == ModeRealtime {
		realtimeKeySource, ok := manager.apiKeyLookup(manager.realtime.TTS.APIKeyEnv)
		if !ok {
			clear(apiKey)
			clear(realtimeKeySource)
			return fmt.Errorf("OpenClaw realtime API-key environment %q is not set", manager.realtime.TTS.APIKeyEnv)
		}
		realtimeAPIKey = bytes.Clone(realtimeKeySource)
		clear(realtimeKeySource)
		if err := validateAPIKey(realtimeAPIKey); err != nil {
			clear(apiKey)
			clear(realtimeAPIKey)
			return fmt.Errorf("OpenClaw realtime API key: %w", err)
		}
	}
	if bytes.Contains(configuration, apiKey) {
		clear(apiKey)
		clear(realtimeAPIKey)
		return errors.New("rendered OpenClaw config contains the API-key value")
	}
	if len(realtimeAPIKey) != 0 && bytes.Contains(configuration, realtimeAPIKey) {
		clear(apiKey)
		clear(realtimeAPIKey)
		return errors.New("rendered OpenClaw config contains the realtime API-key value")
	}
	command := manager.gatewayCommand()
	containerConfig := &container.Config{
		Image: manager.image,
		Env:   []string{"OPENCLAW_CONFIG_PATH=" + configContainerPath},
		Cmd:   append([]string{launcherPath}, command...),
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "openclaw-harness",
			"aries.component": "harness",
			"aries.run":       request.RunID, "aries.task": request.TaskID,
		},
	}
	hostConfig := &container.HostConfig{NetworkMode: container.NetworkMode(request.Endpoint.Network), Resources: resources}
	containerConfig.ExposedPorts = network.PortSet{gatewayPort: struct{}{}}
	hostConfig.PortBindings = network.PortMap{gatewayPort: []network.PortBinding{{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: ""}}}
	id, err := manager.newID()
	if err != nil {
		clear(apiKey)
		clear(realtimeAPIKey)
		return fmt.Errorf("generate OpenClaw harness ID: %w", err)
	}
	gatewayToken, err := randomSecret(32)
	if err != nil {
		clear(apiKey)
		clear(realtimeAPIKey)
		return fmt.Errorf("generate OpenClaw gateway token: %w", err)
	}
	agentIdempotency, err := randomID()
	if err != nil {
		clear(apiKey)
		clear(realtimeAPIKey)
		clear(gatewayToken)
		return fmt.Errorf("generate OpenClaw agent idempotency key: %w", err)
	}
	active := &session{
		runID: request.RunID, taskID: request.TaskID, safeTaskID: safeTaskID(request.TaskID), attemptID: id,
		containerName: "aries-openclaw-" + id, artifactDir: filepath.Join(manager.outputDir, request.TaskID, "harness"),
		endpoint: request.Endpoint, model: request.Model, agentTimeout: agentTimeout, apiKey: apiKey, realtimeAPIKey: realtimeAPIKey, gatewayToken: gatewayToken, agentIdempotency: agentIdempotency,
	}
	containerConfig.Labels["aries.attempt"] = active.attemptID
	fail := func(primary error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout)
		cleanupErr := manager.stopSession(cleanupCtx, active)
		cancel()
		if cleanupErr != nil {
			manager.active = active
			manager.stopErr = cleanupErr
			return errors.Join(primary, fmt.Errorf("rollback partial OpenClaw harness: %w", cleanupErr))
		}
		_ = os.RemoveAll(active.artifactDir)
		return primary
	}
	if err := ensurePrivateDirectory(active.artifactDir); err != nil {
		return fail(fmt.Errorf("create OpenClaw artifact directory: %w", err))
	}
	configArtifact := filepath.Join(active.artifactDir, "openclaw.json")
	if err := writeArtifact(configArtifact, configuration); err != nil {
		return fail(fmt.Errorf("retain rendered OpenClaw config: %w", err))
	}
	active.logPaths = appendUnique(active.logPaths, configArtifact)
	archive, err := manager.runtimeArchive(active, configuration)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)
	created, err := manager.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: active.containerName, Config: containerConfig, HostConfig: hostConfig,
	})
	if err != nil {
		return fail(fmt.Errorf("create OpenClaw container: %w", err))
	}
	active.containerID = created.ID
	if strings.TrimSpace(active.containerID) == "" {
		return fail(errors.New("Docker returned an empty OpenClaw container ID"))
	}
	if _, err := manager.client.CopyToContainer(ctx, active.containerID, client.CopyToContainerOptions{
		DestinationPath: "/", Content: bytes.NewReader(archive), CopyUIDGID: true,
	}); err != nil {
		return fail(fmt.Errorf("copy private OpenClaw runtime: %w", err))
	}
	if err := manager.validateContainer(ctx, active); err != nil {
		return fail(err)
	}
	if _, err := manager.client.ContainerStart(ctx, active.containerID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("start OpenClaw container: %w", err))
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
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
	manager.stopErr = nil
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.taskID, "container": active.containerName}).Info("OpenClaw harness started")
	return nil
}

func harnessResources(request core.HarnessRequest) (container.Resources, error) {
	var resources container.Resources
	if request.CPU != nil {
		scaled := *request.CPU * 1e9
		if *request.CPU <= 0 || math.IsNaN(*request.CPU) || math.IsInf(*request.CPU, 0) || scaled >= math.Exp2(63) {
			return container.Resources{}, errors.New("OpenClaw CPU must be finite, positive, and convert to NanoCPUs below 2^63")
		}
		resources.NanoCPUs = int64(scaled)
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt64>>20 {
			return container.Resources{}, fmt.Errorf("OpenClaw memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)
		}
		resources.Memory = int64(*request.MemoryMB) << 20
	}
	return resources, nil
}

func (manager *Manager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	active := manager.active
	if active == nil {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw harness is not started")
	}
	if active.runAttempted {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw harness accepts exactly one task instruction")
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw task instruction is invalid")
	}
	active.runAttempted = true
	manager.mu.Unlock()

	if manager.mode == ModeRealtime {
		return manager.runRealtime(ctx, active, instruction, started)
	}
	runCtx, cancel := context.WithTimeout(ctx, active.agentTimeout)
	client, err := manager.newGateway(active.gatewayURL, active.gatewayToken)
	if err != nil {
		cancel()
		return failedHarnessResult(active, started, err), err
	}
	defer client.Close()
	connectSummary, err := client.Connect(runCtx, gatewayclient.ConnectOptions{})
	var agentResult gatewayclient.AgentResult
	if err == nil && !connectSummary.HasScope("operator.write") {
		err = errors.New("OpenClaw agent gateway requires operator.write scope")
	}
	if err == nil {
		thinking := ""
		if disablesThinking(active.model) {
			thinking = "off"
		}
		agentResult, err = client.Agent(runCtx, gatewayclient.AgentRequest{
			Message: instruction, SessionKey: "agent:main:aries-" + active.safeTaskID,
			IdempotencyKey: active.agentIdempotency, Thinking: thinking,
		})
	}
	cancel()
	resultPath, writeErr := manager.writeAgentResult(active, connectSummary, agentResult, err)
	if writeErr == nil {
		active.logPaths = appendUnique(active.logPaths, resultPath)
	}
	err = errors.Join(err, writeErr)
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		return failedHarnessResult(active, started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: agentResult.Text, Duration: time.Since(started),
		LogPaths: append([]string(nil), active.logPaths...),
	}, nil
}

func (manager *Manager) runRealtime(ctx context.Context, active *session, instruction string, started time.Time) (core.HarnessResult, error) {
	audioPath, speechPaths, synthErr := manager.synthesizeRealtimeAudio(ctx, active, instruction)
	if len(speechPaths) != 0 {
		active.logPaths = appendUnique(active.logPaths, speechPaths...)
	}
	if synthErr != nil {
		return failedHarnessResult(active, started, synthErr), synthErr
	}
	client, err := manager.newGateway(active.gatewayURL, active.gatewayToken)
	if err != nil {
		return failedHarnessResult(active, started, err), err
	}
	runner, err := manager.newRealtime(client, realtimeclient.RealtimeRunnerOptions{
		OriginalPrompt:        instruction,
		SessionKey:            "agent:main:aries-" + active.safeTaskID,
		Provider:              manager.realtime.Provider,
		Model:                 manager.realtime.Model,
		Voice:                 manager.realtime.Voice,
		ReasoningEffort:       manager.realtime.ReasoningEffort,
		AudioProvider:         manager.realtimeAudioProvider(audioPath),
		ChunkDuration:         manager.realtime.ChunkDuration,
		ListenDuration:        durationOrDefault(manager.realtime.ListenDuration, active.agentTimeout),
		QuietDuration:         manager.realtime.QuietDuration,
		AgentWaitDuration:     durationOrDefault(manager.realtime.AgentWaitDuration, active.agentTimeout),
		ToolCallTimeout:       manager.realtime.ToolCallTimeout,
		AgentQuestionTemplate: manager.realtime.AgentQuestionTemplate,
		IncludeEvents:         manager.realtime.IncludeEvents,
		CloseGateway:          true,
	})
	if err != nil {
		_ = client.Close()
		return failedHarnessResult(active, started, err), err
	}
	runCtx, cancel := context.WithTimeout(ctx, active.agentTimeout)
	realtimeResult, err := runner.Run(runCtx)
	cancel()
	resultPath, writeErr := manager.writeRealtimeResult(active, realtimeResult)
	if writeErr == nil {
		active.logPaths = appendUnique(active.logPaths, resultPath)
	}
	err = errors.Join(err, writeErr)
	if len(realtimeResult.Errors) != 0 {
		err = errors.Join(err, errors.New(strings.Join(realtimeResult.Errors, "; ")))
	}
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		return failedHarnessResult(active, started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: realtimeResult.FinalText(), Duration: time.Since(started),
		LogPaths: append([]string(nil), active.logPaths...),
	}, nil
}

func newGatewayClient(rawURL string, token []byte, scopes []string) (gatewayConnection, error) {
	dialer, err := gatewayclient.NewWebSocketDialer(gatewayclient.WebSocketOptions{URL: rawURL})
	if err != nil {
		return nil, err
	}
	return gatewayclient.New(dialer, gatewayclient.Options{Token: string(token), Scopes: scopes})
}

func gatewayScopes(mode string) []string {
	if mode == ModeRealtime {
		return []string{"operator.read", "operator.write"}
	}
	return []string{"operator.write"}
}

func newRealtimeRunner(gateway realtimeclient.RealtimeGateway, options realtimeclient.RealtimeRunnerOptions) (realtimeRunner, error) {
	return realtimeclient.NewRealtimeRunner(gateway, options)
}

func newSpeechClient(options audioinput.SpeechClientOptions) (speechSynthesizer, error) {
	return audioinput.NewSpeechClient(options)
}

func disablesThinking(model core.ModelConfig) bool {
	return model.BaseURL == "https://api.deepseek.com" && (model.Model == "deepseek-v4-flash" || model.Model == "deepseek-v4-pro")
}

func (manager *Manager) gatewayCommand() []string {
	return append([]string(nil), realtimeGatewayCommand...)
}

func (manager *Manager) readyPort() string {
	return realtimeGatewayPort
}

func (manager *Manager) gatewayURL(ctx context.Context, active *session) (string, error) {
	inspection, err := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect OpenClaw gateway port: %w", err)
	}
	if inspection.Container.NetworkSettings == nil {
		return "", errors.New("OpenClaw gateway port binding is missing")
	}
	bindings := inspection.Container.NetworkSettings.Ports[gatewayPort]
	if len(bindings) != 1 {
		return "", fmt.Errorf("OpenClaw gateway requires exactly one host port binding, got %d", len(bindings))
	}
	binding := bindings[0]
	if binding.HostIP.String() != "127.0.0.1" {
		return "", errors.New("OpenClaw gateway host port must bind 127.0.0.1")
	}
	port, err := strconv.ParseUint(binding.HostPort, 10, 16)
	if err != nil || port == 0 {
		return "", errors.New("OpenClaw gateway host port was not published")
	}
	return "ws://" + net.JoinHostPort("127.0.0.1", binding.HostPort), nil
}

func (manager *Manager) synthesizeRealtimeAudio(ctx context.Context, active *session, instruction string) (string, []string, error) {
	instructionPath := filepath.Join(active.artifactDir, "voice-instruction.txt")
	if err := writeArtifact(instructionPath, []byte(instruction)); err != nil {
		return "", nil, fmt.Errorf("write realtime voice instruction: %w", err)
	}
	apiKeySource, ok := manager.apiKeyLookup(manager.realtime.TTS.APIKeyEnv)
	if !ok {
		clear(apiKeySource)
		return "", []string{instructionPath}, fmt.Errorf("OpenClaw realtime TTS API-key environment %q is not set", manager.realtime.TTS.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeySource)
	clear(apiKeySource)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return "", []string{instructionPath}, fmt.Errorf("OpenClaw realtime TTS API key: %w", err)
	}
	synthesizer, err := manager.newSpeech(audioinput.SpeechClientOptions{BaseURL: manager.realtime.TTS.BaseURL, APIKey: apiKey, Timeout: manager.realtime.TTS.Timeout})
	clear(apiKey)
	if err != nil {
		return "", []string{instructionPath}, fmt.Errorf("construct realtime TTS client: %w", err)
	}
	defer synthesizer.Close()
	result, err := synthesizer.Synthesize(ctx, audioinput.SpeechRequest{
		Text: instruction, Model: manager.realtime.TTS.Model, Voice: manager.realtime.TTS.Voice,
		Format: "wav", Instructions: manager.realtime.TTS.Instructions, Speed: manager.realtime.TTS.Speed,
	})
	if err != nil {
		return "", []string{instructionPath}, fmt.Errorf("synthesize realtime voice instruction: %w", err)
	}
	audioPath := filepath.Join(active.artifactDir, "voice-instruction.wav")
	if err := writeArtifact(audioPath, result.Audio); err != nil {
		clear(result.Audio)
		return "", []string{instructionPath}, fmt.Errorf("write realtime voice audio: %w", err)
	}
	clear(result.Audio)
	metaPath := filepath.Join(active.artifactDir, "voice-instruction.wav.meta.json")
	metadata := map[string]any{
		"provider":    manager.realtime.TTS.Provider,
		"model":       result.Model,
		"voice":       result.Voice,
		"format":      result.Format,
		"text_sha256": result.TextSHA256,
		"text_chars":  len(instruction),
		"cached":      false,
		"output_path": audioPath,
	}
	content, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return "", []string{instructionPath, audioPath}, fmt.Errorf("encode realtime TTS metadata: %w", err)
	}
	content = append(content, '\n')
	if err := writeArtifact(metaPath, content); err != nil {
		return "", []string{instructionPath, audioPath}, fmt.Errorf("write realtime TTS metadata: %w", err)
	}
	return audioPath, []string{instructionPath, audioPath, metaPath}, nil
}

func (manager *Manager) realtimeAudioProvider(audioPath string) realtimeclient.RealtimeAudioProvider {
	return func(session realtimeclient.TalkSessionInfo) (realtimeclient.RealtimeAudio, error) {
		pcm, sourceRate, err := audioinput.ReadWAVFilePCM16Mono(audioPath)
		if err != nil {
			return realtimeclient.RealtimeAudio{}, fmt.Errorf("read realtime audio: %w", err)
		}
		if manager.realtime.TrailingSilenceMillis > 0 {
			silence, err := audioinput.SilencePCM16(sourceRate, manager.realtime.TrailingSilenceMillis)
			if err != nil {
				return realtimeclient.RealtimeAudio{}, err
			}
			pcm = append(pcm, silence...)
		}
		encoding := session.InputEncoding
		if encoding == "" {
			encoding = realtimeclient.DefaultRealtimeInputEncoding
		}
		rate := session.InputSampleRateHz
		if rate <= 0 {
			rate = realtimeclient.DefaultRealtimeInputSampleRate
		}
		prepared, err := audioinput.PrepareAudio(pcm, sourceRate, encoding, rate)
		if err != nil {
			return realtimeclient.RealtimeAudio{}, err
		}
		return realtimeclient.RealtimeAudio{
			Data: prepared.Data, Rate: prepared.Rate,
			BytesPerSample: prepared.BytesPerSample, Encoding: prepared.Encoding,
		}, nil
	}
}

func (manager *Manager) writeRealtimeResult(active *session, result realtimeclient.RealtimeResult) (string, error) {
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode realtime result: %w", err)
	}
	content = append(content, '\n')
	path := filepath.Join(active.artifactDir, "realtime-result.json")
	if err := writeArtifact(path, content); err != nil {
		return "", fmt.Errorf("write realtime result: %w", err)
	}
	return path, nil
}

func (manager *Manager) writeAgentResult(active *session, summary gatewayclient.ConnectSummary, result gatewayclient.AgentResult, runErr error) (string, error) {
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
	content = redactSession(content, active)
	path := filepath.Join(active.artifactDir, "agent-result.json")
	if err := writeArtifact(path, content); err != nil {
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
	if manager.active == nil && !manager.stopping {
		err := manager.stopErr
		manager.mu.Unlock()
		return err
	}
	if manager.stopping {
		done := manager.stopDone
		manager.mu.Unlock()
		select {
		case <-done:
			manager.mu.Lock()
			err := manager.stopErr
			manager.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	active := manager.active
	manager.stopping = true
	manager.stopDone = make(chan struct{})
	done := manager.stopDone
	manager.mu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	err := manager.stopSession(cleanupCtx, active)
	cancel()

	manager.mu.Lock()
	manager.stopErr = err
	manager.stopping = false
	if active == nil || active.containerID == "" {
		manager.active = nil
	}
	close(done)
	manager.mu.Unlock()
	return err
}

type execResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

func (manager *Manager) execAttached(ctx context.Context, containerID string, command []string, workdir string) (execResult, error) {
	token, err := randomID()
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("generate OpenClaw exec token: %w", err)
	}
	wrapped := append([]string{"/bin/sh", "-c", execShell, "aries-openclaw-exec", token}, command...)
	created, err := manager.client.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStdout: true, AttachStderr: true, Cmd: wrapped, WorkingDir: workdir,
	})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("create OpenClaw exec: %w", err)
	}
	attached, err := manager.client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("attach OpenClaw exec: %w", err)
	}
	defer attached.Close()
	_ = attached.CloseWrite()
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = maxDockerOutput, maxDockerOutput
	trailer := newExecTrailer(&stderr, token)
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&stdout, trailer, attached.Reader)
		copyDone <- err
	}()
	inspectDone := make(chan client.ExecInspectResult, 1)
	inspectErr := make(chan error, 1)
	inspectCtx, cancelInspect := context.WithCancel(ctx)
	defer cancelInspect()
	go func() {
		inspection, err := manager.waitExec(inspectCtx, containerID, created.ID)
		if err != nil {
			inspectErr <- err
			return
		}
		inspectDone <- inspection
	}()
	var copyErr error
	streamDone := false
	finished := false
	for !finished {
		select {
		case <-ctx.Done():
			attached.Close()
			if !streamDone {
				<-copyDone
			}
			return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, ctx.Err()
		case err := <-inspectErr:
			attached.Close()
			if !streamDone {
				<-copyDone
			}
			return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, err
		case <-inspectDone:
			finished = true
		case <-trailer.done:
			finished = true
		case copyErr = <-copyDone:
			streamDone = true
		}
	}
	cancelInspect()
	if !streamDone {
		// The random trailer is emitted after the child and is the final stderr
		// record. Drain any buffered frames briefly; Docker 29 may never send EOF
		// until the hijacked connection is closed by the caller.
		select {
		case copyErr = <-copyDone:
			streamDone = true
		case <-time.After(200 * time.Millisecond):
			attached.Close()
			<-copyDone
			streamDone = true
			copyErr = nil
		}
	}
	if copyErr != nil {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, fmt.Errorf("read OpenClaw exec: %w", copyErr)
	}
	if stdout.exceeded || stderr.exceeded {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, errors.New("OpenClaw exec output exceeded its bound")
	}
	exitCode, err := trailer.Finish()
	if err != nil {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, err
	}
	return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}, nil
}

func (manager *Manager) waitExec(ctx context.Context, containerID, execID string) (client.ExecInspectResult, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		inspection, err := manager.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return client.ExecInspectResult{}, fmt.Errorf("inspect OpenClaw exec: %w", err)
		}
		if !inspection.Running {
			return inspection, nil
		}
		if inspection.PID > 0 {
			present, err := manager.containerHasPID(ctx, containerID, inspection.PID)
			if err != nil {
				return client.ExecInspectResult{}, fmt.Errorf("inspect OpenClaw exec process: %w", err)
			}
			if !present {
				return inspection, nil
			}
		}
		select {
		case <-ctx.Done():
			return client.ExecInspectResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager *Manager) containerHasPID(ctx context.Context, containerID string, pid int) (bool, error) {
	top, err := manager.client.ContainerTop(ctx, containerID, client.ContainerTopOptions{Arguments: []string{"-eo", "pid"}})
	if err != nil {
		return false, err
	}
	column := -1
	for index, title := range top.Titles {
		if strings.EqualFold(title, "PID") {
			column = index
			break
		}
	}
	if column < 0 {
		return false, errors.New("Docker top response has no PID column")
	}
	want := strconv.Itoa(pid)
	for _, process := range top.Processes {
		if column < len(process) && process[column] == want {
			return true, nil
		}
	}
	return false, nil
}

type execTrailer struct {
	destination io.Writer
	prefix      []byte
	buffer      bytes.Buffer
	done        chan struct{}
	once        sync.Once
}

func newExecTrailer(destination io.Writer, token string) *execTrailer {
	return &execTrailer{destination: destination, prefix: []byte("\x1eARIES_OPENCLAW_EXIT_" + token + "="), done: make(chan struct{})}
}

func (trailer *execTrailer) Write(content []byte) (int, error) {
	written, _ := trailer.buffer.Write(content)
	buffered := trailer.buffer.Bytes()
	if len(buffered) > 0 && buffered[len(buffered)-1] == '\x1f' && bytes.LastIndex(buffered[:len(buffered)-1], trailer.prefix) >= 0 {
		trailer.once.Do(func() { close(trailer.done) })
	}
	if excess := trailer.buffer.Len() - execTrailerKeep; excess > 0 {
		chunk := trailer.buffer.Next(excess)
		n, err := trailer.destination.Write(chunk)
		if err != nil {
			return 0, err
		}
		if n != len(chunk) {
			return 0, io.ErrShortWrite
		}
	}
	return written, nil
}

func (trailer *execTrailer) Finish() (int, error) {
	content := trailer.buffer.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\x1f' {
		return -1, errors.New("OpenClaw exec output is missing its exit trailer")
	}
	start := bytes.LastIndex(content[:len(content)-1], trailer.prefix)
	if start < 0 {
		return -1, errors.New("OpenClaw exec output has an invalid exit trailer")
	}
	exitCode, err := strconv.Atoi(string(content[start+len(trailer.prefix) : len(content)-1]))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return -1, errors.New("OpenClaw exec output has an invalid exit code")
	}
	if _, err := trailer.destination.Write(content[:start]); err != nil {
		return -1, fmt.Errorf("write OpenClaw exec stderr: %w", err)
	}
	return exitCode, nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	consumed := len(content)
	remaining := buffer.limit - buffer.Len()
	if len(content) > remaining {
		content = content[:max(0, remaining)]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return consumed, err
}

func (manager *Manager) waitReady(ctx context.Context, active *session) error {
	probe := `const http=require("http");const port=Number(process.argv[1]);const r=http.get({host:"127.0.0.1",port,path:"/readyz",timeout:1000},s=>{let b="";s.on("data",c=>b+=c);s.on("end",()=>{try{const j=JSON.parse(b);process.exit(s.statusCode===200&&j.ready===true&&process.getuid()===1000?0:1)}catch{process.exit(1)}})});r.on("timeout",()=>r.destroy());r.on("error",()=>process.exit(1));`
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		result, err := manager.execAttached(probeCtx, active.containerID, []string{"node", "-e", probe, manager.readyPort()}, "/app")
		cancel()
		if err == nil && result.exitCode == 0 {
			return nil
		}
		inspection, inspectErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil {
			return fmt.Errorf("inspect OpenClaw readiness: %w", inspectErr)
		}
		if inspection.Container.State == nil || !inspection.Container.State.Running {
			return errors.New("OpenClaw gateway exited before readiness")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await OpenClaw readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) validateContainer(ctx context.Context, active *session) error {
	inspection, err := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect OpenClaw container: %w", err)
	}
	containerInfo := inspection.Container
	if containerInfo.ID != active.containerID || containerInfo.Config == nil || containerInfo.HostConfig == nil {
		return errors.New("OpenClaw container inspection is incomplete")
	}
	configuration := containerInfo.Config
	if configuration.Image != manager.image || !equalStrings(configuration.Cmd, append([]string{launcherPath}, manager.gatewayCommand()...)) {
		return errors.New("OpenClaw image or gateway command differs from the pinned direct configuration")
	}
	if configuration.Labels["aries.managed"] != "true" || configuration.Labels["aries.kind"] != "openclaw-harness" || configuration.Labels["aries.component"] != "harness" ||
		configuration.Labels["aries.run"] != active.runID || configuration.Labels["aries.task"] != active.taskID || configuration.Labels["aries.attempt"] != active.attemptID {
		return errors.New("OpenClaw container labels do not match the task")
	}
	for _, value := range append(append([]string(nil), configuration.Env...), configuration.Cmd...) {
		if containsSecret(value, active.apiKey, active.realtimeAPIKey, active.gatewayToken) {
			return errors.New("OpenClaw secret entered Docker configuration")
		}
	}
	for _, value := range configuration.Labels {
		if containsSecret(value, active.apiKey, active.realtimeAPIKey, active.gatewayToken) {
			return errors.New("OpenClaw secret entered Docker labels")
		}
	}
	if string(containerInfo.HostConfig.NetworkMode) != active.endpoint.Network || len(containerInfo.Mounts) != 0 {
		return errors.New("OpenClaw container must use only the task network and no mounts")
	}
	return nil
}

func (manager *Manager) runtimeArchive(active *session, configuration []byte) ([]byte, error) {
	clientBytes, err := readStablePrivateFile(active.endpoint.ClientSourceFile, 0o555)
	if err != nil {
		return nil, fmt.Errorf("read OpenClaw SSH client: %w", err)
	}
	defer clear(clientBytes)
	identity, err := readStablePrivateFile(active.endpoint.IdentitySourceFile, 0o600)
	if err != nil {
		return nil, fmt.Errorf("read OpenClaw SSH identity: %w", err)
	}
	defer clear(identity)
	knownHosts, err := readStablePrivateFile(active.endpoint.KnownHostsSourceFile, 0o600)
	if err != nil {
		return nil, fmt.Errorf("read OpenClaw known-hosts: %w", err)
	}
	defer clear(knownHosts)
	files := map[string]stagedFile{
		"run/aries/openclaw.json":    {content: configuration, mode: 0o600},
		"run/aries/model.key":        {content: active.apiKey, mode: 0o600},
		"run/aries/gateway.key":      {content: active.gatewayToken, mode: 0o600},
		"run/aries/launch":           {content: launcherScript(active.model.APIKeyEnv, manager.realtimeAPIKeyEnv(active)), mode: 0o555},
		"run/aries/gateway-proxy.js": {content: gatewayProxyScript(), mode: 0o555},
		"run/aries/realtime-gateway": {content: realtimeGatewayScript(), mode: 0o555},
		"run/aries/ssh/id_ed25519":   {content: identity, mode: 0o600},
		"run/aries/ssh/known_hosts":  {content: knownHosts, mode: 0o600},
		"opt/aries/bin/aries-ssh":    {content: clientBytes, mode: 0o555},
	}
	if len(active.realtimeAPIKey) != 0 {
		files["run/aries/realtime.key"] = stagedFile{content: active.realtimeAPIKey, mode: 0o600}
	}
	return stageArchive(files)
}

func containsSecret(value string, secrets ...[]byte) bool {
	for _, secret := range secrets {
		if len(secret) != 0 && strings.Contains(value, string(secret)) {
			return true
		}
	}
	return false
}

func (manager *Manager) realtimeAPIKeyEnv(active *session) string {
	if manager.mode != ModeRealtime || len(active.realtimeAPIKey) == 0 {
		return ""
	}
	return manager.realtime.TTS.APIKeyEnv
}

func realtimeGatewayScript() []byte {
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
openclaw gateway run --port ` + realtimeGatewayPort + ` --auth token --bind loopback
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
const targetPort = ` + realtimeGatewayPort + `;
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

type stagedFile struct {
	content []byte
	mode    int64
}

func stageArchive(files map[string]stagedFile) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	directories := []string{"run/aries", "run/aries/ssh", "opt/aries", "opt/aries/bin", "home/node/.openclaw", "home/node/.openclaw/.aries"}
	for _, name := range directories {
		mode := int64(0o755)
		if strings.HasPrefix(name, "run/aries") || strings.HasPrefix(name, "home/node/.openclaw") {
			mode = 0o700
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode, Uid: 1000, Gid: 1000}); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file := files[name]
		if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("invalid staged OpenClaw path %q", name)
		}
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.content)), Uid: 1000, Gid: 1000}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.content); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (manager *Manager) stopSession(ctx context.Context, active *session) error {
	if active == nil {
		return nil
	}
	if active.containerID == "" {
		clearSessionSecrets(active)
		return nil
	}
	var errs []error
	inspection, inspectErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(inspectErr) {
		active.containerID = ""
		clearSessionSecrets(active)
		return nil
	}
	if inspectErr != nil {
		errs = append(errs, fmt.Errorf("inspect OpenClaw before stop: %w", inspectErr))
	}
	shouldStop := inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running
	if shouldStop {
		timeout := gracefulStopSeconds
		if _, err := manager.client.ContainerStop(ctx, active.containerID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("stop OpenClaw container: %w", err))
		}
		inspection, inspectErr = manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
			errs = append(errs, fmt.Errorf("inspect OpenClaw after stop: %w", inspectErr))
		}
		if !errdefs.IsNotFound(inspectErr) && (inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running) {
			if _, err := manager.client.ContainerKill(ctx, active.containerID, client.ContainerKillOptions{Signal: "KILL"}); err != nil && !errdefs.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("kill OpenClaw container: %w", err))
			}
		}
	}
	if _, err := manager.client.ContainerRemove(ctx, active.containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("remove OpenClaw container: %w", err))
	}
	if _, err := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{}); err == nil {
		errs = append(errs, errors.New("OpenClaw container remains after removal"))
		return errors.Join(errs...)
	} else if !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("verify OpenClaw removal: %w", err))
		return errors.Join(errs...)
	}
	active.containerID = ""
	clearSessionSecrets(active)
	if warning := errors.Join(errs...); warning != nil {
		manager.logger.WithContext(ctx).WithField("task_id", active.taskID).WithError(warning).Warn("OpenClaw cleanup recovered after lifecycle errors")
	}
	return nil
}

func (manager *Manager) collectArtifacts(ctx context.Context, active *session) error {
	var errs []error
	logs, err := manager.client.ContainerLogs(ctx, active.containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		errs = append(errs, fmt.Errorf("collect OpenClaw gateway logs: %w", err))
	} else {
		var stdout, stderr limitedBuffer
		stdout.limit, stderr.limit = maxDockerOutput, maxDockerOutput
		_, copyErr := stdcopy.StdCopy(&stdout, &stderr, logs)
		closeErr := logs.Close()
		if copyErr != nil || closeErr != nil || stdout.exceeded || stderr.exceeded {
			errs = append(errs, errors.Join(copyErr, closeErr, errors.New("OpenClaw gateway logs exceeded their bound")))
		} else {
			content := allowGatewayLogs(append(stdout.Bytes(), stderr.Bytes()...), active.apiKey, active.realtimeAPIKey, active.gatewayToken)
			path := filepath.Join(active.artifactDir, "gateway.log")
			if err := writeArtifact(path, content); err != nil {
				errs = append(errs, err)
			} else {
				active.logPaths = appendUnique(active.logPaths, path)
			}
		}
	}
	telemetryPaths, telemetryErr := manager.collectTelemetry(ctx, active)
	if telemetryErr != nil {
		errs = append(errs, telemetryErr)
	} else {
		active.logPaths = appendUnique(active.logPaths, telemetryPaths...)
	}
	index, err := json.MarshalIndent(struct {
		Paths []string `json:"paths"`
	}{Paths: telemetryRelativePaths(active.artifactDir, active.logPaths)}, "", "  ")
	if err == nil {
		index = append(index, '\n')
		path := filepath.Join(active.artifactDir, "telemetry.index.json")
		err = writeArtifact(path, index)
		if err == nil {
			active.logPaths = appendUnique(active.logPaths, path)
		}
	}
	if err != nil {
		errs = append(errs, fmt.Errorf("write OpenClaw telemetry index: %w", err))
	}
	return errors.Join(errs...)
}

func (manager *Manager) collectTelemetry(ctx context.Context, active *session) ([]string, error) {
	result, err := manager.client.CopyFromContainer(ctx, active.containerID, client.CopyFromContainerOptions{SourcePath: stateContainerPath + "/agents/main/sessions"})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("collect OpenClaw telemetry: %w", err)
	}
	defer result.Content.Close()
	archive, err := io.ReadAll(io.LimitReader(result.Content, maxDockerOutput+1))
	if err != nil || len(archive) > maxDockerOutput {
		return nil, errors.New("OpenClaw telemetry archive exceeded its bound")
	}
	return extractTelemetry(active.artifactDir, archive, active.apiKey, active.realtimeAPIKey, active.gatewayToken)
}

func failedHarnessResult(active *session, started time.Time, err error) core.HarnessResult {
	status := core.StatusFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = core.StatusCanceled
	}
	return core.HarnessResult{Status: status, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...), Error: err.Error()}
}

func readStablePrivateFile(path string, mode os.FileMode) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm() != mode || before.Size() < 1 || before.Size() > maxDockerOutput {
		return nil, errors.New("private source is not one bounded regular file with the required mode")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxDockerOutput+1))
	if err != nil || len(content) > maxDockerOutput {
		return nil, errors.New("read private source within bound")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || before.ModTime() != after.ModTime() {
		return nil, errors.New("private source changed while being read")
	}
	return content, nil
}

func extractTelemetry(artifactDir string, archive []byte, secrets ...[]byte) ([]string, error) {
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
		path := filepath.Join(artifactDir, "telemetry", base)
		if err := writeArtifact(path, redactSecrets(content, secrets...)); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func allowGatewayLogs(content []byte, secrets ...[]byte) []byte {
	content = redactSecrets(content, secrets...)
	lines := bytes.Split(content, []byte("\n"))
	var output bytes.Buffer
	for _, line := range lines {
		lower := bytes.ToLower(line)
		if len(line) > 4096 || bytes.ContainsRune(line, 0) || bytes.Contains(lower, []byte("authorization:")) || bytes.Contains(lower, []byte("bearer ")) {
			continue
		}
		output.Write(line)
		output.WriteByte('\n')
		if output.Len() >= maxDockerOutput {
			break
		}
	}
	return output.Bytes()
}

func writeArtifact(path string, content []byte) error {
	if err := ensurePrivateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		existing, readErr := readStablePrivateFile(path, 0o600)
		if readErr == nil && bytes.Equal(existing, content) {
			clear(existing)
			return nil
		}
		clear(existing)
		return fmt.Errorf("artifact %q already exists with different content", path)
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func appendUnique(paths []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(paths)+len(additions))
	for _, path := range paths {
		seen[path] = struct{}{}
	}
	for _, path := range additions {
		if _, ok := seen[path]; ok {
			continue
		}
		paths = append(paths, path)
		seen[path] = struct{}{}
	}
	return paths
}

func telemetryRelativePaths(artifactDir string, paths []string) []string {
	prefix := filepath.Join(artifactDir, "telemetry") + string(filepath.Separator)
	var relative []string
	for _, path := range paths {
		if strings.HasPrefix(path, prefix) {
			name, err := filepath.Rel(artifactDir, path)
			if err == nil {
				relative = append(relative, filepath.ToSlash(name))
			}
		}
	}
	return relative
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if resolved != absolute {
		return errors.New("private directory path contains a symbolic link")
	}
	return os.Chmod(path, 0o700)
}

func validateAPIKey(value []byte) error {
	if len(value) == 0 || len(value) > maxAPIKeyBytes {
		return errors.New("OpenClaw API key is empty or exceeds its bound")
	}
	if bytes.ContainsAny(value, "\x00\r\n") {
		return errors.New("OpenClaw API key contains NUL or a line break")
	}
	return nil
}

func environmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	return []byte(value), ok
}

func validateRunID(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("OpenClaw run ID must contain 1 to 128 safe characters")
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return errors.New("OpenClaw run ID contains an unsafe character")
	}
	return nil
}

func validateTaskID(value string) error {
	if value == "" || len(value) > 149 {
		return errors.New("OpenClaw task ID is invalid")
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return errors.New("OpenClaw task ID is invalid")
	}
	return nil
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

func randomID() (string, error) {
	var content [8]byte
	if _, err := rand.Read(content[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(content[:]), nil
}

func randomSecret(size int) ([]byte, error) {
	content := make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		clear(content)
		return nil, err
	}
	encoded := make([]byte, hex.EncodedLen(len(content)))
	hex.Encode(encoded, content)
	clear(content)
	return encoded, nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func clearSessionSecrets(active *session) {
	clear(active.apiKey)
	active.apiKey = nil
	clear(active.realtimeAPIKey)
	active.realtimeAPIKey = nil
	clear(active.gatewayToken)
	active.gatewayToken = nil
	active.agentIdempotency = ""
}

func redactSession(content []byte, active *session) []byte {
	return redactSecrets(content, active.apiKey, active.realtimeAPIKey, active.gatewayToken)
}
