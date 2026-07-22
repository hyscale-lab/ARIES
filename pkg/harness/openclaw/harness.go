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
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	PinnedImage           = "ghcr.io/openclaw/openclaw:2026.5.26@sha256:ae7ff536446f1bbb57ea51b9b21097d8f299d30d683dcd72644973bc0522f3b3"
	defaultCleanupTimeout = 30 * time.Second
	defaultStartTimeout   = 45 * time.Second
	defaultAgentTimeout   = 20 * time.Minute
	gracefulStopSeconds   = 5
	maxAPIKeyBytes        = 16 << 10
	managedLabel          = "aries.managed=true"
	milestoneLabel        = "aries.milestone=m5"
)

var (
	upstreamEntrypoint = []string{"tini", "-s", "--"}
	gatewayCommand     = []string{"node", "openclaw.mjs", "gateway", "--bind", "loopback", "--port", "18789"}
)

type Options struct {
	Image        string
	OutputDir    string
	DockerBinary string
	// APIKeyLookup returns a fresh secret buffer that Start clears after copying.
	APIKeyLookup   func(string) ([]byte, bool)
	CleanupTimeout time.Duration
	StartTimeout   time.Duration
	AgentTimeout   time.Duration
	Logger         *slog.Logger
}

type Manager struct {
	cli            commandRunner
	image          string
	outputDir      string
	cleanupTimeout time.Duration
	startTimeout   time.Duration
	agentTimeout   time.Duration
	logger         *slog.Logger
	apiKeyLookup   func(string) ([]byte, bool)
	newID          func() (string, error)

	mu       sync.Mutex
	active   *session
	stopping bool
	stopDone chan struct{}
	stopErr  error
}

type session struct {
	runID              string
	taskID             string
	safeTaskID         string
	attemptID          string
	containerName      string
	containerID        string
	containerTentative bool
	containerOwned     bool
	configVolume       string
	configTentative    bool
	configOwned        bool
	stateVolume        string
	stateTentative     bool
	stateOwned         bool
	initContainer      string
	initContainerID    string
	initTentative      bool
	initOwned          bool
	artifactDir        string
	endpoint           core.ToolEndpoint
	model              core.ModelConfig
	apiKey             []byte
	gatewayToken       []byte
	runAttempted       bool
	runResultDir       string
	logPaths           []string
}

var _ runner.AgentHarness = (*Manager)(nil)

func New(options Options) (*Manager, error) {
	if options.Image == "" {
		options.Image = PinnedImage
	}
	if options.Image != PinnedImage {
		return nil, fmt.Errorf("OpenClaw image must equal the pinned digest %q", PinnedImage)
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
	if options.DockerBinary == "" {
		options.DockerBinary = defaultDockerBinary
	}
	if strings.ContainsRune(options.DockerBinary, 0) {
		return nil, errors.New("OpenClaw Docker binary contains NUL")
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
		options.Logger = slog.Default()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = environmentAPIKeyLookup
	}
	return &Manager{
		cli: execRunner{binary: options.DockerBinary}, image: options.Image, outputDir: outputDir,
		cleanupTimeout: options.CleanupTimeout, startTimeout: options.StartTimeout, agentTimeout: options.AgentTimeout,
		logger: options.Logger, apiKeyLookup: options.APIKeyLookup, newID: randomID,
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
	configuration, err := renderConfig(request.Model, request.Endpoint)
	if err != nil {
		return err
	}
	clientBytes, err := readStablePrivateFile(request.Endpoint.ClientSourceFile, 0o555)
	if err != nil {
		return fmt.Errorf("validate OpenClaw SSH client source: %w", err)
	}
	clear(clientBytes)
	apiKeyValue, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	if !ok {
		clear(apiKeyValue)
		return fmt.Errorf("OpenClaw API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeyValue)
	clear(apiKeyValue)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return err
	}
	if bytes.Contains(configuration, apiKey) {
		clear(apiKey)
		return errors.New("rendered OpenClaw config contains the API-key value")
	}
	id, err := manager.newID()
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate OpenClaw harness ID: %w", err)
	}
	gatewayToken, err := randomSecret(32)
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate OpenClaw gateway token: %w", err)
	}
	active := &session{
		runID: request.RunID, taskID: request.TaskID, safeTaskID: safeTaskID(request.TaskID), attemptID: id,
		endpoint: request.Endpoint, model: request.Model, apiKey: apiKey, gatewayToken: gatewayToken,
		containerName: "aries-openclaw-" + id, configVolume: "aries-openclaw-config-" + id,
		stateVolume: "aries-openclaw-state-" + id, initContainer: "aries-openclaw-init-" + id,
		artifactDir: filepath.Join(manager.outputDir, "harnesses", id), runResultDir: stateContainerPath + "/.aries/run",
	}
	active.logPaths = []string{
		filepath.Join(active.artifactDir, "gateway.log"),
		filepath.Join(active.artifactDir, "telemetry.index.json"),
	}
	fail := func(primary error) error {
		cleanupErr := manager.rollbackStart(ctx, active)
		if cleanupErr != nil {
			manager.active = active
			manager.stopErr = cleanupErr
			return errors.Join(primary, fmt.Errorf("rollback partial OpenClaw harness: %w", cleanupErr))
		}
		return primary
	}
	if err := ensurePrivateDirectory(active.artifactDir); err != nil {
		return fail(fmt.Errorf("create OpenClaw artifact directory: %w", err))
	}
	if err := manager.createVolume(ctx, active, active.configVolume, "config", &active.configTentative, &active.configOwned); err != nil {
		return fail(err)
	}
	if err := manager.createVolume(ctx, active, active.stateVolume, "state", &active.stateTentative, &active.stateOwned); err != nil {
		return fail(err)
	}
	if err := manager.initializeVolumes(ctx, active, configuration); err != nil {
		return fail(err)
	}
	if err := manager.createHarnessContainer(ctx, active); err != nil {
		return fail(err)
	}
	if _, err := runDockerChecked(ctx, manager.cli, active.apiKey, nil, "container", "start", active.containerID); err != nil {
		return fail(fmt.Errorf("start OpenClaw gateway container: %w", err))
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
	defer cancel()
	if err := manager.waitReady(readyCtx, active); err != nil {
		return fail(err)
	}
	manager.active = active
	manager.stopErr = nil
	manager.logger.InfoContext(ctx, "OpenClaw harness started", "task_id", active.taskID, "container", active.containerName)
	return nil
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
	if strings.ContainsRune(instruction, 0) || strings.TrimSpace(instruction) == "" {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("OpenClaw task instruction is invalid")
	}
	active.runAttempted = true
	manager.mu.Unlock()

	timeoutSeconds := max(1, int(manager.agentTimeout/time.Second))
	agentCommand := buildAgentCommand(active, instruction, timeoutSeconds)
	if _, err := runDockerChecked(ctx, manager.cli, active.apiKey, nil, append([]string{"container", "exec", "--detach", active.containerID}, agentCommand...)...); err != nil {
		return manager.failedResult(active, started, nil, nil, err)
	}
	status, stdout, stderr, err := manager.waitAgent(ctx, active)
	if err != nil {
		if ctx.Err() != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout)
			_ = manager.terminateHarness(cleanupCtx, active)
			cancel()
		}
		return manager.failedResult(active, started, stdout, stderr, err)
	}
	stdout = redactSession(stdout, active)
	stderr = redactSession(stderr, active)
	logPaths, writeErr := manager.writeRunArtifacts(active, stdout, stderr)
	active.logPaths = appendUnique(active.logPaths, logPaths...)
	if writeErr != nil {
		return manager.failedResult(active, started, stdout, stderr, writeErr)
	}
	if status != 0 {
		return core.HarnessResult{Status: core.StatusFailed, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...), Error: fmt.Sprintf("OpenClaw agent exited %d", status)}, fmt.Errorf("OpenClaw agent exited %d", status)
	}
	response, err := parseAgentResult(stdout, stderr)
	if err != nil {
		return core.HarnessResult{Status: core.StatusFailed, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...), Error: err.Error()}, err
	}
	return core.HarnessResult{Status: core.StatusSucceeded, FinalResponse: response, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...)}, nil
}

func buildAgentCommand(active *session, instruction string, timeoutSeconds int) []string {
	command := []string{
		launcherPath, agentWrapperPath, active.runResultDir,
		"node", "openclaw.mjs", "agent",
		"--session-key", "agent:main:aries-" + active.safeTaskID,
		"--message", instruction,
		"--json", "--timeout", strconv.Itoa(timeoutSeconds),
	}
	if disablesThinking(active.model) {
		command = append(command, "--thinking", "off")
	}
	return command
}

func disablesThinking(model core.ModelConfig) bool {
	if model.BaseURL != "https://api.deepseek.com" {
		return false
	}
	return model.Model == "deepseek-v4-flash" || model.Model == "deepseek-v4-pro"
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
	err := manager.stopSession(cleanupCtx, active, true)
	cancel()

	manager.mu.Lock()
	manager.stopErr = err
	manager.stopping = false
	if err == nil {
		manager.active = nil
	}
	close(done)
	manager.mu.Unlock()
	return err
}

type ownershipState uint8

const (
	ownershipUnknown ownershipState = iota
	ownershipAbsent
	ownershipOwned
	ownershipForeign
)

func (manager *Manager) createVolume(ctx context.Context, active *session, name, kind string, tentative, owned *bool) error {
	args := []string{
		"volume", "create", "--label", managedLabel, "--label", milestoneLabel,
		"--label", "aries.kind=openclaw-" + kind, "--label", "aries.task=" + active.safeTaskID,
		"--label", "aries.run=" + active.runID, "--label", "aries.attempt=" + active.attemptID, name,
	}
	*tentative = true
	result, commandErr := manager.cli.Run(ctx, nil, args...)
	_, createErr := checkedDockerResult(ctx, result, commandErr, active.apiKey, args...)
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	defer cancel()
	state, proofErr := manager.awaitVolumeOwnership(proofCtx, active, name, kind)
	manager.applyVolumeOwnership(state, tentative, owned)
	if createErr != nil {
		return fmt.Errorf("create OpenClaw %s volume: %w", kind, errors.Join(createErr, proofErr))
	}
	if proofErr != nil {
		return fmt.Errorf("prove ownership of OpenClaw %s volume: %w", kind, proofErr)
	}
	if state == ownershipAbsent {
		return fmt.Errorf("prove ownership of OpenClaw %s volume: volume is absent after create", kind)
	}
	return nil
}

func (manager *Manager) awaitVolumeOwnership(ctx context.Context, active *session, name, kind string) (ownershipState, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	lastState := ownershipUnknown
	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastState == ownershipAbsent {
				return ownershipAbsent, nil
			}
			return ownershipUnknown, errors.Join(lastErr, ctx.Err())
		}
		inspection, exists, err := inspectVolume(ctx, manager.cli, active.apiKey, name)
		if err == nil && !exists {
			lastState = ownershipAbsent
			lastErr = nil
		} else if err == nil {
			if err := validateVolumeOwnership(inspection, active, name, kind); err != nil {
				return ownershipForeign, err
			}
			return ownershipOwned, nil
		} else {
			lastState = ownershipUnknown
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastState == ownershipAbsent {
				return ownershipAbsent, nil
			}
			return ownershipUnknown, errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (*Manager) applyVolumeOwnership(state ownershipState, tentative, owned *bool) {
	switch state {
	case ownershipOwned:
		*tentative = false
		*owned = true
	case ownershipAbsent, ownershipForeign:
		*tentative = false
		*owned = false
	}
}

func validateVolumeOwnership(inspection volumeInspection, active *session, name, kind string) error {
	if inspection.Name != name || inspection.Labels["aries.managed"] != "true" ||
		inspection.Labels["aries.milestone"] != "m5" || inspection.Labels["aries.kind"] != "openclaw-"+kind ||
		inspection.Labels["aries.task"] != active.safeTaskID || inspection.Labels["aries.run"] != active.runID || inspection.Labels["aries.attempt"] != active.attemptID {
		return fmt.Errorf("volume %q is not owned by OpenClaw attempt %q", name, active.attemptID)
	}
	return nil
}

func (manager *Manager) createOwnedContainer(ctx context.Context, active *session, name, kind string, args []string, id *string, tentative, owned *bool) error {
	*tentative = true
	result, commandErr := manager.cli.Run(ctx, nil, args...)
	_, createErr := checkedDockerResult(ctx, result, commandErr, active.apiKey, args...)
	createdID := strings.TrimSpace(string(result.stdout))
	if createdID != "" {
		*id = createdID
	}
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	defer cancel()
	state, inspection, proofErr := manager.awaitContainerOwnership(proofCtx, active, name, kind)
	manager.applyContainerOwnership(state, inspection, id, tentative, owned)
	if state == ownershipOwned && createdID != "" && createdID != inspection.ID {
		proofErr = errors.Join(proofErr, fmt.Errorf("Docker create returned container ID %q, inspected %q", createdID, inspection.ID))
	}
	if createErr != nil {
		return errors.Join(createErr, proofErr)
	}
	if proofErr != nil {
		return proofErr
	}
	if state == ownershipAbsent {
		return errors.New("container is absent after create")
	}
	return nil
}

func (manager *Manager) awaitContainerOwnership(ctx context.Context, active *session, name, kind string) (ownershipState, containerInspection, error) {
	return manager.awaitContainerOwnershipReferences(ctx, active, []string{name}, kind)
}

func (manager *Manager) awaitContainerOwnershipReferences(ctx context.Context, active *session, references []string, kind string) (ownershipState, containerInspection, error) {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	lastState := ownershipUnknown
	var lastErr error
	for {
		if ctx.Err() != nil {
			if lastState == ownershipAbsent {
				return ownershipAbsent, containerInspection{}, nil
			}
			return ownershipUnknown, containerInspection{}, errors.Join(lastErr, ctx.Err())
		}
		state, inspection, err := manager.inspectContainerOwnershipReferences(ctx, active, references, kind)
		if state == ownershipOwned || state == ownershipForeign {
			return state, inspection, err
		}
		lastState = state
		lastErr = err
		select {
		case <-ctx.Done():
			if lastState == ownershipAbsent {
				return ownershipAbsent, containerInspection{}, nil
			}
			return ownershipUnknown, containerInspection{}, errors.Join(lastErr, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) inspectContainerOwnershipReferences(ctx context.Context, active *session, references []string, kind string) (ownershipState, containerInspection, error) {
	var errs []error
	for _, reference := range references {
		inspection, exists, err := inspectContainerMaybe(ctx, manager.cli, active.apiKey, reference)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect container %q: %w", reference, err))
			continue
		}
		if !exists {
			continue
		}
		if err := validateContainerOwnership(inspection, active, kind); err != nil {
			return ownershipForeign, inspection, err
		}
		return ownershipOwned, inspection, nil
	}
	if len(errs) != 0 {
		return ownershipUnknown, containerInspection{}, errors.Join(errs...)
	}
	return ownershipAbsent, containerInspection{}, nil
}

func (*Manager) applyContainerOwnership(state ownershipState, inspection containerInspection, id *string, tentative, owned *bool) {
	switch state {
	case ownershipOwned:
		*id = inspection.ID
		*tentative = false
		*owned = true
	case ownershipAbsent, ownershipForeign:
		*id = ""
		*tentative = false
		*owned = false
	}
}

func validateContainerOwnership(inspection containerInspection, active *session, kind string) error {
	if inspection.ID == "" || inspection.Config.Labels["aries.managed"] != "true" ||
		inspection.Config.Labels["aries.milestone"] != "m5" || inspection.Config.Labels["aries.kind"] != kind ||
		inspection.Config.Labels["aries.task"] != active.safeTaskID || inspection.Config.Labels["aries.run"] != active.runID || inspection.Config.Labels["aries.attempt"] != active.attemptID {
		return fmt.Errorf("container is not owned by OpenClaw attempt %q as %q", active.attemptID, kind)
	}
	return nil
}

func (manager *Manager) proveContainerOwnership(ctx context.Context, active *session, id, kind string) (bool, error) {
	if id == "" {
		return false, errors.New("owned OpenClaw container has no retained ID")
	}
	inspection, exists, err := inspectContainerMaybe(ctx, manager.cli, active.apiKey, id)
	if err != nil || !exists {
		return exists, err
	}
	if inspection.ID != id {
		return true, fmt.Errorf("OpenClaw container identity changed from %q to %q", id, inspection.ID)
	}
	if err := validateContainerOwnership(inspection, active, kind); err != nil {
		return true, err
	}
	return true, nil
}

func (manager *Manager) removeOwnedContainer(ctx context.Context, active *session, id, kind string) error {
	exists, err := manager.proveContainerOwnership(ctx, active, id, kind)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	result, removeErr := manager.cli.Run(ctx, nil, "container", "rm", "--force", id)
	if removeErr != nil && !resourceMissing(result.stderr, "container") {
		return fmt.Errorf("remove owned OpenClaw container: %s", strings.TrimSpace(string(redactSession(result.stderr, active))))
	}
	absent, err := containerAbsent(ctx, manager.cli, active.apiKey, id)
	if err != nil {
		return err
	}
	if !absent {
		return errors.New("owned OpenClaw container remains after removal")
	}
	return nil
}

func (manager *Manager) initializeVolumes(ctx context.Context, active *session, configuration []byte) error {
	initializer := `set -eu
mkdir -p /run/aries/ssh /home/node/.openclaw/.aries
cp /tmp/aries-stage/openclaw.json /run/aries/openclaw.json
cp /tmp/aries-stage/model.key /run/aries/model.key
cp /tmp/aries-stage/gateway.key /run/aries/gateway.key
cp /tmp/aries-stage/launch /run/aries/launch
cp /tmp/aries-stage/run-agent /run/aries/run-agent
cp /tmp/aries-stage/id_ed25519 /run/aries/ssh/id_ed25519
cp /tmp/aries-stage/known_hosts /run/aries/ssh/known_hosts
cp /tmp/aries-stage/ssh-config /run/aries/ssh/config
chown -R 1000:1000 /run/aries /home/node/.openclaw
chmod 0700 /run/aries /run/aries/ssh /home/node/.openclaw /home/node/.openclaw/.aries
chmod 0600 /run/aries/openclaw.json /run/aries/model.key /run/aries/gateway.key /run/aries/ssh/id_ed25519 /run/aries/ssh/known_hosts /run/aries/ssh/config
chmod 0555 /run/aries/launch /run/aries/run-agent
sync
printf ok >/tmp/aries-init-status.tmp
mv /tmp/aries-init-status.tmp /tmp/aries-init-status`
	args := []string{
		"container", "create", "--name", active.initContainer,
		"--label", managedLabel, "--label", milestoneLabel, "--label", "aries.kind=openclaw-initializer", "--label", "aries.task=" + active.safeTaskID,
		"--label", "aries.run=" + active.runID, "--label", "aries.attempt=" + active.attemptID,
		"--network", "none", "--user", "0:0",
		"--mount", "type=volume,src=" + active.configVolume + ",dst=/run/aries",
		"--mount", "type=volume,src=" + active.stateVolume + ",dst=/home/node/.openclaw",
		"--entrypoint", "/bin/sh", manager.image, "-c", initializer,
	}
	if err := manager.createOwnedContainer(ctx, active, active.initContainer, "openclaw-initializer", args, &active.initContainerID, &active.initTentative, &active.initOwned); err != nil {
		return fmt.Errorf("create OpenClaw volume initializer: %w", err)
	}
	identity, err := readStablePrivateFile(active.endpoint.IdentitySourceFile, 0o600)
	if err != nil {
		return fmt.Errorf("read OpenClaw SSH identity source: %w", err)
	}
	defer clear(identity)
	knownHosts, err := readStablePrivateFile(active.endpoint.KnownHostsSourceFile, 0o600)
	if err != nil {
		return fmt.Errorf("read OpenClaw known-hosts source: %w", err)
	}
	sshConfiguration := []byte("Host openclaw-sandbox\n  HostName task-sandbox\n  Port 2222\n  BatchMode yes\n  ConnectTimeout 5\n  ServerAliveInterval 15\n  ServerAliveCountMax 3\n  StrictHostKeyChecking yes\n  UpdateHostKeys no\n  User aries\n  UserKnownHostsFile /run/aries/ssh/known_hosts\n  IdentityFile /run/aries/ssh/id_ed25519\n  IdentitiesOnly yes\n")
	archive, err := stageArchive(map[string]stagedFile{
		"openclaw.json": {content: configuration, mode: 0o600},
		"model.key":     {content: active.apiKey, mode: 0o600},
		"gateway.key":   {content: active.gatewayToken, mode: 0o600},
		"launch":        {content: launcherScript(active.model.APIKeyEnv), mode: 0o555},
		"run-agent":     {content: agentWrapperScript(), mode: 0o555},
		"id_ed25519":    {content: identity, mode: 0o600},
		"known_hosts":   {content: knownHosts, mode: 0o600},
		"ssh-config":    {content: sshConfiguration, mode: 0o600},
	})
	if err != nil {
		return err
	}
	defer clear(archive)
	if _, err := runDockerChecked(ctx, manager.cli, active.apiKey, archive, "container", "cp", "-", active.initContainerID+":/tmp"); err != nil {
		return fmt.Errorf("copy private OpenClaw runtime material into initializer: %w", err)
	}
	if _, err := runDockerChecked(ctx, manager.cli, active.apiKey, nil, "container", "start", active.initContainerID); err != nil {
		return fmt.Errorf("start OpenClaw volume initializer: %w", err)
	}
	if err := manager.waitForExactFile(ctx, active, active.initContainerID, "/tmp/aries-init-status", []byte("ok")); err != nil {
		return fmt.Errorf("wait for OpenClaw volume initializer: %w", err)
	}
	if err := manager.removeOwnedContainer(ctx, active, active.initContainerID, "openclaw-initializer"); err != nil {
		return fmt.Errorf("remove OpenClaw volume initializer: %w", err)
	}
	active.initOwned = false
	active.initTentative = false
	active.initContainerID = ""
	return nil
}

func (manager *Manager) createHarnessContainer(ctx context.Context, active *session) error {
	args := []string{
		"container", "create", "--name", active.containerName,
		"--label", managedLabel, "--label", milestoneLabel, "--label", "aries.kind=openclaw-harness", "--label", "aries.task=" + active.safeTaskID,
		"--label", "aries.run=" + active.runID, "--label", "aries.attempt=" + active.attemptID,
		"--network", active.endpoint.Network,
		"--env", "OPENCLAW_CONFIG_PATH=" + configContainerPath,
		"--mount", "type=volume,src=" + active.configVolume + ",dst=/run/aries,readonly",
		"--mount", "type=volume,src=" + active.stateVolume + ",dst=" + stateContainerPath,
		"--mount", "type=bind,src=" + active.endpoint.ClientSourceFile + ",dst=" + active.endpoint.ClientCommand + ",readonly",
		manager.image, launcherPath,
	}
	args = append(args, gatewayCommand...)
	if err := manager.createOwnedContainer(ctx, active, active.containerName, "openclaw-harness", args, &active.containerID, &active.containerTentative, &active.containerOwned); err != nil {
		return fmt.Errorf("create OpenClaw gateway container: %w", err)
	}
	inspection, err := inspectContainer(ctx, manager.cli, active.apiKey, active.containerID)
	if err != nil {
		return err
	}
	if err := validateContainerInspection(inspection, active); err != nil {
		return err
	}
	return nil
}

func validateContainerInspection(inspection containerInspection, active *session) error {
	if inspection.Config.Image != PinnedImage || inspection.Config.User != "node" {
		return fmt.Errorf("OpenClaw container image/user = %q/%q, want pinned image/default node", inspection.Config.Image, inspection.Config.User)
	}
	wantedCommand := append([]string{launcherPath}, gatewayCommand...)
	if !equalStrings(inspection.Config.Entrypoint, upstreamEntrypoint) || !equalStrings(inspection.Config.Cmd, wantedCommand) {
		return errors.New("OpenClaw container entrypoint or gateway argv differs from the pinned direct command")
	}
	for _, environment := range inspection.Config.Env {
		if strings.HasPrefix(environment, active.model.APIKeyEnv+"=") || strings.HasPrefix(environment, gatewayTokenEnv+"=") ||
			strings.Contains(environment, string(active.apiKey)) || strings.Contains(environment, string(active.gatewayToken)) {
			return errors.New("OpenClaw secret entered Docker Config.Env")
		}
	}
	if inspection.ID != active.containerID || inspection.Config.Labels["aries.managed"] != "true" || inspection.Config.Labels["aries.milestone"] != "m5" || inspection.Config.Labels["aries.task"] != active.safeTaskID || inspection.Config.Labels["aries.run"] != active.runID || inspection.Config.Labels["aries.attempt"] != active.attemptID {
		return errors.New("OpenClaw container labels do not match the task")
	}
	if len(inspection.NetworkSettings.Networks) != 1 {
		return errors.New("OpenClaw container is not attached to exactly one task network")
	}
	if _, ok := inspection.NetworkSettings.Networks[active.endpoint.Network]; !ok {
		return errors.New("OpenClaw container is not attached to the bridge network")
	}
	wanted := map[string]bool{"/run/aries": false, stateContainerPath: true, active.endpoint.ClientCommand: false}
	for _, mount := range inspection.Mounts {
		writable, ok := wanted[mount.Destination]
		if !ok || mount.RW != writable || mount.Destination == "/var/run/docker.sock" {
			return fmt.Errorf("unexpected OpenClaw mount %q", mount.Destination)
		}
		delete(wanted, mount.Destination)
	}
	if len(wanted) != 0 {
		return errors.New("OpenClaw container is missing a required mount")
	}
	return nil
}

func (manager *Manager) waitReady(ctx context.Context, active *session) error {
	statusPath := stateContainerPath + "/.aries/ready"
	script := `const fs=require("fs"),http=require("http");const out=process.argv[1];const req=http.get({host:"127.0.0.1",port:18789,path:"/readyz",timeout:1500},res=>{let b="";res.setEncoding("utf8");res.on("data",c=>b+=c);res.on("end",()=>{try{const j=JSON.parse(b);if(res.statusCode===200&&j.ready===true&&process.getuid()===1000){const t=out+".tmp";fs.writeFileSync(t,JSON.stringify({status:res.statusCode,ready:j.ready,uid:process.getuid()}),{mode:0o600});fs.renameSync(t,out)}}catch{}})});req.on("timeout",()=>req.destroy());req.on("error",()=>{});`
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, _ = manager.cli.Run(ctx, nil, "container", "exec", "--detach", active.containerID, launcherPath, "node", "-e", script, statusPath)
		content, found, err := copyContainerFile(ctx, manager.cli, active.apiKey, active.containerID, statusPath)
		if err != nil {
			return err
		}
		if found {
			var ready struct {
				Status int  `json:"status"`
				Ready  bool `json:"ready"`
				UID    int  `json:"uid"`
			}
			if err := json.Unmarshal(content, &ready); err != nil || ready.Status != 200 || !ready.Ready || ready.UID != 1000 {
				return errors.New("OpenClaw readiness evidence is invalid")
			}
			processes, err := containerProcesses(ctx, manager.cli, active.apiKey, active.containerID)
			if err != nil {
				return err
			}
			if !containsExactProcess(processes, "openclaw") {
				return fmt.Errorf("OpenClaw gateway process is absent after readiness: %q", processes)
			}
			return nil
		}
		processes, err := containerProcesses(ctx, manager.cli, active.apiKey, active.containerID)
		if err != nil {
			return err
		}
		if len(processes) == 0 {
			return errors.New("OpenClaw gateway exited before readiness")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await exact OpenClaw readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) waitAgent(ctx context.Context, active *session) (int, []byte, []byte, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	statusPath := active.runResultDir + "/status"
	for {
		statusBytes, found, err := copyContainerFile(ctx, manager.cli, active.apiKey, active.containerID, statusPath)
		if err != nil {
			return -1, nil, nil, err
		}
		if found {
			status, err := strconv.Atoi(strings.TrimSpace(string(statusBytes)))
			if err != nil {
				return -1, nil, nil, errors.New("OpenClaw agent status file is invalid")
			}
			stdout, stdoutFound, stdoutErr := copyContainerFile(ctx, manager.cli, active.apiKey, active.containerID, active.runResultDir+"/stdout")
			stderr, stderrFound, stderrErr := copyContainerFile(ctx, manager.cli, active.apiKey, active.containerID, active.runResultDir+"/stderr")
			if err := errors.Join(stdoutErr, stderrErr); err != nil {
				return -1, nil, nil, err
			}
			if !stdoutFound || !stderrFound {
				return -1, nil, nil, errors.New("OpenClaw agent status appeared before complete output artifacts")
			}
			return status, stdout, stderr, nil
		}
		select {
		case <-ctx.Done():
			return -1, nil, nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager *Manager) waitForExactFile(ctx context.Context, active *session, container, path string, expected []byte) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		content, found, err := copyContainerFile(ctx, manager.cli, active.apiKey, container, path)
		if err != nil {
			return err
		}
		if found {
			if !bytes.Equal(content, expected) {
				return errors.New("detached OpenClaw status file has unexpected content")
			}
			return nil
		}
		processes, err := containerProcesses(ctx, manager.cli, active.apiKey, container)
		if err != nil {
			return err
		}
		if len(processes) == 0 {
			return errors.New("detached OpenClaw initializer exited without success status")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (manager *Manager) stopSession(ctx context.Context, active *session, collect bool) error {
	if active == nil {
		return nil
	}
	if err := manager.resolveTentativeResources(ctx, active); err != nil {
		return err
	}
	var errs []error
	if active.containerOwned {
		if err := manager.terminateHarness(ctx, active); err != nil {
			return err
		}
		artifactsCollected := true
		if collect {
			if err := manager.collectArtifacts(ctx, active); err != nil {
				errs = append(errs, err)
				artifactsCollected = false
			}
		}
		if !artifactsCollected {
			return errors.Join(errs...)
		}
		if err := manager.removeOwnedContainer(ctx, active, active.containerID, "openclaw-harness"); err != nil {
			errs = append(errs, fmt.Errorf("remove OpenClaw harness: %w", err))
		} else {
			active.containerOwned = false
			active.containerID = ""
		}
	}
	if active.initOwned {
		if err := manager.removeOwnedContainer(ctx, active, active.initContainerID, "openclaw-initializer"); err != nil {
			errs = append(errs, fmt.Errorf("remove OpenClaw initializer: %w", err))
		} else {
			active.initOwned = false
			active.initContainerID = ""
		}
	}
	if !active.containerOwned && !active.initOwned {
		volumes := []struct {
			name  string
			kind  string
			owned *bool
		}{
			{name: active.stateVolume, kind: "state", owned: &active.stateOwned},
			{name: active.configVolume, kind: "config", owned: &active.configOwned},
		}
		for _, volume := range volumes {
			if !*volume.owned {
				continue
			}
			inspection, exists, inspectErr := inspectVolume(ctx, manager.cli, active.apiKey, volume.name)
			if inspectErr != nil {
				errs = append(errs, inspectErr)
				continue
			}
			if !exists {
				*volume.owned = false
				continue
			}
			if err := validateVolumeOwnership(inspection, active, volume.name, volume.kind); err != nil {
				errs = append(errs, err)
				continue
			}
			removeResult, removeErr := manager.cli.Run(ctx, nil, "volume", "rm", volume.name)
			if removeErr != nil && !resourceMissing(removeResult.stderr, "volume") {
				errs = append(errs, fmt.Errorf("remove OpenClaw volume %q: %s", volume.name, strings.TrimSpace(string(redactSession(removeResult.stderr, active)))))
				continue
			}
			absent, err := volumeAbsent(ctx, manager.cli, active.apiKey, volume.name)
			if err != nil || !absent {
				if err == nil {
					err = errors.New("volume remains")
				}
				errs = append(errs, fmt.Errorf("confirm OpenClaw volume %q removal: %w", volume.name, err))
				continue
			}
			*volume.owned = false
		}
	}
	if len(errs) == 0 {
		clearSessionSecrets(active)
	}
	return errors.Join(errs...)
}

func (manager *Manager) resolveTentativeResources(ctx context.Context, active *session) error {
	var errs []error
	containers := []struct {
		name      string
		kind      string
		id        *string
		tentative *bool
		owned     *bool
	}{
		{name: active.containerName, kind: "openclaw-harness", id: &active.containerID, tentative: &active.containerTentative, owned: &active.containerOwned},
		{name: active.initContainer, kind: "openclaw-initializer", id: &active.initContainerID, tentative: &active.initTentative, owned: &active.initOwned},
	}
	for _, container := range containers {
		if !*container.tentative {
			continue
		}
		state, inspection, err := manager.awaitTentativeContainerOwnership(ctx, active, container.name, *container.id, container.kind)
		manager.applyContainerOwnership(state, inspection, container.id, container.tentative, container.owned)
		if state == ownershipUnknown {
			errs = append(errs, fmt.Errorf("resolve tentative %s container %q: %w", container.kind, container.name, err))
		}
	}
	volumes := []struct {
		name      string
		kind      string
		tentative *bool
		owned     *bool
	}{
		{name: active.stateVolume, kind: "state", tentative: &active.stateTentative, owned: &active.stateOwned},
		{name: active.configVolume, kind: "config", tentative: &active.configTentative, owned: &active.configOwned},
	}
	for _, volume := range volumes {
		if !*volume.tentative {
			continue
		}
		state, err := manager.awaitVolumeOwnership(ctx, active, volume.name, volume.kind)
		manager.applyVolumeOwnership(state, volume.tentative, volume.owned)
		if state == ownershipUnknown {
			errs = append(errs, fmt.Errorf("resolve tentative OpenClaw %s volume %q: %w", volume.kind, volume.name, err))
		}
	}
	return errors.Join(errs...)
}

func (manager *Manager) awaitTentativeContainerOwnership(ctx context.Context, active *session, name, retainedID, kind string) (ownershipState, containerInspection, error) {
	references := []string{name}
	if retainedID != "" {
		references = []string{retainedID, name}
	}
	return manager.awaitContainerOwnershipReferences(ctx, active, references, kind)
}

func (manager *Manager) rollbackStart(ctx context.Context, active *session) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		lastErr = manager.stopSession(cleanupCtx, active, false)
		if lastErr == nil {
			return nil
		}
		select {
		case <-cleanupCtx.Done():
			return errors.Join(lastErr, cleanupCtx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) terminateHarness(ctx context.Context, active *session) error {
	exists, err := manager.proveContainerOwnership(ctx, active, active.containerID, "openclaw-harness")
	if err != nil || !exists {
		return err
	}
	stopResult, stopErr := manager.cli.Run(ctx, nil, "container", "stop", "--time", strconv.Itoa(gracefulStopSeconds), active.containerID)
	stopFailed := stopErr != nil && !resourceMissing(stopResult.stderr, "container") && !strings.Contains(strings.ToLower(string(stopResult.stderr)), "not running")
	processes, processErr := containerProcesses(ctx, manager.cli, active.apiKey, active.containerID)
	if !stopFailed && processErr == nil && len(processes) == 0 {
		return nil
	}
	killResult, killErr := manager.cli.Run(ctx, nil, "container", "kill", "--signal", "KILL", active.containerID)
	if killErr != nil && !resourceMissing(killResult.stderr, "container") && !strings.Contains(strings.ToLower(string(killResult.stderr)), "not running") {
		return errors.Join(
			fmt.Errorf("gracefully stop OpenClaw harness: %s", strings.TrimSpace(string(redactSession(stopResult.stderr, active)))),
			fmt.Errorf("kill OpenClaw harness: %s", strings.TrimSpace(string(redactSession(killResult.stderr, active)))),
		)
	}
	processes, err = containerProcesses(ctx, manager.cli, active.apiKey, active.containerID)
	if err != nil {
		return err
	}
	if len(processes) != 0 {
		return errors.New("OpenClaw processes remain after graceful-stop and KILL fallback")
	}
	return nil
}

func (manager *Manager) collectArtifacts(ctx context.Context, active *session) error {
	var errs []error
	logs, err := manager.cli.Run(ctx, nil, "container", "logs", active.containerID)
	if err != nil && !strings.Contains(strings.ToLower(string(logs.stderr)), "not running") {
		errs = append(errs, fmt.Errorf("collect OpenClaw gateway logs: %s", strings.TrimSpace(string(redactBytes(logs.stderr, active.apiKey)))))
	} else {
		content := allowGatewayLogs(append(append([]byte(nil), logs.stdout...), logs.stderr...), active.apiKey, active.gatewayToken)
		path := filepath.Join(active.artifactDir, "gateway.log")
		if writeErr := writeArtifact(path, content); writeErr != nil {
			errs = append(errs, writeErr)
		} else {
			active.logPaths = appendUnique(active.logPaths, path)
		}
	}
	archive, found, archiveErr := copyContainerArchive(ctx, manager.cli, active.apiKey, active.containerID, stateContainerPath+"/agents/main/sessions")
	if archiveErr != nil {
		errs = append(errs, archiveErr)
	} else if found {
		paths, err := extractTelemetry(active.artifactDir, archive, active.apiKey, active.gatewayToken)
		if err != nil {
			errs = append(errs, err)
		} else {
			active.logPaths = appendUnique(active.logPaths, paths...)
		}
	}
	if len(errs) == 0 {
		index, err := json.MarshalIndent(struct {
			Paths []string `json:"paths"`
		}{Paths: telemetryRelativePaths(active.artifactDir, active.logPaths)}, "", "  ")
		if err != nil {
			errs = append(errs, fmt.Errorf("encode OpenClaw telemetry index: %w", err))
		} else {
			index = append(index, '\n')
			if err := writeArtifact(filepath.Join(active.artifactDir, "telemetry.index.json"), index); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (manager *Manager) writeRunArtifacts(active *session, stdout, stderr []byte) ([]string, error) {
	paths := []string{filepath.Join(active.artifactDir, "agent.json"), filepath.Join(active.artifactDir, "agent.stderr")}
	if err := writeArtifact(paths[0], stdout); err != nil {
		return nil, err
	}
	if err := writeArtifact(paths[1], stderr); err != nil {
		return nil, err
	}
	return paths, nil
}

func (manager *Manager) failedResult(active *session, started time.Time, stdout, stderr []byte, err error) (core.HarnessResult, error) {
	stdout = redactSession(stdout, active)
	stderr = redactSession(stderr, active)
	paths, writeErr := manager.writeRunArtifacts(active, stdout, stderr)
	active.logPaths = appendUnique(active.logPaths, paths...)
	if writeErr != nil {
		err = errors.Join(err, writeErr)
	}
	status := core.StatusFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = core.StatusCanceled
	}
	return core.HarnessResult{Status: status, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...), Error: err.Error()}, err
}

type stagedFile struct {
	content []byte
	mode    int64
}

func stageArchive(files map[string]stagedFile) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	if err := writer.WriteHeader(&tar.Header{Name: "aries-stage", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 0, Gid: 0}); err != nil {
		return nil, err
	}
	for _, name := range []string{"openclaw.json", "model.key", "gateway.key", "launch", "run-agent", "id_ed25519", "known_hosts", "ssh-config"} {
		file, ok := files[name]
		if !ok {
			return nil, fmt.Errorf("missing staged OpenClaw file %q", name)
		}
		header := &tar.Header{Name: "aries-stage/" + name, Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.content)), Uid: 0, Gid: 0}
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
		name, ok := cleanArchivePath(header.Name)
		if !ok || header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size < 0 || header.Size > maxDockerOutput {
			continue
		}
		base := filepath.Base(name)
		if base != "sessions.json" && !strings.HasSuffix(base, ".jsonl") && !strings.Contains(strings.ToLower(base), "trajectory") {
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
		if len(line) > 4096 || bytes.ContainsRune(line, 0) {
			continue
		}
		if bytes.Contains(bytes.ToLower(line), []byte("authorization:")) || bytes.Contains(bytes.ToLower(line), []byte("bearer ")) {
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
		if errors.Is(err, os.ErrExist) {
			existing, readErr := readStablePrivateFile(path, 0o600)
			if readErr != nil {
				return fmt.Errorf("verify existing artifact %q: %w", path, readErr)
			}
			defer clear(existing)
			if bytes.Equal(existing, content) {
				return nil
			}
			return fmt.Errorf("artifact %q already exists with different content", path)
		}
		return err
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

func clearSessionSecrets(active *session) {
	clear(active.apiKey)
	active.apiKey = nil
	clear(active.gatewayToken)
	active.gatewayToken = nil
}

func telemetryRelativePaths(artifactDir string, paths []string) []string {
	telemetryDir := filepath.Join(artifactDir, "telemetry") + string(filepath.Separator)
	relative := make([]string, 0)
	for _, path := range paths {
		if !strings.HasPrefix(path, telemetryDir) {
			continue
		}
		name, err := filepath.Rel(artifactDir, path)
		if err == nil {
			relative = append(relative, filepath.ToSlash(name))
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
	if !ok {
		return nil, false
	}
	return []byte(value), true
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
	if strings.TrimSpace(value) == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
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

func containsExactProcess(processes []string, command string) bool {
	for _, process := range processes {
		if process == command {
			return true
		}
	}
	return false
}

func redactSession(content []byte, active *session) []byte {
	return redactSecrets(content, active.apiKey, active.gatewayToken)
}

func resourceMissing(stderr []byte, kind string) bool {
	message := strings.ToLower(string(stderr))
	return strings.Contains(message, "no such "+kind) || strings.Contains(message, kind+" not found")
}
