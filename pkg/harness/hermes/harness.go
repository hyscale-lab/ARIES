package hermes

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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/sirupsen/logrus"
)

const (
	defaultDockerSocket    = "/var/run/docker.sock"
	defaultCleanupTimeout  = 30 * time.Second
	defaultStartTimeout    = 45 * time.Second
	defaultAgentTimeout    = 20 * time.Minute
	defaultMaxTurns        = 90
	defaultTerminalTimeout = 180
	maxDockerOutput        = 16 << 20
	maxAPIKeyBytes         = 16 << 10
	gracefulStopSeconds    = 5
	execTrailerKeep        = 256

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
)

// execShell reports the child's exit status as a delimited stderr trailer.
// Docker's exec inspection can race a fast child, so the trailer is the
// authoritative status rather than a second inspect call.
const execShell = `token=$1
shift
"$@"
status=$?
printf '\036ARIES_HERMES_EXIT_%s=%s\037' "$token" "$status" >&2
exit "$status"`

// idleCommand replaces the upstream entrypoint so ARIES owns when the agent
// starts. It first aligns the staged runtime with the unprivileged `hermes`
// UID: the archive already carries that ownership, but the Engine's copy API
// resets it to root, and the start command is the one place that runs as root
// after the copy and before readiness.
var (
	idleEntrypoint = []string{"/bin/sh"}
	idleCommand    = []string{"-c", fmt.Sprintf("chown -R %d:%d %s && exec sleep infinity", runtimeUID, runtimeGID, stagedRoot)}
)

// Options are the host-local inputs to one upstream Hermes container.
type Options struct {
	Image           string
	OutputDir       string
	DockerSocket    string
	APIKeyLookup    func(string) ([]byte, bool)
	MaxTurns        int
	TerminalTimeout int
	CleanupTimeout  time.Duration
	StartTimeout    time.Duration
	AgentTimeout    time.Duration
	Logger          *logrus.Logger
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
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
}

type Manager struct {
	client          dockerClient
	image           string
	outputDir       string
	cleanupTimeout  time.Duration
	startTimeout    time.Duration
	agentTimeout    time.Duration
	maxTurns        int
	terminalTimeout int
	logger          *logrus.Logger
	apiKeyLookup    func(string) ([]byte, bool)
	newID           func() (string, error)

	mu        sync.Mutex
	active    *session
	stopping  bool
	stopDone  chan struct{}
	stopErr   error
	closeOnce sync.Once
	closeErr  error
}

type session struct {
	runID         string
	taskID        string
	attemptID     string
	containerName string
	containerID   string
	artifactDir   string
	endpoint      core.ToolEndpoint
	model         core.ModelConfig
	agentTimeout  time.Duration
	apiKey        []byte
	runAttempted  bool
	logPaths      []string
}

var _ runner.AgentHarness = (*Manager)(nil)

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

// New constructs a harness without contacting Docker.
func New(options Options) (*Manager, error) {
	if err := containerimage.ValidatePinnedTagOnly(options.Image); err != nil {
		return nil, fmt.Errorf("Hermes image: %w", err)
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("Hermes output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve Hermes output directory: %w", err)
	}
	if err := ensurePrivateDirectory(outputDir); err != nil {
		return nil, fmt.Errorf("prepare Hermes output directory: %w", err)
	}
	if options.DockerSocket == "" {
		options.DockerSocket = defaultDockerSocket
	}
	host := options.DockerSocket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-hermes/1"))
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
	if options.MaxTurns <= 0 {
		options.MaxTurns = defaultMaxTurns
	}
	if options.TerminalTimeout <= 0 {
		options.TerminalTimeout = defaultTerminalTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	if options.APIKeyLookup == nil {
		options.APIKeyLookup = environmentAPIKeyLookup
	}
	return &Manager{
		client: api, image: options.Image, outputDir: outputDir,
		cleanupTimeout: options.CleanupTimeout, startTimeout: options.StartTimeout,
		agentTimeout: options.AgentTimeout, maxTurns: options.MaxTurns,
		terminalTimeout: options.TerminalTimeout, logger: options.Logger,
		apiKeyLookup: options.APIKeyLookup, newID: randomID,
	}, nil
}

func (manager *Manager) Start(ctx context.Context, request core.HarnessRequest) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.active != nil || manager.stopping {
		return errors.New("Hermes harness is already active")
	}
	if err := validateRunID(request.RunID); err != nil {
		return err
	}
	if err := validateTaskID(request.TaskID); err != nil {
		return err
	}
	if request.Timeout < 0 {
		return errors.New("Hermes task timeout must not be negative")
	}
	resources, err := harnessResources(request)
	if err != nil {
		return err
	}
	agentTimeout := request.Timeout
	if agentTimeout == 0 {
		agentTimeout = manager.agentTimeout
	}
	configuration, err := renderConfig(request.Model, manager.maxTurns)
	if err != nil {
		return err
	}
	environment, err := containerEnvironment(request.Endpoint, workspaceRoot, manager.terminalTimeout)
	if err != nil {
		return err
	}
	apiKeySource, ok := manager.apiKeyLookup(request.Model.APIKeyEnv)
	if !ok {
		clear(apiKeySource)
		return fmt.Errorf("Hermes API-key environment %q is not set", request.Model.APIKeyEnv)
	}
	apiKey := bytes.Clone(apiKeySource)
	clear(apiKeySource)
	if err := validateAPIKey(apiKey); err != nil {
		clear(apiKey)
		return err
	}
	if bytes.Contains(configuration, apiKey) {
		clear(apiKey)
		return errors.New("rendered Hermes config contains the API-key value")
	}
	id, err := manager.newID()
	if err != nil {
		clear(apiKey)
		return fmt.Errorf("generate Hermes harness ID: %w", err)
	}
	containerConfig := &container.Config{
		Image:      manager.image,
		Env:        environment,
		Entrypoint: append([]string(nil), idleEntrypoint...),
		Cmd:        append([]string(nil), idleCommand...),
		Labels: map[string]string{
			"aries.managed": "true", "aries.kind": "hermes-harness",
			"aries.component": "harness",
			"aries.run":       request.RunID, "aries.task": request.TaskID,
			"aries.attempt": id,
		},
	}
	hostConfig := &container.HostConfig{NetworkMode: container.NetworkMode(request.Endpoint.Network), Resources: resources}
	active := &session{
		runID: request.RunID, taskID: request.TaskID, attemptID: id,
		containerName: "aries-hermes-" + id,
		artifactDir:   filepath.Join(manager.outputDir, request.TaskID, "harness"),
		endpoint:      request.Endpoint, model: request.Model,
		agentTimeout: agentTimeout, apiKey: apiKey,
	}
	fail := func(primary error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), manager.cleanupTimeout)
		cleanupErr := manager.stopSession(cleanupCtx, active)
		cancel()
		if cleanupErr != nil {
			manager.active = active
			manager.stopErr = cleanupErr
			return errors.Join(primary, fmt.Errorf("rollback partial Hermes harness: %w", cleanupErr))
		}
		_ = os.RemoveAll(active.artifactDir)
		return primary
	}
	if err := ensurePrivateDirectory(active.artifactDir); err != nil {
		return fail(fmt.Errorf("create Hermes artifact directory: %w", err))
	}
	configArtifact := filepath.Join(active.artifactDir, "config.yaml")
	if err := writeArtifact(configArtifact, redactSession(configuration, active)); err != nil {
		return fail(fmt.Errorf("retain rendered Hermes config: %w", err))
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
		return fail(fmt.Errorf("create Hermes container: %w", err))
	}
	active.containerID = created.ID
	if strings.TrimSpace(active.containerID) == "" {
		return fail(errors.New("Docker returned an empty Hermes container ID"))
	}
	if _, err := manager.client.CopyToContainer(ctx, active.containerID, client.CopyToContainerOptions{
		DestinationPath: "/", Content: bytes.NewReader(archive), CopyUIDGID: true,
	}); err != nil {
		return fail(fmt.Errorf("copy private Hermes runtime: %w", err))
	}
	if err := manager.validateContainer(ctx, active); err != nil {
		return fail(err)
	}
	if _, err := manager.client.ContainerStart(ctx, active.containerID, client.ContainerStartOptions{}); err != nil {
		return fail(fmt.Errorf("start Hermes container: %w", err))
	}
	readyCtx, cancel := context.WithTimeout(ctx, manager.startTimeout)
	err = manager.waitReady(readyCtx, active)
	cancel()
	if err != nil {
		return fail(err)
	}
	manager.active = active
	manager.stopErr = nil
	manager.logger.WithContext(ctx).WithFields(logrus.Fields{"task_id": active.taskID, "container": active.containerName}).Info("Hermes harness started")
	return nil
}

func harnessResources(request core.HarnessRequest) (container.Resources, error) {
	var resources container.Resources
	if request.CPU != nil {
		scaled := *request.CPU * 1e9
		if *request.CPU <= 0 || math.IsNaN(*request.CPU) || math.IsInf(*request.CPU, 0) || scaled >= math.Exp2(63) {
			return container.Resources{}, errors.New("Hermes CPU must be finite, positive, and convert to NanoCPUs below 2^63")
		}
		resources.NanoCPUs = int64(scaled)
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt64>>20 {
			return container.Resources{}, fmt.Errorf("Hermes memory must be positive and no greater than %d MiB", int64(math.MaxInt64)>>20)
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
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Hermes harness is not started")
	}
	if active.runAttempted {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Hermes harness accepts exactly one task instruction")
	}
	if strings.TrimSpace(instruction) == "" || strings.ContainsRune(instruction, 0) {
		manager.mu.Unlock()
		return core.HarnessResult{Status: core.StatusFailed}, errors.New("Hermes task instruction is invalid")
	}
	active.runAttempted = true
	manager.mu.Unlock()

	runCtx, cancel := context.WithTimeout(ctx, active.agentTimeout)
	result, runErr := manager.execAttached(runCtx, active.containerID,
		[]string{agentWrapperPath, active.model.Model, active.model.Provider, instruction}, workspaceRoot)
	cancel()

	stdout := redactSession(result.stdout, active)
	stderr := redactSession(result.stderr, active)
	err := runErr
	if err == nil && result.exitCode != 0 {
		err = fmt.Errorf("Hermes one-shot exited with status %d", result.exitCode)
	}
	artifactCtx, artifactCancel := context.WithTimeout(context.WithoutCancel(ctx), manager.cleanupTimeout)
	artifactErr := manager.collectArtifacts(artifactCtx, active, stdout, stderr)
	artifactCancel()
	err = errors.Join(err, artifactErr)
	if err != nil {
		err = redactSessionError(err, active)
		return failedHarnessResult(active, started, err), err
	}
	return core.HarnessResult{
		Status: core.StatusSucceeded, FinalResponse: strings.TrimRight(string(stdout), "\n"),
		Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...),
	}, nil
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
		return execResult{exitCode: -1}, fmt.Errorf("generate Hermes exec token: %w", err)
	}
	wrapped := append([]string{"/bin/sh", "-c", execShell, "aries-hermes-exec", token}, command...)
	created, err := manager.client.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		AttachStdout: true, AttachStderr: true, Cmd: wrapped, WorkingDir: workdir,
	})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("create Hermes exec: %w", err)
	}
	attached, err := manager.client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("attach Hermes exec: %w", err)
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
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, fmt.Errorf("read Hermes exec: %w", copyErr)
	}
	if stdout.exceeded || stderr.exceeded {
		return execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}, errors.New("Hermes exec output exceeded its bound")
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
			return client.ExecInspectResult{}, fmt.Errorf("inspect Hermes exec: %w", err)
		}
		if !inspection.Running {
			return inspection, nil
		}
		if inspection.PID > 0 {
			present, err := manager.containerHasPID(ctx, containerID, inspection.PID)
			if err != nil {
				return client.ExecInspectResult{}, fmt.Errorf("inspect Hermes exec process: %w", err)
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
	return &execTrailer{destination: destination, prefix: []byte("\x1eARIES_HERMES_EXIT_" + token + "="), done: make(chan struct{})}
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
		return -1, errors.New("Hermes exec output is missing its exit trailer")
	}
	start := bytes.LastIndex(content[:len(content)-1], trailer.prefix)
	if start < 0 {
		return -1, errors.New("Hermes exec output has an invalid exit trailer")
	}
	exitCode, err := strconv.Atoi(string(content[start+len(trailer.prefix) : len(content)-1]))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return -1, errors.New("Hermes exec output has an invalid exit code")
	}
	if _, err := trailer.destination.Write(content[:start]); err != nil {
		return -1, fmt.Errorf("write Hermes exec stderr: %w", err)
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

// waitReady confirms the container is running and the staged runtime is intact
// before any task instruction is accepted. Hermes exposes no readiness service,
// so the positive signal is the CLI answering from the staged configuration.
//
// The probe deliberately runs `hermes --version` rather than only stat-ing the
// staged files. Those checks run as root and pass regardless of ownership,
// while the real agent runs through the PATH shim as an unprivileged user; only
// invoking the CLI proves the staged runtime is readable by the identity that
// will actually use it.
func (manager *Manager) waitReady(ctx context.Context, active *session) error {
	probe := `test -x ` + agentWrapperPath + ` && test -r ` + configContainerPath + ` && test -r ` + modelKeyPath +
		` && test -r ` + identityContainerFS + ` && command -v ssh >/dev/null && hermes --version >/dev/null 2>&1`
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		result, err := manager.execAttached(probeCtx, active.containerID, []string{"/bin/sh", "-c", probe}, workspaceRoot)
		cancel()
		if err == nil && result.exitCode == 0 {
			return nil
		}
		inspection, inspectErr := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil {
			return fmt.Errorf("inspect Hermes readiness: %w", inspectErr)
		}
		if inspection.Container.State == nil || !inspection.Container.State.Running {
			return errors.New("Hermes container exited before readiness")
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await Hermes readiness: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (manager *Manager) validateContainer(ctx context.Context, active *session) error {
	inspection, err := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect Hermes container: %w", err)
	}
	containerInfo := inspection.Container
	if containerInfo.ID != active.containerID || containerInfo.Config == nil || containerInfo.HostConfig == nil {
		return errors.New("Hermes container inspection is incomplete")
	}
	configuration := containerInfo.Config
	if configuration.Image != manager.image || !slices.Equal(configuration.Cmd, idleCommand) || !slices.Equal(configuration.Entrypoint, idleEntrypoint) {
		return errors.New("Hermes image or idle command differs from the pinned direct configuration")
	}
	if configuration.Labels["aries.managed"] != "true" || configuration.Labels["aries.kind"] != "hermes-harness" || configuration.Labels["aries.component"] != "harness" ||
		configuration.Labels["aries.run"] != active.runID || configuration.Labels["aries.task"] != active.taskID || configuration.Labels["aries.attempt"] != active.attemptID {
		return errors.New("Hermes container labels do not match the task")
	}
	for _, value := range append(append([]string(nil), configuration.Env...), configuration.Cmd...) {
		if containsSecret(value, active.apiKey) {
			return errors.New("Hermes secret entered Docker configuration")
		}
	}
	for _, value := range configuration.Labels {
		if containsSecret(value, active.apiKey) {
			return errors.New("Hermes secret entered Docker labels")
		}
	}
	if string(containerInfo.HostConfig.NetworkMode) != active.endpoint.Network {
		return errors.New("Hermes container must use only the task network")
	}
	// The upstream image declares VOLUME /opt/data, so Docker always creates
	// one anonymous local volume there. ARIES relocates HERMES_HOME away from
	// it and removes it with the container, but the mount itself is
	// unavoidable, so allow exactly that and nothing else — in particular no
	// bind mount, which is how host state would reach the harness.
	for _, mount := range containerInfo.Mounts {
		if mount.Type != "volume" || mount.Destination != imageDeclaredVolume || mount.Name == "" {
			return errors.New("Hermes container must not carry mounts beyond the image-declared volume")
		}
	}
	if len(containerInfo.HostConfig.Binds) != 0 || len(containerInfo.HostConfig.Mounts) != 0 {
		return errors.New("Hermes container must not request binds or mounts")
	}
	return nil
}

func (manager *Manager) runtimeArchive(active *session, configuration []byte) ([]byte, error) {
	identity, err := readStablePrivateFile(active.endpoint.IdentitySourceFile, 0o600)
	if err != nil {
		return nil, fmt.Errorf("read Hermes SSH identity: %w", err)
	}
	defer clear(identity)
	files := map[string]stagedFile{
		strings.TrimPrefix(configContainerPath, "/"): {content: configuration, mode: 0o600},
		strings.TrimPrefix(modelKeyPath, "/"):        {content: active.apiKey, mode: 0o600},
		strings.TrimPrefix(identityContainerFS, "/"): {content: identity, mode: 0o600},
		strings.TrimPrefix(agentWrapperPath, "/"):    {content: agentWrapperScript(active.model.APIKeyEnv), mode: 0o555},
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

type stagedFile struct {
	content []byte
	mode    int64
}

func stageArchive(files map[string]stagedFile) ([]byte, error) {
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	// Everything is owned by the image's unprivileged `hermes` user, because
	// that is the identity the PATH shim drops to. Modes stay restrictive: the
	// wrapper still reads the key as root before handing off.
	directories := []string{"run/aries", "run/aries/hermes", "run/aries/ssh", "run/aries/workspace"}
	for _, name := range directories {
		mode := int64(0o700)
		if name == "run/aries/workspace" {
			mode = 0o755
		}
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: mode, Uid: runtimeUID, Gid: runtimeGID}); err != nil {
			return nil, err
		}
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		file := files[name]
		if name == "" || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("invalid staged Hermes path %q", name)
		}
		header := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: file.mode, Size: int64(len(file.content)), Uid: runtimeUID, Gid: runtimeGID}
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
		errs = append(errs, fmt.Errorf("inspect Hermes before stop: %w", inspectErr))
	}
	shouldStop := inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running
	if shouldStop {
		timeout := gracefulStopSeconds
		if _, err := manager.client.ContainerStop(ctx, active.containerID, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !errdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("stop Hermes container: %w", err))
		}
		inspection, inspectErr = manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{})
		if inspectErr != nil && !errdefs.IsNotFound(inspectErr) {
			errs = append(errs, fmt.Errorf("inspect Hermes after stop: %w", inspectErr))
		}
		if !errdefs.IsNotFound(inspectErr) && (inspectErr != nil || inspection.Container.State == nil || inspection.Container.State.Running) {
			if _, err := manager.client.ContainerKill(ctx, active.containerID, client.ContainerKillOptions{Signal: "KILL"}); err != nil && !errdefs.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("kill Hermes container: %w", err))
			}
		}
	}
	if _, err := manager.client.ContainerRemove(ctx, active.containerID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("remove Hermes container: %w", err))
	}
	if _, err := manager.client.ContainerInspect(ctx, active.containerID, client.ContainerInspectOptions{}); err == nil {
		errs = append(errs, errors.New("Hermes container remains after removal"))
		return errors.Join(errs...)
	} else if !errdefs.IsNotFound(err) {
		errs = append(errs, fmt.Errorf("verify Hermes removal: %w", err))
		return errors.Join(errs...)
	}
	active.containerID = ""
	clearSessionSecrets(active)
	if warning := errors.Join(errs...); warning != nil {
		manager.logger.WithContext(ctx).WithField("task_id", active.taskID).WithError(warning).Warn("Hermes cleanup recovered after lifecycle errors")
	}
	return nil
}

func (manager *Manager) collectArtifacts(ctx context.Context, active *session, stdout, stderr []byte) error {
	var errs []error
	for _, item := range []struct {
		name    string
		content []byte
	}{{"hermes_stdout.log", stdout}, {"hermes_stderr.log", stderr}} {
		path := filepath.Join(active.artifactDir, item.name)
		if err := writeArtifact(path, item.content); err != nil {
			errs = append(errs, fmt.Errorf("retain %s: %w", item.name, err))
			continue
		}
		active.logPaths = appendUnique(active.logPaths, path)
	}
	if logs, err := manager.client.ContainerLogs(ctx, active.containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true}); err != nil {
		errs = append(errs, fmt.Errorf("collect Hermes container logs: %w", err))
	} else {
		var out, errBuffer limitedBuffer
		out.limit, errBuffer.limit = maxDockerOutput, maxDockerOutput
		_, copyErr := stdcopy.StdCopy(&out, &errBuffer, logs)
		closeErr := logs.Close()
		if copyErr != nil || closeErr != nil || out.exceeded || errBuffer.exceeded {
			errs = append(errs, errors.Join(copyErr, closeErr, errors.New("Hermes container logs exceeded their bound")))
		} else {
			content := allowContainerLogs(append(out.Bytes(), errBuffer.Bytes()...), active.apiKey)
			path := filepath.Join(active.artifactDir, "container.log")
			if err := writeArtifact(path, content); err != nil {
				errs = append(errs, err)
			} else {
				active.logPaths = appendUnique(active.logPaths, path)
			}
		}
	}
	sessionPaths, sessionErr := manager.collectSessions(ctx, active)
	if sessionErr != nil {
		errs = append(errs, sessionErr)
	} else {
		active.logPaths = appendUnique(active.logPaths, sessionPaths...)
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
		errs = append(errs, fmt.Errorf("write Hermes telemetry index: %w", err))
	}
	return errors.Join(errs...)
}

// collectSessions exports Hermes's own SQLite session store as the
// message-level trajectory. Writing to stdout avoids depending on a path
// inside the container that the export may or may not have produced.
func (manager *Manager) collectSessions(ctx context.Context, active *session) ([]string, error) {
	result, err := manager.execAttached(ctx, active.containerID,
		[]string{"hermes", "sessions", "export", "-"}, workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("export Hermes sessions: %w", err)
	}
	if result.exitCode != 0 || len(bytes.TrimSpace(result.stdout)) == 0 {
		// A run that never reached the model leaves no session; that is not a
		// harness failure and must not mask the real result.
		manager.logger.WithContext(ctx).WithField("task_id", active.taskID).Debug("Hermes produced no session export")
		return nil, nil
	}
	path := filepath.Join(active.artifactDir, "telemetry", "sessions.jsonl")
	if err := writeArtifact(path, redactSession(result.stdout, active)); err != nil {
		return nil, fmt.Errorf("retain Hermes sessions: %w", err)
	}
	return []string{path}, nil
}

func allowContainerLogs(content []byte, secrets ...[]byte) []byte {
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

func failedHarnessResult(active *session, started time.Time, err error) core.HarnessResult {
	status := core.StatusFailed
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = core.StatusCanceled
	}
	errorText := ""
	if err != nil {
		errorText = string(redactSession([]byte(err.Error()), active))
	}
	return core.HarnessResult{Status: status, Duration: time.Since(started), LogPaths: append([]string(nil), active.logPaths...), Error: errorText}
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
		return errors.New("Hermes API key is empty or exceeds its bound")
	}
	if bytes.ContainsAny(value, "\x00\r\n") {
		return errors.New("Hermes API key contains NUL or a line break")
	}
	return nil
}

func environmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	return []byte(value), ok
}

func validateRunID(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("Hermes run ID must contain 1 to 128 safe characters")
	}
	if !safeIdentifierChars(value) {
		return errors.New("Hermes run ID contains an unsafe character")
	}
	return nil
}

func validateTaskID(value string) error {
	if value == "" || len(value) > 149 || !safeIdentifierChars(value) {
		return errors.New("Hermes task ID is invalid")
	}
	return nil
}

// safeIdentifierChars accepts an alphanumeric label that may also carry `-`,
// `_`, or `.` after its first character, so the value stays safe in a container
// name, a label, and a filesystem path.
func safeIdentifierChars(value string) bool {
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return false
	}
	return true
}

func randomID() (string, error) {
	var content [8]byte
	if _, err := rand.Read(content[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(content[:]), nil
}

func clearSessionSecrets(active *session) {
	clear(active.apiKey)
	active.apiKey = nil
}

func redactSession(content []byte, active *session) []byte {
	return redactSecrets(content, active.apiKey)
}

type sessionRedactedError struct {
	message string
	cause   error
}

func (err *sessionRedactedError) Error() string { return err.message }
func (err *sessionRedactedError) Unwrap() error { return err.cause }

func redactSessionError(err error, active *session) error {
	if err == nil {
		return nil
	}
	message := string(redactSession([]byte(err.Error()), active))
	if message == err.Error() {
		return err
	}
	return &sessionRedactedError{message: message, cause: err}
}
