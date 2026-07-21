package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

const (
	defaultDockerBinary   = "docker"
	defaultExecHelperPath = "bin/aries-exec-helper"
	defaultCleanupTimeout = 30 * time.Second
	stopTimeoutSeconds    = 5

	managedLabel   = "aries.managed=true"
	milestoneLabel = "aries.milestone=m3"
	networkAlias   = "task-sandbox"
)

// Options are the explicit host-local inputs to the Docker sandbox manager.
type Options struct {
	OutputDir      string
	DockerBinary   string
	DockerSocket   string
	ExecHelperPath string
	CleanupTimeout time.Duration
	Logger         *slog.Logger
}

// Manager starts one isolated Docker container and network per task.
type Manager struct {
	cli            commandRunner
	outputDir      string
	cleanupTimeout time.Duration
	logger         *slog.Logger
	helperPath     string
	newID          func() (string, error)
	engine         execEngine
}

// Sandbox is a live Docker task environment.
type Sandbox struct {
	cli            commandRunner
	containerID    string
	containerName  string
	networkName    string
	workdir        string
	artifactDir    string
	runtimeDir     string
	outputDir      string
	cleanupTimeout time.Duration
	runID          string
	taskID         string
	engine         execEngine
	execGate       chan struct{}
	testHooks      transferTestHooks

	mu             sync.Mutex
	containerOwned bool
	containerRun   bool
	networkOwned   bool
	runtimeOwned   bool
	stopped        bool
	stopping       bool
	stopDone       chan struct{}
	stopErr        error
}

type commandResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
}

type commandRunner interface {
	Run(context.Context, []byte, ...string) (commandResult, error)
}

type execRunner struct {
	binary string
	socket string
}

var (
	_ runner.ToolSandbox = (*Manager)(nil)
	_ runner.Sandbox     = (*Sandbox)(nil)
)

// New constructs a Docker manager without contacting the daemon.
func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("docker sandbox output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve docker sandbox output directory: %w", err)
	}
	if options.DockerBinary == "" {
		options.DockerBinary = defaultDockerBinary
	}
	if options.DockerSocket == "" {
		options.DockerSocket = defaultDockerSocket
	}
	if options.ExecHelperPath == "" {
		options.ExecHelperPath = defaultExecHelperPath
	}
	helperPath, err := filepath.Abs(options.ExecHelperPath)
	if err != nil {
		return nil, fmt.Errorf("resolve Docker exec helper: %w", err)
	}
	if strings.ContainsRune(options.DockerBinary, 0) {
		return nil, errors.New("docker binary contains NUL")
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	return &Manager{
		cli:            execRunner{binary: options.DockerBinary, socket: options.DockerSocket},
		outputDir:      outputDir,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
		helperPath:     helperPath,
		newID:          randomID,
		engine:         newEngineClient(options.DockerSocket),
	}, nil
}

// Start creates and positively inspects one running task container. Any failed
// partial acquisition is rolled back before Start returns.
func (m *Manager) Start(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	if err := validateIdentity("run", request.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", request.TaskID); err != nil {
		return nil, err
	}
	environment := request.Environment
	if err := validateEnvironment(environment); err != nil {
		return nil, err
	}
	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("generate docker sandbox ID: %w", err)
	}
	containerName := "aries-task-" + id
	networkName := "aries-net-" + id
	artifactDir := filepath.Join(m.outputDir, "sandboxes", id)
	if err := ensureDirectory(artifactDir); err != nil {
		return nil, fmt.Errorf("create docker sandbox artifact directory: %w", err)
	}
	helperPath, err := stageExecHelper(m.helperPath, artifactDir)
	if err != nil {
		return nil, fmt.Errorf("stage Docker exec helper: %w", err)
	}
	runtimeDir, err := os.MkdirTemp("", "aries-exec-"+id+"-")
	if err != nil {
		return nil, fmt.Errorf("create private Docker exec runtime: %w", err)
	}

	sandbox := &Sandbox{
		cli:            m.cli,
		containerName:  containerName,
		networkName:    networkName,
		workdir:        environment.Workdir,
		artifactDir:    artifactDir,
		runtimeDir:     runtimeDir,
		outputDir:      m.outputDir,
		cleanupTimeout: m.cleanupTimeout,
		runID:          request.RunID,
		taskID:         request.TaskID,
		engine:         m.engine,
		execGate:       make(chan struct{}, 1),
		runtimeOwned:   true,
	}

	networkArgs := []string{
		"network", "create",
		"--label", managedLabel,
		"--label", milestoneLabel,
		"--label", "aries.kind=task-network",
		"--label", "aries.run=" + request.RunID,
		"--label", "aries.task=" + request.TaskID,
	}
	if !environment.AllowNetwork {
		networkArgs = append(networkArgs, "--internal")
	}
	networkArgs = append(networkArgs, networkName)
	sandbox.networkOwned = true
	if _, err := runChecked(ctx, m.cli, nil, networkArgs...); err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("create docker task network: %w", err))
	}

	sandbox.containerID = containerName
	sandbox.containerOwned = true
	createResult, err := runChecked(ctx, m.cli, nil, containerCreateArgs(request, containerName, networkName, helperPath, runtimeDir)...)
	if err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("create docker task container: %w", err))
	}
	sandbox.containerID = strings.TrimSpace(string(createResult.stdout))
	if sandbox.containerID == "" {
		return nil, sandbox.rollbackStart(ctx, errors.New("create docker task container: Docker returned an empty container ID"))
	}
	if _, err := runChecked(ctx, m.cli, nil, "container", "start", sandbox.containerID); err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("start docker task container: %w", err))
	}
	sandbox.containerRun = true
	inspection, err := inspectContainer(ctx, m.cli, sandbox.containerID)
	if err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("inspect started docker task container: %w", err))
	}
	if !inspection.State.Running {
		return nil, sandbox.rollbackStart(ctx, errors.New("inspect started docker task container: container is not running"))
	}
	if inspection.Config.WorkingDir != environment.Workdir {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("inspect started docker task container: workdir %q, want %q", inspection.Config.WorkingDir, environment.Workdir))
	}
	if _, ok := inspection.NetworkSettings.Networks[networkName]; !ok {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("inspect started docker task container: network %q is not attached", networkName))
	}
	if inspection.Config.Labels["aries.run"] != request.RunID || inspection.Config.Labels["aries.task"] != request.TaskID || inspection.Config.Labels["aries.managed"] != "true" {
		return nil, sandbox.rollbackStart(ctx, errors.New("inspect started docker task container: ARIES ownership labels do not match"))
	}
	if !inspection.hasReadonlyMount(helperContainerPath) || !inspection.hasReadonlyMount(socketContainerDir) {
		return nil, sandbox.rollbackStart(ctx, errors.New("inspect started docker task container: trusted helper mounts do not match"))
	}
	networkInspection, err := inspectNetwork(ctx, m.cli, networkName)
	if err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("inspect started docker task network: %w", err))
	}
	if networkInspection.Labels["aries.run"] != request.RunID || networkInspection.Labels["aries.task"] != request.TaskID || networkInspection.Labels["aries.managed"] != "true" {
		return nil, sandbox.rollbackStart(ctx, errors.New("inspect started docker task network: ARIES ownership labels do not match"))
	}

	m.logger.InfoContext(ctx, "docker task sandbox started", "container", containerName, "network", networkName)
	return sandbox, nil
}

// ContainerID returns the immutable Docker container identifier.
func (s *Sandbox) ContainerID() string { return s.containerID }

// ContainerName returns the generated, task-data-free Docker name.
func (s *Sandbox) ContainerName() string { return s.containerName }

// NetworkName returns the task-scoped network used by the future SSH bridge.
func (s *Sandbox) NetworkName() string { return s.networkName }

// Workdir returns the benchmark-declared working directory in the container.
func (s *Sandbox) Workdir() string { return s.workdir }

// Exec runs one direct argv through the trusted helper and preserves separate
// output streams and exit status. A nonzero command exit is a result.
func (s *Sandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if err := validateCommand(command); err != nil {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	execCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()

	select {
	case s.execGate <- struct{}{}:
		defer func() { <-s.execGate }()
	case <-execCtx.Done():
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, execCtx.Err()
	}

	result, launched, err := s.engine.Exec(execCtx, s.containerID, s.runtimeDir, command)
	if err == nil || !launched {
		return result, err
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(execCtx), s.cleanupTimeout)
	defer cleanupCancel()
	restartErr := s.restartAfterExecFailure(cleanupCtx)
	if restartErr != nil {
		restartErr = fmt.Errorf("restart Docker task container after exec failure: %w", restartErr)
	}
	return result, errors.Join(err, restartErr)
}

func (s *Sandbox) restartAfterExecFailure(ctx context.Context) error {
	_, stopErr := runChecked(ctx, s.cli, nil, "container", "stop", "--time", "1", s.containerID)
	inspection, inspectErr := inspectContainer(ctx, s.cli, s.containerID)
	if inspectErr != nil {
		return errors.Join(stopErr, fmt.Errorf("inspect stopped Docker task container: %w", inspectErr))
	}
	if inspection.State.Running {
		_, killErr := runChecked(ctx, s.cli, nil, "container", "kill", s.containerID)
		inspection, inspectErr = inspectContainer(ctx, s.cli, s.containerID)
		if inspectErr != nil {
			return errors.Join(stopErr, killErr, fmt.Errorf("inspect killed Docker task container: %w", inspectErr))
		}
		if inspection.State.Running {
			return errors.Join(stopErr, killErr, errors.New("Docker task container remained running after stop and kill"))
		}
	}
	if _, err := runChecked(ctx, s.cli, nil, "container", "start", s.containerID); err != nil {
		return fmt.Errorf("restart Docker task container: %w", err)
	}
	inspection, err := inspectContainer(ctx, s.cli, s.containerID)
	if err != nil {
		return fmt.Errorf("inspect restarted Docker task container: %w", err)
	}
	if !inspection.State.Running {
		return errors.New("inspect restarted Docker task container: container is not running")
	}
	if inspection.Config.WorkingDir != s.workdir ||
		inspection.Config.Labels["aries.run"] != s.runID ||
		inspection.Config.Labels["aries.task"] != s.taskID ||
		inspection.Config.Labels["aries.managed"] != "true" {
		return errors.New("inspect restarted Docker task container: identity or workdir changed")
	}
	if !inspection.hasReadonlyMount(helperContainerPath) || !inspection.hasReadonlyMount(socketContainerDir) {
		return errors.New("inspect restarted Docker task container: trusted helper mounts do not match")
	}
	if _, ok := inspection.NetworkSettings.Networks[s.networkName]; !ok {
		return fmt.Errorf("inspect restarted Docker task container: network %q is not attached", s.networkName)
	}
	network, err := inspectNetwork(ctx, s.cli, s.networkName)
	if err != nil {
		return fmt.Errorf("inspect restarted Docker task network: %w", err)
	}
	if network.Labels["aries.run"] != s.runID || network.Labels["aries.task"] != s.taskID || network.Labels["aries.managed"] != "true" {
		return errors.New("inspect restarted Docker task network: identity labels changed")
	}
	return nil
}

// Upload copies one regular host file to one unambiguous absolute container
// path without a host shell.
func (s *Sandbox) Upload(ctx context.Context, source, destination string) error {
	return s.uploadFile(ctx, source, destination)
}

// Download copies one absolute container file through private staging and
// publishes it beneath the configured output directory without following
// caller-controlled symbolic links.
func (s *Sandbox) Download(ctx context.Context, source, destination string) error {
	return s.downloadFile(ctx, source, destination)
}

// Stop records logs, stops and removes the container, removes its network, and
// positively verifies absence. Concurrent callers share one attempt; a later
// call can retry resources whose removal was not confirmed.
func (s *Sandbox) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil
	}
	if s.stopping {
		done := s.stopDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			err := s.stopErr
			s.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.stopping = true
	s.stopDone = make(chan struct{})
	done := s.stopDone
	s.mu.Unlock()

	err := s.stopOnce(ctx, true)

	s.mu.Lock()
	s.stopErr = err
	s.stopping = false
	s.stopped = !s.containerOwned && !s.networkOwned && !s.runtimeOwned
	close(done)
	s.mu.Unlock()
	return err
}

func (s *Sandbox) stopOnce(ctx context.Context, collectLogs bool) error {
	s.mu.Lock()
	containerOwned := s.containerOwned
	containerRun := s.containerRun
	networkOwned := s.networkOwned
	runtimeOwned := s.runtimeOwned
	s.mu.Unlock()

	var errs []error
	if containerOwned {
		if collectLogs {
			if err := s.collectLogs(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if containerRun {
			if _, err := runChecked(ctx, s.cli, nil, "container", "stop", "--time", strconv.Itoa(stopTimeoutSeconds), s.containerID); err != nil && !isNotFoundError(err) {
				errs = append(errs, fmt.Errorf("stop docker task container: %w", err))
			} else {
				s.mu.Lock()
				s.containerRun = false
				s.mu.Unlock()
			}
		}
		if _, err := runChecked(ctx, s.cli, nil, "container", "rm", "--force", s.containerID); err != nil && !isNotFoundError(err) {
			errs = append(errs, fmt.Errorf("remove docker task container: %w", err))
		}
		absent, err := containerAbsent(ctx, s.cli, s.containerID)
		if err != nil {
			errs = append(errs, fmt.Errorf("confirm docker task container absence: %w", err))
		} else if !absent {
			errs = append(errs, errors.New("confirm docker task container absence: container still exists"))
		} else {
			s.mu.Lock()
			s.containerOwned = false
			s.containerRun = false
			s.mu.Unlock()
		}
	}

	if networkOwned {
		if _, err := runChecked(ctx, s.cli, nil, "network", "rm", s.networkName); err != nil && !isNotFoundError(err) {
			errs = append(errs, fmt.Errorf("remove docker task network: %w", err))
		}
		absent, err := networkAbsent(ctx, s.cli, s.networkName)
		if err != nil {
			errs = append(errs, fmt.Errorf("confirm docker task network absence: %w", err))
		} else if !absent {
			errs = append(errs, errors.New("confirm docker task network absence: network still exists"))
		} else {
			s.mu.Lock()
			s.networkOwned = false
			s.mu.Unlock()
		}
	}

	s.mu.Lock()
	containerOwned = s.containerOwned
	s.mu.Unlock()
	if runtimeOwned && !containerOwned {
		if err := os.RemoveAll(s.runtimeDir); err != nil {
			errs = append(errs, fmt.Errorf("remove private Docker exec runtime: %w", err))
		} else if _, err := os.Lstat(s.runtimeDir); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				errs = append(errs, errors.New("confirm private Docker exec runtime absence: path still exists"))
			} else {
				errs = append(errs, fmt.Errorf("confirm private Docker exec runtime absence: %w", err))
			}
		} else {
			s.mu.Lock()
			s.runtimeOwned = false
			s.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

func (s *Sandbox) rollbackStart(ctx context.Context, primary error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	cleanupErr := s.stopOnce(cleanupCtx, false)
	if cleanupErr == nil {
		return primary
	}
	return errors.Join(primary, fmt.Errorf("rollback partial docker sandbox: %w", cleanupErr))
}

func (s *Sandbox) collectLogs(ctx context.Context) error {
	result, commandErr := s.cli.Run(ctx, nil, "container", "logs", s.containerID)
	var errs []error
	if commandErr != nil && !isNotFoundResult(result) {
		errs = append(errs, fmt.Errorf("collect docker task container logs: %w", commandErr))
	}
	if err := writePrivateFile(filepath.Join(s.artifactDir, "container.stdout.log"), result.stdout); err != nil {
		errs = append(errs, fmt.Errorf("write docker task stdout log: %w", err))
	}
	if err := writePrivateFile(filepath.Join(s.artifactDir, "container.stderr.log"), result.stderr); err != nil {
		errs = append(errs, fmt.Errorf("write docker task stderr log: %w", err))
	}
	return errors.Join(errs...)
}

func containerCreateArgs(request core.SandboxRequest, containerName, networkName, helperPath, runtimeDir string) []string {
	environment := request.Environment
	args := []string{
		"container", "create",
		"--name", containerName,
		"--label", managedLabel,
		"--label", milestoneLabel,
		"--label", "aries.kind=task-container",
		"--label", "aries.run=" + request.RunID,
		"--label", "aries.task=" + request.TaskID,
		"--network", networkName,
		"--network-alias", networkAlias,
		"--workdir", environment.Workdir,
		"--init",
		"--mount", "type=bind,src=" + helperPath + ",dst=" + helperContainerPath + ",readonly",
		"--mount", "type=bind,src=" + runtimeDir + ",dst=" + socketContainerDir + ",readonly",
	}
	if environment.CPU > 0 {
		args = append(args, "--cpus", strconv.FormatFloat(environment.CPU, 'f', -1, 64))
	}
	if environment.MemoryMB > 0 {
		args = append(args, "--memory", strconv.Itoa(environment.MemoryMB)+"m")
	}
	if environment.StorageMB > 0 {
		args = append(args, "--storage-opt", "size="+strconv.Itoa(environment.StorageMB)+"m")
	}
	if environment.GPUs > 0 {
		args = append(args, "--gpus", strconv.Itoa(environment.GPUs))
	}
	for _, key := range sortedKeys(environment.Env) {
		args = append(args, "--env", key+"="+environment.Env[key])
	}
	args = append(args, "--entrypoint", "/bin/sleep", environment.Image, "infinity")
	return args
}

func validateEnvironment(environment core.Environment) error {
	if err := validateImmutableImage(environment.Image); err != nil {
		return fmt.Errorf("invalid docker sandbox image: %w", err)
	}
	if environment.BuildDir != "" {
		return errors.New("docker sandbox BuildDir is unsupported by the MVP; supply a pinned image")
	}
	if _, err := cleanContainerPath(environment.Workdir); err != nil {
		return fmt.Errorf("invalid docker sandbox workdir: %w", err)
	}
	if environment.CPU < 0 || math.IsNaN(environment.CPU) || math.IsInf(environment.CPU, 0) {
		return errors.New("docker sandbox CPU must be finite and nonnegative")
	}
	if environment.MemoryMB < 0 || environment.StorageMB < 0 || environment.GPUs < 0 {
		return errors.New("docker sandbox memory, storage, and GPU counts must be nonnegative")
	}
	for key, value := range environment.Env {
		if !validEnvName(key) {
			return fmt.Errorf("invalid docker sandbox environment name %q", key)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("docker sandbox environment %q contains NUL", key)
		}
	}
	return nil
}

func validateIdentity(kind, value string) error {
	if value == "" || len(value) > 128 {
		return fmt.Errorf("docker sandbox %s ID must contain 1 to 128 characters", kind)
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return fmt.Errorf("docker sandbox %s ID %q contains an unsafe character", kind, value)
	}
	return nil
}

func validateCommand(command core.Command) error {
	if _, err := cleanContainerPath(command.Path); err != nil {
		return fmt.Errorf("invalid command path: %w", err)
	}
	if command.Dir != "" {
		if _, err := cleanContainerPath(command.Dir); err != nil {
			return fmt.Errorf("invalid command workdir: %w", err)
		}
	}
	if command.Timeout < 0 {
		return errors.New("command timeout must be nonnegative")
	}
	for _, argument := range command.Args {
		if strings.ContainsRune(argument, 0) {
			return errors.New("command argument contains NUL")
		}
	}
	for key, value := range command.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid command environment %q", key)
		}
		if strings.HasPrefix(key, "ARIES_") {
			return fmt.Errorf("command environment %q uses the reserved ARIES_ prefix", key)
		}
	}
	return nil
}

func validateImmutableImage(image string) error {
	if strings.ContainsRune(image, 0) || strings.TrimSpace(image) != image || image == "" {
		return errors.New("image reference is empty, padded, or contains NUL")
	}
	digest := ""
	if strings.HasPrefix(image, "sha256:") {
		digest = strings.TrimPrefix(image, "sha256:")
	} else if marker := strings.LastIndex(image, "@sha256:"); marker >= 0 {
		digest = image[marker+len("@sha256:"):]
	}
	if len(digest) != 64 {
		return errors.New("image must use a full immutable sha256 digest")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return errors.New("image sha256 digest is malformed")
	}
	return nil
}

func cleanContainerPath(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !strings.HasPrefix(path, "/") {
		return "", errors.New("path must be absolute, nonempty, and NUL-free")
	}
	clean := filepath.Clean(path)
	if clean != path || clean == "/" {
		return "", errors.New("path must be clean and must not be the container root")
	}
	return clean, nil
}

func inspectContainer(ctx context.Context, cli commandRunner, id string) (containerInspection, error) {
	result, err := runChecked(ctx, cli, nil, "container", "inspect", id)
	if err != nil {
		return containerInspection{}, err
	}
	var inspections []containerInspection
	if err := json.Unmarshal(result.stdout, &inspections); err != nil {
		return containerInspection{}, fmt.Errorf("decode Docker inspection: %w", err)
	}
	if len(inspections) != 1 {
		return containerInspection{}, fmt.Errorf("Docker inspection returned %d records", len(inspections))
	}
	return inspections[0], nil
}

type containerInspection struct {
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		WorkingDir string            `json:"WorkingDir"`
		Labels     map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	} `json:"Mounts"`
}

func (inspection containerInspection) hasReadonlyMount(destination string) bool {
	for _, mount := range inspection.Mounts {
		if mount.Destination == destination && !mount.RW {
			return true
		}
	}
	return false
}

type networkInspection struct {
	Labels map[string]string `json:"Labels"`
}

func inspectNetwork(ctx context.Context, cli commandRunner, name string) (networkInspection, error) {
	result, err := runChecked(ctx, cli, nil, "network", "inspect", name)
	if err != nil {
		return networkInspection{}, err
	}
	var inspections []networkInspection
	if err := json.Unmarshal(result.stdout, &inspections); err != nil {
		return networkInspection{}, fmt.Errorf("decode Docker network inspection: %w", err)
	}
	if len(inspections) != 1 {
		return networkInspection{}, fmt.Errorf("Docker network inspection returned %d records", len(inspections))
	}
	return inspections[0], nil
}

func containerAbsent(ctx context.Context, cli commandRunner, id string) (bool, error) {
	result, err := cli.Run(ctx, nil, "container", "inspect", id)
	if err == nil {
		return false, nil
	}
	if isNotFoundResult(result) {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	return false, fmt.Errorf("docker container inspect failed: %w: %s", err, compactStderr(result.stderr))
}

func networkAbsent(ctx context.Context, cli commandRunner, name string) (bool, error) {
	result, err := cli.Run(ctx, nil, "network", "inspect", name)
	if err == nil {
		return false, nil
	}
	if isNotFoundResult(result) {
		return true, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	return false, fmt.Errorf("docker network inspect failed: %w: %s", err, compactStderr(result.stderr))
}

func runChecked(ctx context.Context, cli commandRunner, stdin []byte, args ...string) (commandResult, error) {
	result, err := cli.Run(ctx, stdin, args...)
	if err == nil {
		return result, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	return result, fmt.Errorf("docker %s: %w: %s", strings.Join(commandSummary(args), " "), err, compactStderr(result.stderr))
}

func (r execRunner) Run(ctx context.Context, stdin []byte, args ...string) (commandResult, error) {
	socket := r.socket
	if socket == "" {
		socket = defaultDockerSocket
	}
	dockerArgs := append([]string{"--host", "unix://" + socket}, args...)
	command := exec.CommandContext(ctx, r.binary, dockerArgs...)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: exitCode}, err
}

func randomID() (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func validEnvName(value string) bool {
	for index, r := range value {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return value != ""
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if resolved != abs {
		return errors.New("directory path contains a symbolic link")
	}
	return nil
}

func stageExecHelper(source, artifactDir string) (string, error) {
	sourceFile, before, err := openRegularNoFollow(source)
	if err != nil {
		return "", err
	}
	defer sourceFile.Close()
	if before.Mode().Perm()&0o111 == 0 {
		return "", errors.New("Docker exec helper is not executable")
	}
	helperDir := filepath.Join(artifactDir, "helper")
	if err := ensureDirectory(helperDir); err != nil {
		return "", err
	}
	helperPath := filepath.Join(helperDir, "aries-exec-helper")
	staged, err := os.OpenFile(helperPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o555)
	if err != nil {
		return "", err
	}
	remove := true
	defer func() {
		_ = staged.Close()
		if remove {
			_ = os.Remove(helperPath)
		}
	}()
	copied, err := io.Copy(staged, sourceFile)
	if err != nil {
		return "", err
	}
	after, err := sourceFile.Stat()
	if err != nil {
		return "", err
	}
	if !stableFile(before, after, copied) {
		return "", errors.New("Docker exec helper changed while being staged")
	}
	if err := staged.Sync(); err != nil {
		return "", err
	}
	if err := staged.Close(); err != nil {
		return "", err
	}
	remove = false
	return helperPath, nil
}

func writePrivateFile(path string, content []byte) error {
	if err := ensureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o600)
}

func isNotFoundResult(result commandResult) bool {
	message := strings.ToLower(string(result.stderr))
	return strings.Contains(message, "no such object") ||
		strings.Contains(message, "no such container") ||
		strings.Contains(message, "no such network") ||
		strings.Contains(message, "not found")
}

func isNotFoundError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such")
}

func compactStderr(stderr []byte) string {
	message := strings.TrimSpace(string(stderr))
	if len(message) > 512 {
		message = message[:512] + "..."
	}
	return message
}

func commandSummary(args []string) []string {
	if len(args) <= 3 {
		return args
	}
	return args[:3]
}
