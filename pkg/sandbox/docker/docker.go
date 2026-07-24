package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/hyscale-lab/aries/pkg/core"
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
	maxExecInput          = 16 << 20
	maxExecOutput         = 16 << 20
	networkAlias          = "task-sandbox"
	execPollInterval      = 20 * time.Millisecond
	execDrainTimeout      = 200 * time.Millisecond
	execTrailerKeep       = 128
	execStatePrefix       = "/tmp/.aries-exec-"
	execShell             = `state=$1; token=$2; shift 2; umask 077; trap 'rm -f "$state" "$state.tmp"' EXIT; exec 3<&0; setsid "$@" <&3 & pid=$!; printf '%s\n' "$pid" >"$state.tmp" || exit 125; mv "$state.tmp" "$state" || exit 125; wait "$pid"; status=$?; rm -f "$state" "$state.tmp"; trap - EXIT; printf '\036ARIES_EXEC_EXIT_%s=%d\037' "$token" "$status" >&2; exit "$status"`
	cancelExecShell       = `state=$1; attempts=0; while [ ! -r "$state" ]; do attempts=$((attempts+1)); [ "$attempts" -ge 200 ] && exit 70; sleep 0.01; done; IFS= read -r pgid <"$state" || exit 71; case "$pgid" in ''|*[!0-9]*|0|1) exit 71;; esac; kill -TERM "-$pgid" 2>/dev/null || :; sleep 0.2; kill -KILL "-$pgid" 2>/dev/null || :; rm -f "$state"; exit 0`
)

var (
	_ runner.ToolSandbox = (*Manager)(nil)
	_ runner.Sandbox     = (*Sandbox)(nil)
)

// Options are the host-local inputs to the Docker sandbox manager.
type Options struct {
	OutputDir      string
	DockerSocket   string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// dockerClient is the small Engine surface used by this package. The official
// client implements it directly; tests use a compact fake.
type dockerClient interface {
	NetworkCreate(context.Context, string, client.NetworkCreateOptions) (client.NetworkCreateResult, error)
	NetworkInspect(context.Context, string, client.NetworkInspectOptions) (client.NetworkInspectResult, error)
	NetworkRemove(context.Context, string, client.NetworkRemoveOptions) (client.NetworkRemoveResult, error)
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerTop(context.Context, string, client.ContainerTopOptions) (client.ContainerTopResult, error)
	ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecStart(context.Context, string, client.ExecStartOptions) (client.ExecStartResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	CopyToContainer(context.Context, string, client.CopyToContainerOptions) (client.CopyToContainerResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
}

// Manager starts one isolated Docker container and network per task.
type Manager struct {
	client         dockerClient
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	newID          func() (string, error)
}

// Sandbox is a live Docker task environment.
type Sandbox struct {
	owner          *Manager
	client         dockerClient
	containerID    string
	containerName  string
	networkName    string
	workdir        string
	artifactDir    string
	outputDir      string
	cleanupTimeout time.Duration
	runID          string
	taskID         string

	mu             sync.Mutex
	containerOwned bool
	networkOwned   bool
	stopped        bool
	stopping       bool
	stopDone       chan struct{}
	stopErr        error
}

// New constructs a Docker manager without contacting the daemon.
func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("docker sandbox output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve docker sandbox output directory: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create docker sandbox output directory: %w", err)
	}
	if options.DockerSocket == "" {
		options.DockerSocket = defaultDockerSocket
	}
	host := options.DockerSocket
	if !strings.Contains(host, "://") {
		host = "unix://" + host
	}
	api, err := client.New(client.WithHost(host), client.WithUserAgent("aries-sandbox/1"))
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		client:         api,
		outputDir:      outputDir,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
		newID:          randomID,
	}, nil
}

// Start creates and positively inspects one task container and network.
func (m *Manager) Start(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	if err := validateIdentity("run", request.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", request.TaskID); err != nil {
		return nil, err
	}
	if err := validateEnvironment(request.Environment); err != nil {
		return nil, err
	}
	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("generate docker sandbox ID: %w", err)
	}
	sandbox := &Sandbox{
		owner:          m,
		client:         m.client,
		containerName:  "aries-task-" + id,
		networkName:    "aries-net-" + id,
		workdir:        request.Environment.Workdir,
		artifactDir:    filepath.Join(m.outputDir, request.TaskID, "sandbox"),
		outputDir:      m.outputDir,
		cleanupTimeout: m.cleanupTimeout,
		runID:          request.RunID,
		taskID:         request.TaskID,
	}
	if err := os.MkdirAll(sandbox.artifactDir, 0o700); err != nil {
		return nil, fmt.Errorf("create docker sandbox artifact directory: %w", err)
	}

	networkLabels := ownershipLabels(request, "task-network")
	if _, err := m.client.NetworkCreate(ctx, sandbox.networkName, client.NetworkCreateOptions{
		Internal: !request.Environment.AllowNetwork,
		Labels:   networkLabels,
	}); err != nil {
		return nil, fmt.Errorf("create docker task network: %w", err)
	}
	sandbox.networkOwned = true

	created, err := m.client.ContainerCreate(ctx, containerOptions(request, sandbox, ownershipLabels(request, "task-container")))
	if err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("create docker task container: %w", err))
	}
	if strings.TrimSpace(created.ID) == "" {
		return nil, sandbox.rollbackStart(ctx, errors.New("create docker task container: Docker returned an empty container ID"))
	}
	sandbox.containerID = created.ID
	sandbox.containerOwned = true
	if _, err := m.client.ContainerStart(ctx, sandbox.containerID, client.ContainerStartOptions{}); err != nil {
		return nil, sandbox.rollbackStart(ctx, fmt.Errorf("start docker task container: %w", err))
	}
	if err := sandbox.verifyLive(ctx); err != nil {
		return nil, sandbox.rollbackStart(ctx, err)
	}
	m.logger.WithContext(ctx).WithFields(logrus.Fields{"container": sandbox.containerName, "network": sandbox.networkName}).Info("docker task sandbox started")
	return sandbox, nil
}

// Stop releases a sandbox created by this manager.
func (m *Manager) Stop(ctx context.Context, live runner.Sandbox) error {
	if live == nil {
		return errors.New("stop Docker sandbox: sandbox is required")
	}
	sandbox, ok := live.(*Sandbox)
	if !ok || sandbox == nil {
		return fmt.Errorf("stop Docker sandbox: unsupported sandbox type %T", live)
	}
	if sandbox.owner != m {
		return errors.New("stop Docker sandbox: sandbox belongs to another manager")
	}
	return sandbox.stop(ctx)
}

func ownershipLabels(request core.SandboxRequest, kind string) map[string]string {
	labels := map[string]string{
		"aries.managed": "true",
		"aries.kind":    kind,
		"aries.run":     request.RunID,
		"aries.task":    request.TaskID,
	}
	if kind == "task-container" {
		labels["aries.component"] = "sandbox"
	}
	return labels
}

func containerOptions(request core.SandboxRequest, sandbox *Sandbox, labels map[string]string) client.ContainerCreateOptions {
	environment := request.Environment
	resources := container.Resources{
		NanoCPUs: int64(environment.CPU * 1e9),
		Memory:   int64(environment.MemoryMB) << 20,
	}
	if environment.GPUs > 0 {
		resources.DeviceRequests = []container.DeviceRequest{{
			Driver: "nvidia", Count: environment.GPUs, Capabilities: [][]string{{"gpu"}},
		}}
	}
	host := &container.HostConfig{
		NetworkMode: container.NetworkMode(sandbox.networkName),
		Resources:   resources,
		Init:        boolPointer(true),
	}
	if environment.StorageMB > 0 {
		host.StorageOpt = map[string]string{"size": fmt.Sprintf("%dm", environment.StorageMB)}
	}
	return client.ContainerCreateOptions{
		Name: sandbox.containerName,
		Config: &container.Config{
			Image: environment.Image, WorkingDir: environment.Workdir, Env: taskDockerEnvironment(environment.Env),
			Entrypoint: []string{"/bin/sleep"}, Cmd: []string{"infinity"}, Labels: labels,
		},
		HostConfig: host,
		NetworkingConfig: &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			sandbox.networkName: {Aliases: []string{networkAlias}},
		}},
	}
}

func boolPointer(value bool) *bool { return &value }

func (s *Sandbox) verifyLive(ctx context.Context) error {
	inspection, err := s.client.ContainerInspect(ctx, s.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect started docker task container: %w", err)
	}
	c := inspection.Container
	if c.State == nil || !c.State.Running {
		return errors.New("inspect started docker task container: container is not running")
	}
	if c.Config == nil || c.Config.WorkingDir != s.workdir || !sameIdentity(c.Config.Labels, s.runID, s.taskID) {
		return errors.New("inspect started docker task container: identity or workdir does not match")
	}
	if c.NetworkSettings == nil || c.NetworkSettings.Networks[s.networkName] == nil {
		return fmt.Errorf("inspect started docker task container: network %q is not attached", s.networkName)
	}
	inspectedNetwork, err := s.client.NetworkInspect(ctx, s.networkName, client.NetworkInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect started docker task network: %w", err)
	}
	if !sameIdentity(inspectedNetwork.Network.Labels, s.runID, s.taskID) {
		return errors.New("inspect started docker task network: identity labels do not match")
	}
	return nil
}

func sameIdentity(labels map[string]string, runID, taskID string) bool {
	return labels["aries.managed"] == "true" && labels["aries.run"] == runID && labels["aries.task"] == taskID
}

// ContainerID returns the immutable Docker container identifier.
func (s *Sandbox) ContainerID() string { return s.containerID }

// ContainerName returns the generated task container name.
func (s *Sandbox) ContainerName() string { return s.containerName }

// NetworkName returns the task-scoped Docker network.
func (s *Sandbox) NetworkName() string { return s.networkName }

// Workdir returns the benchmark-declared container working directory.
func (s *Sandbox) Workdir() string { return s.workdir }

// RunID returns the owning experiment run identity for bridge tool logs.
func (s *Sandbox) RunID() string { return s.runID }

// TaskID returns the owning benchmark task identity for bridge tool logs.
func (s *Sandbox) TaskID() string { return s.taskID }

// NetworkGateway returns the IPv4 gateway of the task-scoped network.
func (s *Sandbox) NetworkGateway(ctx context.Context) (string, error) {
	result, err := s.client.NetworkInspect(ctx, s.networkName, client.NetworkInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect Docker task network gateway: %w", err)
	}
	if !sameIdentity(result.Network.Labels, s.runID, s.taskID) {
		return "", errors.New("inspect Docker task network gateway: identity labels do not match")
	}
	for _, config := range result.Network.IPAM.Config {
		if config.Gateway.Is4() {
			return config.Gateway.String(), nil
		}
	}
	return "", errors.New("inspect Docker task network gateway: no IPv4 gateway")
}

// Exec runs one argv directly through Docker's typed exec API. Nonzero exits
// are returned as results, not transport errors.
func (s *Sandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if len(command.Stdin) > maxExecInput {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("Docker exec stdin exceeds %d bytes", maxExecInput)
	}
	var stdout, stderr bytes.Buffer
	var stdin io.Reader
	if len(command.Stdin) > 0 {
		stdin = bytes.NewReader(command.Stdin)
	}
	result, err := s.ExecStream(ctx, command, stdin,
		&limitedWriter{writer: &stdout, limit: maxExecOutput},
		&limitedWriter{writer: &stderr, limit: maxExecOutput},
	)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return result, err
}

// ExecStream is the bridge-facing streaming form of Exec. It starts reading
// output while stdin is still arriving, so interactive SSH commands cannot
// deadlock on full pipes.
func (s *Sandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	failure := func() core.CommandResult { return core.CommandResult{ExitCode: -1, Duration: time.Since(started)} }
	if err := validateCommand(command); err != nil {
		return failure(), err
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	attachInput := stdin != nil
	if !attachInput {
		stdin = bytes.NewReader(nil)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	execCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()

	token, err := randomID()
	if err != nil {
		return failure(), fmt.Errorf("generate Docker exec exit token: %w", err)
	}
	statePath := execStatePrefix + token
	created, err := s.client.ExecCreate(execCtx, s.containerID, client.ExecCreateOptions{
		AttachStdin: attachInput, AttachStdout: true, AttachStderr: true,
		Cmd: wrappedCommand(statePath, token, command),
		Env: dockerEnvironment(command.Env), WorkingDir: command.Dir,
	})
	if err != nil {
		return failure(), fmt.Errorf("create Docker exec: %w", err)
	}
	attached, err := s.client.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return failure(), fmt.Errorf("attach Docker exec: %w", err)
	}
	defer attached.Close()

	readDone := make(chan struct{})
	go func() {
		select {
		case <-execCtx.Done():
			attached.Close()
		case <-readDone:
		}
	}()
	var closeWrite sync.Once
	closeDockerInput := func() { closeWrite.Do(func() { _ = attached.CloseWrite() }) }
	writeDone := make(chan error, 1)
	go func() {
		limited := io.LimitReader(stdin, maxExecInput+1)
		written, writeErr := io.Copy(attached.Conn, limited)
		if writeErr == nil && written > maxExecInput {
			writeErr = fmt.Errorf("Docker exec stdin exceeds %d bytes", maxExecInput)
		}
		closeDockerInput()
		writeDone <- writeErr
	}()

	copyDone := make(chan error, 1)
	exitTrailer := newExitTrailer(&limitedWriter{writer: stderr, limit: maxExecOutput}, token)
	go func() {
		_, copyErr := stdcopy.StdCopy(
			&limitedWriter{writer: stdout, limit: maxExecOutput},
			exitTrailer,
			attached.Reader,
		)
		copyDone <- copyErr
	}()
	exitDone := make(chan error, 1)
	go func() { exitDone <- s.waitForExecExit(execCtx, created.ID) }()
	var stopRead sync.Once
	stopReading := func() {
		stopRead.Do(func() {
			close(readDone)
			attached.Close()
		})
	}
	abort := func(cause error) (core.CommandResult, error) {
		stopReading()
		// Closing the attach can make a stream-copy error race with cancellation.
		// Cancellation owns the result once it is observable: the caller needs to
		// know whether targeted termination was confirmed, not which attach error
		// happened to win the select.
		contextErr := execCtx.Err()
		terminateCtx, terminateCancel := context.WithTimeout(context.WithoutCancel(execCtx), s.cleanupTimeout)
		defer terminateCancel()
		terminateErr := s.terminateExec(terminateCtx, created.ID, statePath)
		if contextErr != nil {
			if terminateErr == nil {
				return failure(), contextErr
			}
			return failure(), errors.Join(contextErr, terminateErr)
		}
		if terminateErr == nil {
			return failure(), cause
		}
		return failure(), errors.Join(cause, terminateErr)
	}

	copyFinished := false
	var copyErr error
	var observedErr error
	waiting := true
	for waiting {
		select {
		case <-execCtx.Done():
			return abort(execCtx.Err())
		case err := <-copyDone:
			copyFinished, copyErr = true, err
			if err != nil {
				return abort(err)
			}
		case observedErr = <-exitDone:
			waiting = false
		}
	}
	if observedErr != nil {
		return abort(observedErr)
	}
	closeDockerInput()
	forcedClose := false
	if !copyFinished {
		select {
		case copyErr = <-copyDone:
			copyFinished = true
		case <-time.After(execDrainTimeout):
			forcedClose = true
			attached.Close()
			copyErr = <-copyDone
			copyFinished = true
		}
	}
	stopReading()
	if copyErr != nil && !forcedClose {
		return abort(copyErr)
	}
	select {
	case writeErr := <-writeDone:
		if writeErr != nil && !forcedClose {
			return abort(writeErr)
		}
	default:
	}
	exitCode, err := exitTrailer.Finish()
	if err != nil {
		return abort(err)
	}
	return core.CommandResult{
		ExitCode: exitCode,
		Duration: time.Since(started),
	}, nil
}

func wrappedCommand(statePath, token string, command core.Command) []string {
	arguments := []string{"/bin/sh", "-c", execShell, "aries-exec", statePath, token, command.Path}
	return append(arguments, command.Args...)
}

type exitTrailerWriter struct {
	destination io.Writer
	prefix      []byte
	buffer      bytes.Buffer
}

func newExitTrailer(destination io.Writer, token string) *exitTrailerWriter {
	return &exitTrailerWriter{
		destination: destination,
		prefix:      []byte("\x1eARIES_EXEC_EXIT_" + token + "="),
	}
}

func (w *exitTrailerWriter) Write(content []byte) (int, error) {
	written, _ := w.buffer.Write(content)
	if excess := w.buffer.Len() - execTrailerKeep; excess > 0 {
		chunk := w.buffer.Next(excess)
		if n, err := w.destination.Write(chunk); err != nil {
			return 0, err
		} else if n != len(chunk) {
			return 0, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *exitTrailerWriter) Finish() (int, error) {
	content := w.buffer.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\x1f' {
		return -1, errors.New("Docker exec output is missing its exit trailer")
	}
	start := bytes.LastIndex(content[:len(content)-1], w.prefix)
	if start < 0 {
		return -1, errors.New("Docker exec output has an invalid exit trailer")
	}
	codeBytes := content[start+len(w.prefix) : len(content)-1]
	exitCode, err := strconv.Atoi(string(codeBytes))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return -1, errors.New("Docker exec output has an invalid exit code")
	}
	if _, err := w.destination.Write(content[:start]); err != nil {
		return -1, fmt.Errorf("write Docker exec stderr: %w", err)
	}
	return exitCode, nil
}

func (s *Sandbox) waitForExecExit(ctx context.Context, execID string) error {
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		inspection, err := s.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect running Docker exec: %w", err)
		}
		if !inspection.Running {
			return nil
		}
		if inspection.PID > 0 {
			present, err := s.containerHasPID(ctx, inspection.PID)
			if err != nil {
				return fmt.Errorf("inspect Docker exec process: %w", err)
			}
			if !present {
				// Docker 29 can keep ExecInspect.Running true until a hijacked
				// attach is closed even after the process has exited.
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Sandbox) containerHasPID(ctx context.Context, pid int) (bool, error) {
	top, err := s.client.ContainerTop(ctx, s.containerID, client.ContainerTopOptions{Arguments: []string{"-eo", "pid"}})
	if err != nil {
		return false, err
	}
	pidColumn := -1
	for index, title := range top.Titles {
		if strings.EqualFold(title, "PID") {
			pidColumn = index
			break
		}
	}
	if pidColumn < 0 {
		return false, errors.New("Docker top response has no PID column")
	}
	want := strconv.Itoa(pid)
	for _, process := range top.Processes {
		if pidColumn < len(process) && process[pidColumn] == want {
			return true, nil
		}
	}
	return false, nil
}

func (s *Sandbox) terminateExec(ctx context.Context, execID, statePath string) error {
	inspection, err := s.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect Docker exec before termination: %w", err)
	}
	if !inspection.Running {
		return nil
	}
	if inspection.PID <= 0 {
		return errors.New("inspect Docker exec before termination: running exec has no PID")
	}
	processGroup, present, err := s.findExecProcessGroup(ctx, inspection.PID)
	if err != nil {
		return fmt.Errorf("locate Docker exec process group: %w", err)
	}
	if !present {
		return nil
	}
	created, err := s.client.ExecCreate(ctx, s.containerID, client.ExecCreateOptions{
		Cmd: []string{"/bin/sh", "-c", cancelExecShell, "aries-cancel", statePath},
	})
	if err != nil {
		return fmt.Errorf("create Docker exec termination helper: %w", err)
	}
	if _, err := s.client.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("start Docker exec termination helper: %w", err)
	}
	helper, err := s.client.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect Docker exec termination helper: %w", err)
	}
	if helper.Running && helper.PID <= 0 {
		return errors.New("inspect Docker exec termination helper: running helper has no PID")
	}
	if err := s.waitForProcessAbsence(ctx, inspection.PID, processGroup, helper.PID); err != nil {
		return fmt.Errorf("confirm terminated Docker exec process-group exit: %w", err)
	}
	return nil
}

func (s *Sandbox) findExecProcessGroup(ctx context.Context, wrapperPID int) (int, bool, error) {
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		table, err := s.processTable(ctx)
		if err != nil {
			return 0, false, err
		}
		if !table.hasPID(wrapperPID) {
			return 0, false, nil
		}
		for _, process := range table.processes {
			if process.ppid == wrapperPID && process.pgid > 1 {
				return process.pgid, true, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Sandbox) waitForProcessAbsence(ctx context.Context, wrapperPID, processGroup, helperPID int) error {
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		table, err := s.processTable(ctx)
		if err != nil {
			return err
		}
		if !table.hasPID(wrapperPID) && !table.hasGroup(processGroup) && (helperPID <= 0 || !table.hasPID(helperPID)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type containerProcess struct {
	pid  int
	ppid int
	pgid int
}

type processTable struct{ processes []containerProcess }

func (s *Sandbox) processTable(ctx context.Context) (processTable, error) {
	top, err := s.client.ContainerTop(ctx, s.containerID, client.ContainerTopOptions{Arguments: []string{"-eo", "pid,ppid,pgid"}})
	if err != nil {
		return processTable{}, err
	}
	columns := map[string]int{}
	for index, title := range top.Titles {
		columns[strings.ToUpper(title)] = index
	}
	for _, name := range []string{"PID", "PPID", "PGID"} {
		if _, ok := columns[name]; !ok {
			return processTable{}, fmt.Errorf("Docker top response has no %s column", name)
		}
	}
	result := processTable{processes: make([]containerProcess, 0, len(top.Processes))}
	for _, row := range top.Processes {
		if columns["PID"] >= len(row) || columns["PPID"] >= len(row) || columns["PGID"] >= len(row) {
			return processTable{}, errors.New("Docker top response contains a short process row")
		}
		pid, pidErr := strconv.Atoi(row[columns["PID"]])
		ppid, ppidErr := strconv.Atoi(row[columns["PPID"]])
		pgid, pgidErr := strconv.Atoi(row[columns["PGID"]])
		if pidErr != nil || ppidErr != nil || pgidErr != nil {
			return processTable{}, errors.New("Docker top response contains a nonnumeric process identity")
		}
		result.processes = append(result.processes, containerProcess{pid: pid, ppid: ppid, pgid: pgid})
	}
	return result, nil
}

func (t processTable) hasPID(pid int) bool {
	return slices.ContainsFunc(t.processes, func(process containerProcess) bool { return process.pid == pid })
}

func (t processTable) hasGroup(pgid int) bool {
	return slices.ContainsFunc(t.processes, func(process containerProcess) bool { return process.pgid == pgid })
}

func intPointer(value int) *int { return &value }

type limitedWriter struct {
	writer io.Writer
	wrote  int
	limit  int
}

func (w *limitedWriter) Write(content []byte) (int, error) {
	if len(content) > w.limit-w.wrote {
		return 0, fmt.Errorf("Docker exec output exceeds %d bytes", w.limit)
	}
	written, err := w.writer.Write(content)
	w.wrote += written
	return written, err
}

// Upload copies one regular host file to an absolute container path.
func (s *Sandbox) Upload(ctx context.Context, source, destination string) error {
	destination, err := cleanContainerPath(destination)
	if err != nil {
		return fmt.Errorf("invalid Docker upload destination: %w", err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("stat Docker upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("Docker upload source must be a regular file")
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open Docker upload source: %w", err)
	}
	defer file.Close()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: filepath.Base(destination), Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return fmt.Errorf("archive Docker upload: %w", err)
	}
	if _, err := io.Copy(writer, file); err != nil {
		return fmt.Errorf("archive Docker upload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close Docker upload archive: %w", err)
	}
	_, err = s.client.CopyToContainer(ctx, s.containerID, client.CopyToContainerOptions{
		DestinationPath: filepath.Dir(destination), Content: bytes.NewReader(archive.Bytes()),
	})
	if err != nil {
		return fmt.Errorf("upload file to Docker task container: %w", err)
	}
	return nil
}

// Download copies one regular container file beneath the configured output directory.
func (s *Sandbox) Download(ctx context.Context, source, destination string) error {
	source, err := cleanContainerPath(source)
	if err != nil {
		return fmt.Errorf("invalid Docker download source: %w", err)
	}
	destination, err = outputPath(s.outputDir, destination)
	if err != nil {
		return err
	}
	result, err := s.client.CopyFromContainer(ctx, s.containerID, client.CopyFromContainerOptions{SourcePath: source})
	if err != nil {
		return fmt.Errorf("download file from Docker task container: %w", err)
	}
	defer result.Content.Close()
	if !result.Stat.Mode.IsRegular() {
		return errors.New("Docker download source must be a regular file")
	}
	reader := tar.NewReader(result.Content)
	var header *tar.Header
	for {
		header, err = reader.Next()
		if err == nil && header.FileInfo().Mode().IsRegular() {
			break
		}
		if errors.Is(err, io.EOF) {
			return errors.New("Docker download archive contains no regular file")
		}
		if err != nil {
			return fmt.Errorf("read Docker download archive: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create Docker download directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".aries-download-*")
	if err != nil {
		return fmt.Errorf("create Docker download destination: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.CopyN(temporary, reader, header.Size); err != nil {
		temporary.Close()
		return fmt.Errorf("write Docker download: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure Docker download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Docker download: %w", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("publish Docker download: %w", err)
	}
	return nil
}

// stop records logs and removes the task container and network. Concurrent and
// repeated calls are safe; failed removals can be retried.
func (s *Sandbox) stop(ctx context.Context) error {
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
	s.stopped = !s.containerOwned && !s.networkOwned
	close(done)
	s.mu.Unlock()
	return err
}

func (s *Sandbox) stopOnce(ctx context.Context, collectLogs bool) error {
	s.mu.Lock()
	containerOwned, networkOwned := s.containerOwned, s.networkOwned
	s.mu.Unlock()
	var errs []error
	if containerOwned {
		if collectLogs {
			errs = append(errs, s.collectLogs(ctx))
		}
		_, err := s.client.ContainerStop(ctx, s.containerID, client.ContainerStopOptions{Timeout: intPointer(5)})
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("stop docker task container: %w", err))
		}
		_, err = s.client.ContainerRemove(ctx, s.containerID, client.ContainerRemoveOptions{Force: true})
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove docker task container: %w", err))
		}
		_, err = s.client.ContainerInspect(ctx, s.containerID, client.ContainerInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			s.mu.Lock()
			s.containerOwned = false
			s.mu.Unlock()
		} else if err == nil {
			errs = append(errs, errors.New("confirm docker task container absence: container still exists"))
		} else {
			errs = append(errs, fmt.Errorf("confirm docker task container absence: %w", err))
		}
	}
	if networkOwned {
		_, err := s.client.NetworkRemove(ctx, s.networkName, client.NetworkRemoveOptions{})
		if err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("remove docker task network: %w", err))
		}
		_, err = s.client.NetworkInspect(ctx, s.networkName, client.NetworkInspectOptions{})
		if cerrdefs.IsNotFound(err) {
			s.mu.Lock()
			s.networkOwned = false
			s.mu.Unlock()
		} else if err == nil {
			errs = append(errs, errors.New("confirm docker task network absence: network still exists"))
		} else {
			errs = append(errs, fmt.Errorf("confirm docker task network absence: %w", err))
		}
	}
	return errors.Join(errs...)
}

func (s *Sandbox) rollbackStart(ctx context.Context, primary error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	if cleanupErr := s.stopOnce(cleanupCtx, false); cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("rollback partial docker sandbox: %w", cleanupErr))
	}
	return primary
}

func (s *Sandbox) collectLogs(ctx context.Context) error {
	stdout, err := os.OpenFile(filepath.Join(s.artifactDir, "container.stdout.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create docker task stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(s.artifactDir, "container.stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create docker task stderr log: %w", err)
	}
	defer stderr.Close()
	stream, err := s.client.ContainerLogs(ctx, s.containerID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("collect docker task container logs: %w", err)
	}
	defer stream.Close()
	if _, err := stdcopy.StdCopy(stdout, stderr, stream); err != nil {
		return fmt.Errorf("demultiplex docker task container logs: %w", err)
	}
	return nil
}

func outputPath(root, destination string) (string, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve Docker download destination: %w", err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("Docker download destination is outside the configured output directory")
	}
	return absolute, nil
}

func dockerEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		result = append(result, key+"="+values[key])
	}
	return result
}

func taskDockerEnvironment(values map[string]string) []string {
	owned := maps.Clone(values)
	if owned == nil {
		owned = make(map[string]string, 2)
	}
	timezone := os.Getenv("TZ")
	if timezone == "" {
		timezone = "UTC"
	}
	owned["TZ"] = timezone
	owned["DEBIAN_FRONTEND"] = "noninteractive"
	return dockerEnvironment(owned)
}

func validateEnvironment(environment core.Environment) error {
	if err := validatePullImage(environment.Image); err != nil {
		return fmt.Errorf("invalid docker sandbox image: %w", err)
	}
	if _, err := cleanContainerWorkdir(environment.Workdir); err != nil {
		return fmt.Errorf("invalid docker sandbox workdir: %w", err)
	}
	if environment.CPU < 0 || math.IsNaN(environment.CPU) || math.IsInf(environment.CPU, 0) || environment.CPU*1e9 >= math.Exp2(63) {
		return errors.New("docker sandbox CPU must be finite, nonnegative, and convert to NanoCPUs below 2^63")
	}
	if environment.MemoryMB < 0 || int64(environment.MemoryMB) > math.MaxInt64>>20 || environment.StorageMB < 0 || environment.GPUs < 0 {
		return errors.New("docker sandbox memory, storage, and GPU counts must be nonnegative")
	}
	for key, value := range environment.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid docker sandbox environment %q", key)
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
		if _, err := cleanContainerWorkdir(command.Dir); err != nil {
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
	}
	return nil
}

func cleanContainerPath(path string) (string, error) {
	clean, err := cleanContainerWorkdir(path)
	if err != nil {
		return "", err
	}
	if clean == "/" {
		return "", errors.New("path must not be the container root")
	}
	return clean, nil
}

func cleanContainerWorkdir(path string) (string, error) {
	if path == "" || strings.ContainsRune(path, 0) || !strings.HasPrefix(path, "/") {
		return "", errors.New("path must be absolute, nonempty, and NUL-free")
	}
	clean := filepath.Clean(path)
	if clean != path {
		return "", errors.New("path must be clean")
	}
	return clean, nil
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

func randomID() (string, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
