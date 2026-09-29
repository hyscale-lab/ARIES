// Package codex runs the pinned upstream Codex CLI in a private container.
// Task tools use the bridge's native remote exec-server environment over SSH.
package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const maxOutputBytes = 16 << 20

type Options struct {
	Image        string
	CodexPath    string
	CodexVersion string
	OutputDir    string
	DockerSocket string
	// ReasoningEffort is omitted when empty so the model keeps its default.
	ReasoningEffort       string
	DeveloperInstructions string
	SubagentsEnabled      bool
	// Zero retains Codex's own concurrency limit. Ignored when disabled.
	MaxConcurrentSubagents int
	// APIKeyLookup transfers ownership of its returned buffer to the harness,
	// which clones the key and clears that buffer before returning from Start.
	APIKeyLookup   func(string) ([]byte, bool)
	Logger         *logrus.Logger
	CleanupTimeout time.Duration
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
}

type dockerClient interface {
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	CopyToContainer(context.Context, string, client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Manager struct {
	client                                     dockerClient
	image, codexSource, outputDir              string
	cleanupTimeout, startTimeout, agentTimeout time.Duration
	apiKeyLookup                               func(string) ([]byte, bool)
	logger                                     *logrus.Logger
	reasoningEffort, developerInstructions     string
	subagentsEnabled                           bool
	maxConcurrentSubagents                     int
	mu                                         sync.Mutex
	active                                     *session
	stopping                                   bool
	stopDone                                   chan struct{}
	stopErr                                    error
	closeOnce                                  sync.Once
	closeErr                                   error
}

type session struct {
	runID, taskID, attemptID, containerName, containerID, artifactDir string
	endpoint                                                          core.ToolEndpoint
	model                                                             core.ModelConfig
	agentTimeout                                                      time.Duration
	apiKey                                                            []byte
	creationAttempted, runAttempted                                   bool
	logPaths                                                          []string
}

var _ runner.AgentHarness = (*Manager)(nil)

// New validates host-local options without contacting Docker.
func New(options Options) (*Manager, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("Codex image: %w", err)
	}
	if options.CodexVersion != supportedVersion {
		return nil, fmt.Errorf("Codex native SSH integration requires version %s", supportedVersion)
	}
	if err := validateNativeSettings(options.ReasoningEffort, options.DeveloperInstructions, options.MaxConcurrentSubagents); err != nil {
		return nil, err
	}
	if strings.TrimSpace(options.CodexPath) == "" || strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Codex executable and output directory are required")
	}
	source, err := filepath.Abs(options.CodexPath)
	if err != nil {
		return nil, err
	}
	output, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(output); err != nil {
		return nil, fmt.Errorf("prepare Codex output: %w", err)
	}
	if options.DockerSocket == "" {
		options.DockerSocket = "/var/run/docker.sock"
	}
	host := options.DockerSocket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-codex/1"))
	if err != nil {
		return nil, err
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = func(name string) ([]byte, bool) { value, ok := os.LookupEnv(name); return []byte(value), ok }
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = 30 * time.Second
	}
	if options.StartTimeout <= 0 {
		options.StartTimeout = 45 * time.Second
	}
	if options.AgentTimeout <= 0 {
		options.AgentTimeout = 20 * time.Minute
	}
	return &Manager{
		client: api, image: options.Image, codexSource: source, outputDir: output,
		cleanupTimeout: options.CleanupTimeout, startTimeout: options.StartTimeout, agentTimeout: options.AgentTimeout,
		apiKeyLookup: options.APIKeyLookup, logger: options.Logger,
		reasoningEffort: options.ReasoningEffort, developerInstructions: options.DeveloperInstructions,
		subagentsEnabled: options.SubagentsEnabled, maxConcurrentSubagents: options.MaxConcurrentSubagents,
	}, nil
}

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

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return errors.New("Codex harness is already active")
	}
	if !safeIdentifier(request.RunID, 128) || !safeIdentifier(request.TaskID, 149) {
		return errors.New("Codex run and task IDs must be safe nonempty identifiers")
	}
	if request.Timeout < 0 {
		return errors.New("Codex task timeout must not be negative")
	}
	resources, err := harnessResources(request)
	if err != nil {
		return err
	}
	configuration, err := renderConfig(request.Model, manager.reasoningEffort, manager.developerInstructions, manager.subagentsEnabled, manager.maxConcurrentSubagents)
	if err != nil {
		return err
	}
	environments, err := renderEnvironments(request.Endpoint)
	if err != nil {
		return err
	}
	sourceKey, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	key := bytes.Clone(sourceKey)
	clear(sourceKey)
	if !ok || len(key) == 0 || len(key) > 16<<10 || bytes.ContainsAny(key, "\x00\r\n") {
		clear(key)
		return errors.New("Codex model key is missing, oversized, or contains NUL or line breaks")
	}
	id, err := randomID()
	if err != nil {
		clear(key)
		return err
	}
	active := &session{runID: request.RunID, taskID: request.TaskID, attemptID: id, containerName: "aries-codex-" + id, artifactDir: filepath.Join(manager.outputDir, request.TaskID, "harness"), endpoint: request.Endpoint, model: request.Model, agentTimeout: request.Timeout, apiKey: key}
	if active.agentTimeout == 0 {
		active.agentTimeout = manager.agentTimeout
	}
	fail := func(primary error) error {
		primary = redactError(primary, active.apiKey)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout)
		cleanupErr := manager.stopSession(cleanupCtx, active)
		cancel()
		if cleanupErr != nil {
			manager.active = active
			manager.stopErr = cleanupErr
			return errors.Join(primary, fmt.Errorf("rollback partial Codex harness: %w", cleanupErr))
		}
		return primary
	}
	containerConfig := &container.Config{Image: manager.image, Env: []string{"HOME=" + codexHome, "CODEX_HOME=" + codexHome, "CODEX_ROLLOUT_TRACE_ROOT=" + rolloutTraceContainerPath, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "exec sleep infinity"}, Labels: map[string]string{"aries.managed": "true", "aries.kind": "codex-harness", "aries.component": "harness", "aries.run": active.runID, "aries.task": active.taskID, "aries.attempt": id}}
	metadata, _ := json.Marshal(containerConfig)
	// Check instructions before TOML escaping as well: quoting a credential
	// containing a backslash or quote must not bypass the artifact boundary.
	if bytes.Contains(metadata, key) || bytes.Contains(configuration, key) || bytes.Contains(environments, key) || strings.Contains(manager.developerInstructions, string(key)) {
		return fail(errors.New("Codex credential overlaps configuration or Docker metadata"))
	}
	for _, artifact := range []struct {
		name string
		data []byte
	}{{"config.toml", configuration}, {"environments.toml", environments}} {
		filename := filepath.Join(active.artifactDir, artifact.name)
		if err := writeArtifact(filename, artifact.data); err != nil {
			return fail(fmt.Errorf("retain Codex configuration: %w", err))
		}
		active.logPaths = append(active.logPaths, filename)
	}
	archive, err := manager.runtimeArchive(active, configuration, environments)
	if err != nil {
		return fail(err)
	}
	defer clear(archive)
	startCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
	defer cancel()
	active.creationAttempted = true
	created, err := manager.client.ContainerCreate(startCtx, client.ContainerCreateOptions{Name: active.containerName, Config: containerConfig, HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(active.endpoint.Network), Resources: resources, SecurityOpt: []string{"no-new-privileges=true"}, CapDrop: []string{"ALL"}}})
	active.containerID = created.ID
	if err != nil {
		return fail(fmt.Errorf("create Codex container: %w", err))
	}
	if active.containerID == "" {
		return fail(errors.New("Docker returned an empty Codex container ID"))
	}
	inspection, err := manager.client.ContainerInspect(startCtx, active.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fail(err)
	}
	if err := manager.validateContainer(inspection.Container, active); err != nil {
		return fail(err)
	}
	if _, err := manager.client.CopyToContainer(startCtx, active.containerID, client.CopyToContainerOptions{DestinationPath: "/", Content: bytes.NewReader(archive), CopyUIDGID: true}); err != nil {
		return fail(fmt.Errorf("stage Codex runtime: %w", err))
	}
	if _, err := manager.client.ContainerStart(startCtx, active.containerID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("start Codex container: %w", err))
	}
	version, err := manager.execAttached(startCtx, active.containerID, []string{codexPath, "--version"}, "/")
	if err != nil {
		return fail(fmt.Errorf("check staged Codex version: %w", err))
	}
	if version.exitCode != 0 || strings.TrimSpace(string(version.stdout)) != "codex-cli "+supportedVersion {
		return fail(errors.New("staged Codex does not report the pinned version"))
	}
	manager.active, manager.stopErr = active, nil
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.taskID, "container": active.containerName}).Info("Codex harness started")
	return nil
}

func (manager *Manager) Run(ctx context.Context, instruction string) (core.HarnessResult, error) {
	started := time.Now()
	manager.mu.Lock()
	if manager.active == nil || manager.stopping {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Codex harness is not started")
	}
	if manager.active.runAttempted {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Codex harness accepts exactly one instruction")
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Codex task instruction is invalid")
	}
	if bytes.Contains([]byte(instruction), manager.active.apiKey) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Codex task instruction contains the model credential")
	}
	manager.active.runAttempted = true
	active := *manager.active
	active.apiKey = bytes.Clone(active.apiKey)
	active.logPaths = slices.Clone(active.logPaths)
	manager.mu.Unlock()
	defer clear(active.apiKey)
	recorder, err := newEventRecorder(active.artifactDir, active.apiKey)
	if err != nil {
		return core.HarnessResult{Status: core.StatusFailed, Error: err.Error(), LogPaths: active.logPaths}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, active.agentTimeout)
	output, runErr := manager.execAttachedObserved(runCtx, active.containerID, []string{agentWrapperPath, "exec", "--json", "--ephemeral", "--skip-git-repo-check", "--ignore-rules", "--color", "never", "--cd", active.endpoint.Workdir, "--", instruction}, active.endpoint.Workdir, recorder)
	cancel()
	runErr = errors.Join(runErr, recorder.finish())
	telemetryPaths := []string{recorder.path}
	// Collection has a fresh bounded context so cancellation retains partial evidence.
	traceCtx, traceCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	tracePaths, traceErr := manager.collectInferenceTrace(traceCtx, &active, runErr != nil)
	traceCancel()
	telemetryPaths = append(telemetryPaths, tracePaths...)
	active.logPaths = append(active.logPaths, telemetryPaths...)
	runErr = errors.Join(runErr, traceErr)
	stdout, stderr := redact(output.stdout, active.apiKey), redact(output.stderr, active.apiKey)
	if runErr == nil && output.exitCode != 0 {
		runErr = fmt.Errorf("Codex exec exited with status %d", output.exitCode)
	}
	final, trajectoryErr := finalResponse(stdout)
	final = string(redact([]byte(final), active.apiKey))
	if runErr == nil {
		runErr = trajectoryErr
	}
	runErr = redactError(runErr, active.apiKey)
	paths, artifactErr := writeRunArtifacts(active.artifactDir, started, output.exitCode, runErr, final, stdout, stderr, telemetryPaths)
	active.logPaths = append(active.logPaths, paths...)
	runErr = errors.Join(runErr, artifactErr)
	result := core.HarnessResult{Status: core.StatusSucceeded, FinalResponse: final, Duration: time.Since(started), LogPaths: active.logPaths}
	if runErr != nil {
		runErr = redactError(runErr, active.apiKey)
		result.Status, result.Error = core.StatusFailed, runErr.Error()
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			result.Status = core.StatusCanceled
		}
	}
	return result, runErr
}

// A root invocation creates exactly one trace bundle; children share its writer.
// Copy only its event file: payload files repeat prompts and can grow quadratically.
func (manager *Manager) collectInferenceTrace(ctx context.Context, active *session, quiesce bool) ([]string, error) {
	// Resolve the bundle with a short bound before stopping a canceled CLI. Docker
	// can copy from stopped containers, so the potentially slower transfer happens
	// only after the harness has stopped producing inference and tool requests.
	locateCtx, locateCancel := context.WithTimeout(ctx, 2*time.Second)
	output, err := manager.execAttached(locateCtx, active.containerID, []string{"/usr/bin/find", rolloutTraceContainerPath, "-mindepth", "2", "-maxdepth", "2", "-type", "f", "-name", "trace.jsonl", "-print"}, "/")
	locateCancel()
	if quiesce {
		inspection, inspectErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil {
			return nil, fmt.Errorf("inspect Codex before trace snapshot: %w", inspectErr)
		}
		if !ownedContainer(inspection.Container, active) {
			return nil, errors.New("refuse to stop unowned Codex trace producer")
		}
		if inspection.Container.State == nil || inspection.Container.State.Running {
			_, killErr := manager.client.ContainerKill(ctx, active.containerID, client.ContainerKillOptions{Signal: "KILL"})
			inspection, inspectErr = manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
			if inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running {
				return nil, errors.Join(killErr, inspectErr, errors.New("Codex trace producer is not confirmed stopped"))
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("locate Codex inference trace: %w", err)
	}
	names := strings.Split(strings.TrimSpace(string(output.stdout)), "\n")
	if output.exitCode != 0 || len(names) != 1 || !strings.HasPrefix(names[0], rolloutTraceContainerPath+"/") || filepath.Base(names[0]) != "trace.jsonl" || filepath.Clean(names[0]) != names[0] || filepath.Dir(filepath.Dir(names[0])) != rolloutTraceContainerPath {
		return nil, errors.New("Codex did not produce exactly one native inference trace")
	}
	archive, err := manager.client.CopyFromContainer(ctx, active.containerID, client.CopyFromContainerOptions{SourcePath: names[0]})
	if err != nil {
		return nil, fmt.Errorf("copy Codex inference trace: %w", err)
	}
	defer archive.Content.Close()
	return collectNativeTrace(archive.Content, active.artifactDir, active.apiKey)
}

func (manager *Manager) Stop(ctx context.Context) error {
	manager.mu.Lock()
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
	if manager.active == nil {
		err := manager.stopErr
		manager.mu.Unlock()
		return err
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
	defer manager.mu.Unlock()
	manager.stopErr, manager.stopping = err, false
	if err == nil {
		manager.active = nil
	}
	close(done)
	return err
}

func (manager *Manager) stopSession(ctx context.Context, active *session) error {
	if !active.creationAttempted {
		clear(active.apiKey)
		return nil
	}
	identifier := active.containerID
	if identifier == "" {
		identifier = active.containerName
	}
	inspection, err := manager.client.ContainerInspect(ctx, identifier, client.ContainerInspectOptions{})
	if errdefs.IsNotFound(err) {
		active.containerID = ""
		clear(active.apiKey)
		return nil
	}
	if err != nil {
		return redactError(fmt.Errorf("inspect Codex ownership before stop: %w", err), active.apiKey)
	}
	if !ownedContainer(inspection.Container, active) {
		return errors.New("refuse to stop a Codex container with different ownership")
	}
	active.containerID = inspection.Container.ID
	var cleanupErr error
	if inspection.Container.State == nil || inspection.Container.State.Running {
		timeout := 5
		_, stopErr := manager.client.ContainerStop(ctx, active.containerID, client.ContainerStopOptions{Timeout: &timeout})
		if stopErr != nil && !errdefs.IsNotFound(stopErr) {
			_, killErr := manager.client.ContainerKill(ctx, active.containerID, client.ContainerKillOptions{Signal: "KILL"})
			cleanupErr = errors.Join(stopErr, killErr)
		}
	}
	_, removeErr := manager.client.ContainerRemove(ctx, active.containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	_, absentErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
	if !errdefs.IsNotFound(absentErr) {
		if absentErr == nil {
			absentErr = errors.New("Codex container remains after removal")
		}
		return redactError(errors.Join(cleanupErr, removeErr, absentErr), active.apiKey)
	}
	if warning := errors.Join(cleanupErr, removeErr); warning != nil {
		manager.logger.WithContext(ctx).WithField("task_id", active.taskID).WithError(redactError(warning, active.apiKey)).Warn("Codex cleanup recovered after lifecycle errors")
	}
	active.containerID = ""
	clear(active.apiKey)
	return nil
}

func ownedContainer(info container.InspectResponse, active *session) bool {
	if info.ID == "" || active.containerID != "" && info.ID != active.containerID || info.Config == nil {
		return false
	}
	labels := info.Config.Labels
	return labels["aries.managed"] == "true" && labels["aries.kind"] == "codex-harness" && labels["aries.component"] == "harness" && labels["aries.run"] == active.runID && labels["aries.task"] == active.taskID && labels["aries.attempt"] == active.attemptID
}

func (manager *Manager) validateContainer(info container.InspectResponse, active *session) error {
	if !ownedContainer(info, active) || info.HostConfig == nil {
		return errors.New("Codex container inspection does not establish ownership")
	}
	if info.Config.Image != manager.image || !slices.Equal(info.Config.Entrypoint, []string{"/bin/sh"}) || !slices.Equal(info.Config.Cmd, []string{"-c", "exec sleep infinity"}) {
		return errors.New("Codex image or startup differs from pinned configuration")
	}
	if string(info.HostConfig.NetworkMode) != active.endpoint.Network || len(info.HostConfig.Binds) != 0 || len(info.HostConfig.Mounts) != 0 || len(info.Mounts) != 0 || info.HostConfig.Privileged {
		return errors.New("Codex must use only the task network without mounts or privileges")
	}
	if !slices.Contains(info.HostConfig.CapDrop, "ALL") || len(info.HostConfig.CapAdd) != 0 {
		return errors.New("Codex container must confirm all capabilities are dropped without additions")
	}
	noNewPrivileges := false
	for _, option := range info.HostConfig.SecurityOpt {
		switch option {
		case "no-new-privileges", "no-new-privileges=true", "no-new-privileges:true":
			noNewPrivileges = true
		default:
			if strings.HasPrefix(option, "no-new-privileges") {
				return errors.New("Codex container has a disabled or invalid no-new-privileges option")
			}
		}
	}
	if !noNewPrivileges {
		return errors.New("Codex container must confirm no-new-privileges is enabled")
	}
	metadata, _ := json.Marshal(info.Config)
	if bytes.Contains(metadata, active.apiKey) {
		return errors.New("Codex credential entered Docker metadata")
	}
	return nil
}

func harnessResources(request core.HarnessRequest) (container.Resources, error) {
	var resources container.Resources
	if request.CPU != nil {
		value := *request.CPU
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) || value*1e9 >= math.Exp2(63) {
			return resources, errors.New("Codex CPU must be finite, positive, and convert below 2^63 NanoCPUs")
		}
		resources.NanoCPUs = int64(value * 1e9)
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt64>>20 {
			return resources, errors.New("Codex memory must be positive and fit int64 bytes")
		}
		resources.Memory = int64(*request.MemoryMB) << 20
	}
	return resources, nil
}

func safeIdentifier(value string, limit int) bool {
	if value == "" || len(value) > limit {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func randomID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func redact(content, key []byte) []byte {
	result := bytes.Clone(content)
	if len(key) == 0 {
		return result
	}
	result = bytes.ReplaceAll(result, key, []byte("[REDACTED]"))
	escaped, _ := json.Marshal(string(key))
	if len(escaped) > 2 {
		result = bytes.ReplaceAll(result, escaped[1:len(escaped)-1], []byte("[REDACTED]"))
	}
	return result
}

type redactedError struct {
	message string
	cause   error
}

func (err *redactedError) Error() string { return err.message }
func (err *redactedError) Unwrap() error { return err.cause }
func redactError(err error, key []byte) error {
	if err == nil {
		return nil
	}
	return &redactedError{string(redact([]byte(err.Error()), key)), err}
}
