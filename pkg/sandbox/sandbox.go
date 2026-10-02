package sandbox

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
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/containerimage"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultCleanupTimeout = 30 * time.Second
	maxExecInput          = 16 << 20
	maxConfiguredOutput   = 1 << 30
	networkAlias          = "task-sandbox"
)

var (
	_ runner.ToolSandbox       = (*Manager)(nil)
	_ runner.Sandbox           = (*Sandbox)(nil)
	_ runner.LimitedDownloader = (*Sandbox)(nil)
	_ runner.StreamExecutor    = (*Sandbox)(nil)
)

// Options configures benchmark policy and the shared runtime deployment.
// New takes ownership of Deployment after successful construction.
type Options struct {
	OutputDir      string
	Deployment     deployment.Deployment
	NewEnvironment func() deployment.TaskEnvironment
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

type Manager struct {
	deployment     deployment.Deployment
	newEnvironment func() deployment.TaskEnvironment
	mu             sync.Mutex
	active         map[*Sandbox]struct{}
	pending        map[*Sandbox]struct{}
	closeMu        sync.Mutex
	outputDir      string
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	newID          func() (string, error)
	closeOnce      sync.Once
	closeErr       error
}

// Sandbox applies benchmark policy to a live deployment.
type Sandbox struct {
	owner          *Manager
	deployment     deployment.Deployment
	containerID    string
	containerName  string
	networkName    string
	environment    deployment.TaskEnvironment
	workdir        string
	execUser       string
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

// Close releases the deployment transport after all sandboxes have stopped.
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.mu.Lock()
	pending := make([]*Sandbox, 0, len(m.pending))
	for s := range m.pending {
		pending = append(pending, s)
	}
	m.mu.Unlock()
	var cleanupErrors []error
	for _, s := range pending {
		ctx, cancel := context.WithTimeout(context.Background(), m.cleanupTimeout)
		err := s.stopOnce(ctx, false)
		cancel()
		if err != nil {
			cleanupErrors = append(cleanupErrors, err)
			continue
		}
		m.mu.Lock()
		delete(m.pending, s)
		m.mu.Unlock()
	}
	if err := errors.Join(cleanupErrors...); err != nil {
		return err
	}
	m.closeOnce.Do(func() { m.closeErr = m.deployment.Close() })
	return m.closeErr
}

// New constructs a manager without contacting the deployment backend.
func New(options Options) (*Manager, error) {
	if options.Deployment == nil {
		return nil, errors.New("sandbox deployment is required")
	}
	if options.NewEnvironment == nil {
		return nil, errors.New("sandbox task environment constructor is required")
	}
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("sandbox output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox output directory: %w", err)
	}
	if err = os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox output directory: %w", err)
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		deployment:     options.Deployment,
		newEnvironment: options.NewEnvironment,
		active:         make(map[*Sandbox]struct{}),
		pending:        make(map[*Sandbox]struct{}),
		outputDir:      outputDir,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
		newID:          randomID,
	}, nil
}

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
		return nil, fmt.Errorf("generate sandbox ID: %w", err)
	}
	s := &Sandbox{
		owner: m, deployment: m.deployment,
		containerName: "aries-task-" + id,
		workdir:       request.Environment.Workdir, execUser: request.Environment.ExecUser,
		artifactDir: filepath.Join(m.outputDir, request.TaskID, "sandbox"),
		outputDir:   m.outputDir, cleanupTimeout: m.cleanupTimeout,
		runID: request.RunID, taskID: request.TaskID,
	}
	if err := os.MkdirAll(s.artifactDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox artifact directory: %w", err)
	}
	s.environment = m.newEnvironment()
	if s.environment == nil {
		return nil, errors.New("sandbox task environment constructor returned nil")
	}
	s.networkOwned = true
	s.networkName, err = s.environment.Start(ctx, request)
	if err != nil {
		return nil, s.rollbackStart(ctx, fmt.Errorf("start task environment: %w", err))
	}
	if s.networkName == "" {
		return nil, s.rollbackStart(ctx, errors.New("task environment returned an empty attachment"))
	}
	if err = s.environment.Validate(ctx); err != nil {
		return nil, s.rollbackStart(ctx, err)
	}
	env := request.Environment
	deploymentRequest := deployment.Request{
		Name: s.containerName, Image: env.Image, Workdir: env.Workdir,
		Env:        taskEnvironment(env.Env),
		Entrypoint: []string{"/bin/sleep"}, Args: []string{"infinity"},
		Labels:  ownershipLabels(request, "task-container"),
		Network: s.networkName, NetworkAliases: []string{networkAlias},
		StorageMB: env.StorageMB, GPUs: env.GPUs,
		Init: true, NoNewPrivileges: env.ExecUser != "", AllowImageVolumes: true,
	}
	if env.CPU > 0 {
		deploymentRequest.CPU = &env.CPU
	}
	if env.MemoryMB > 0 {
		deploymentRequest.MemoryMB = &env.MemoryMB
	}
	s.containerID, err = m.deployment.Create(ctx, deploymentRequest)
	s.containerOwned = s.containerID != ""
	if err != nil {
		return nil, s.rollbackStart(ctx, fmt.Errorf("create task container: %w", err))
	}
	if !s.containerOwned {
		return nil, s.rollbackStart(ctx, errors.New("create task container: deployment returned an empty container ID"))
	}
	if err = m.deployment.Validate(ctx, s.containerID, deploymentRequest, nil); err != nil {
		return nil, s.rollbackStart(ctx, err)
	}
	if err = m.deployment.Start(ctx, s.containerID); err != nil {
		return nil, s.rollbackStart(ctx, err)
	}
	running, err := m.deployment.Running(ctx, s.containerID)
	if err != nil {
		return nil, s.rollbackStart(ctx, err)
	}
	if !running {
		return nil, s.rollbackStart(ctx, errors.New("task container is not running"))
	}
	if err = m.deployment.Validate(ctx, s.containerID, deploymentRequest, nil); err != nil {
		return nil, s.rollbackStart(ctx, err)
	}
	if err = s.environment.Validate(ctx); err != nil {
		return nil, s.rollbackStart(ctx, err)
	}
	m.logger.WithContext(ctx).WithFields(logrus.Fields{"container": s.containerName, "network": s.networkName}).Info("task sandbox started")
	m.mu.Lock()
	m.active[s] = struct{}{}
	m.mu.Unlock()
	return s, nil
}

// Stop releases a sandbox created by this manager.
func (m *Manager) Stop(ctx context.Context, live runner.Sandbox) error {
	if live == nil {
		return errors.New("stop deployment sandbox: sandbox is required")
	}
	sandbox, ok := live.(*Sandbox)
	if !ok || sandbox == nil {
		return fmt.Errorf("stop deployment sandbox: unsupported sandbox type %T", live)
	}
	if sandbox.owner != m {
		return errors.New("stop deployment sandbox: sandbox belongs to another manager")
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

// ContainerID returns the immutable deployment container identifier.
func (s *Sandbox) ContainerID() string { return s.containerID }

// ContainerName returns the generated task container name.
func (s *Sandbox) ContainerName() string { return s.containerName }

// NetworkName returns the task-scoped deployment network.
func (s *Sandbox) NetworkName() string { return s.networkName }

// Workdir returns the benchmark-declared container working directory.
func (s *Sandbox) Workdir() string { return s.workdir }

// RunID returns the owning experiment run identity for bridge tool logs.
func (s *Sandbox) RunID() string { return s.runID }

// TaskID returns the owning benchmark task identity for bridge tool logs.
func (s *Sandbox) TaskID() string { return s.taskID }

// BridgeListen resolves the exact occurrence's owned task endpoint.
func (s *Sandbox) BridgeListen(ctx context.Context) (core.BridgeListen, error) {
	return s.environment.BridgeListen(ctx)
}

// BridgeListen resolves the sole active occurrence composed with this manager.
func (m *Manager) BridgeListen(ctx context.Context) (core.BridgeListen, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.active) != 1 {
		return core.BridgeListen{}, errors.New("bridge resolution requires exactly one active task environment")
	}
	for s := range m.active {
		return s.BridgeListen(ctx)
	}
	panic("unreachable")
}

// Exec runs one argv directly through deployment's typed exec API. Nonzero exits
// are returned as results, not transport errors.
func (s *Sandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if len(command.Stdin) > maxExecInput {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("deployment exec stdin exceeds %d bytes", maxExecInput)
	}
	var stdout, stderr bytes.Buffer
	var stdin io.Reader
	if len(command.Stdin) > 0 {
		stdin = bytes.NewReader(command.Stdin)
	}
	result, err := s.ExecStream(ctx, command, stdin, &stdout, &stderr)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	return result, err
}

// ExecStream preserves benchmark execution defaults while the deployment owns execution and cancellation.
func (s *Sandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	if err := validateCommand(command); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	if command.User == "" {
		command.User = s.execUser
	}
	return s.deployment.ExecStream(ctx, s.containerID, command, stdin, stdout, stderr)
}

// Upload copies one regular host file to an absolute container path.
func (s *Sandbox) Upload(ctx context.Context, source, destination string) error {
	destination, err := cleanContainerPath(destination)
	if err != nil {
		return fmt.Errorf("invalid deployment upload destination: %w", err)
	}
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("stat deployment upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("deployment upload source must be a regular file")
	}
	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open deployment upload source: %w", err)
	}
	defer file.Close()
	archiveReader, archiveWriter := io.Pipe()
	archiveErr := make(chan error, 1)
	go func() {
		writer := tar.NewWriter(archiveWriter)
		writeErr := writer.WriteHeader(&tar.Header{Name: filepath.Base(destination), Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()})
		if writeErr == nil {
			_, writeErr = io.Copy(writer, file)
		}
		if closeErr := writer.Close(); writeErr == nil {
			writeErr = closeErr
		}
		_ = archiveWriter.CloseWithError(writeErr)
		archiveErr <- writeErr
	}()
	copyErr := s.deployment.UploadArchive(ctx, s.containerID, filepath.Dir(destination), archiveReader)
	_ = archiveReader.Close()
	writeErr := <-archiveErr
	if copyErr != nil || writeErr != nil {
		return fmt.Errorf("upload file to deployment task container: %w", errors.Join(copyErr, writeErr))
	}
	return nil
}

// Download copies one regular container file beneath the configured output directory.
func (s *Sandbox) Download(ctx context.Context, source, destination string) error {
	return s.download(ctx, source, destination, nil)
}

// DownloadLimit copies one regular container file while bounding host bytes.
func (s *Sandbox) DownloadLimit(ctx context.Context, source, destination string, maxBytes int64) error {
	if maxBytes < 0 {
		return errors.New("deployment download byte limit must be nonnegative")
	}
	return s.download(ctx, source, destination, &maxBytes)
}

func (s *Sandbox) download(ctx context.Context, source, destination string, maxBytes *int64) error {
	source, err := cleanContainerPath(source)
	if err != nil {
		return fmt.Errorf("invalid deployment download source: %w", err)
	}
	destination, err = outputPath(s.outputDir, destination)
	if err != nil {
		return err
	}
	content, info, err := s.deployment.DownloadArchive(ctx, s.containerID, source)
	if err != nil {
		return fmt.Errorf("download task file: %w", err)
	}
	defer content.Close()
	if maxBytes != nil && (info.Size < 0 || info.Size > *maxBytes) {
		return fmt.Errorf("deployment download source size %d exceeds limit %d", info.Size, *maxBytes)
	}
	if !info.Mode.IsRegular() {
		return errors.New("deployment download source must be a regular file")
	}
	reader := tar.NewReader(content)
	var header *tar.Header
	for {
		header, err = reader.Next()
		if err == nil && header.FileInfo().Mode().IsRegular() {
			break
		}
		if errors.Is(err, io.EOF) {
			return errors.New("deployment download archive contains no regular file")
		}
		if err != nil {
			return fmt.Errorf("read deployment download archive: %w", err)
		}
	}
	if maxBytes != nil && header.Size > *maxBytes {
		return fmt.Errorf("deployment download archive file size %d exceeds limit %d", header.Size, *maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return fmt.Errorf("create deployment download directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".aries-download-*")
	if err != nil {
		return fmt.Errorf("create deployment download destination: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.CopyN(temporary, reader, header.Size); err != nil {
		temporary.Close()
		return fmt.Errorf("write deployment download: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("secure deployment download: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close deployment download: %w", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("publish deployment download: %w", err)
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
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		s.owner.mu.Lock()
		delete(s.owner.active, s)
		s.owner.mu.Unlock()
	}
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
		if err := s.deployment.Stop(ctx, s.containerID); err != nil {
			errs = append(errs, fmt.Errorf("stop task container: %w", err))
		} else {
			s.mu.Lock()
			s.containerOwned = false
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	containerRemoved := !s.containerOwned
	s.mu.Unlock()
	if networkOwned && containerRemoved {
		if err := s.environment.Stop(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop task network: %w", err))
		} else {
			s.mu.Lock()
			s.networkOwned = false
			s.mu.Unlock()
		}
	}
	return errors.Join(errs...)
}

func (s *Sandbox) rollbackStart(ctx context.Context, primary error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	if cleanupErr := s.stopOnce(cleanupCtx, false); cleanupErr != nil {
		s.owner.mu.Lock()
		s.owner.pending[s] = struct{}{}
		s.owner.mu.Unlock()
		return errors.Join(primary, fmt.Errorf("rollback partial sandbox: %w", cleanupErr))
	}
	return primary
}

func (s *Sandbox) collectLogs(ctx context.Context) error {
	stdout, err := os.OpenFile(filepath.Join(s.artifactDir, "container.stdout.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create sandbox task stdout log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(s.artifactDir, "container.stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create sandbox task stderr log: %w", err)
	}
	defer stderr.Close()
	err = s.deployment.LogsStream(ctx, s.containerID, stdout, stderr)
	if errors.Is(err, runner.ErrNotFound) {
		return nil
	}
	return err
}

func outputPath(root, destination string) (string, error) {
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve deployment download destination: %w", err)
	}
	relative, err := filepath.Rel(root, absolute)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("deployment download destination is outside the configured output directory")
	}
	return absolute, nil
}

func deploymentEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		result = append(result, key+"="+values[key])
	}
	return result
}

func taskEnvironment(values map[string]string) []string {
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
	return deploymentEnvironment(owned)
}

func validateEnvironment(environment core.Environment) error {
	if err := validatePullImage(environment.Image); err != nil {
		return fmt.Errorf("invalid sandbox image: %w", err)
	}
	if _, err := cleanContainerWorkdir(environment.Workdir); err != nil {
		return fmt.Errorf("invalid sandbox workdir: %w", err)
	}
	if err := validateExecUser(environment.ExecUser); err != nil {
		return fmt.Errorf("invalid sandbox exec user: %w", err)
	}
	if environment.CPU < 0 || math.IsNaN(environment.CPU) || math.IsInf(environment.CPU, 0) || environment.CPU*1e9 >= math.Exp2(63) {
		return errors.New("sandbox CPU must be finite, nonnegative, and convert to NanoCPUs below 2^63")
	}
	if environment.MemoryMB < 0 || int64(environment.MemoryMB) > math.MaxInt64>>20 || environment.StorageMB < 0 || environment.GPUs < 0 {
		return errors.New("sandbox memory, storage, and GPU counts must be nonnegative")
	}
	for key, value := range environment.Env {
		if !validEnvName(key) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid sandbox environment %q", key)
		}
	}
	return nil
}

func validateIdentity(kind, value string) error {
	limit := 128
	if kind == "task" {
		limit = 149
	}
	if value == "" || len(value) > limit {
		return fmt.Errorf("sandbox %s ID must contain 1 to %d characters", kind, limit)
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return fmt.Errorf("sandbox %s ID %q contains an unsafe character", kind, value)
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
	if err := validateExecUser(command.User); err != nil {
		return fmt.Errorf("invalid command user: %w", err)
	}
	if command.Timeout < 0 {
		return errors.New("command timeout must be nonnegative")
	}
	if command.OutputLimitBytes < 0 || command.OutputLimitBytes > maxConfiguredOutput {
		return fmt.Errorf("command output limit must be between 0 and %d bytes", maxConfiguredOutput)
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

func validateExecUser(value string) error {
	if value == "" {
		return nil
	}
	uid, gid, found := strings.Cut(value, ":")
	if !found || strings.ContainsRune(gid, ':') || !decimalDigits(uid) || !decimalDigits(gid) {
		return errors.New("exec user must be a numeric UID:GID pair")
	}
	return nil
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
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

func validatePullImage(image string) error {
	if err := containerimage.Validate(image); err == nil {
		return nil
	}
	tagged, err := containerimage.ValidateTagOnly(image)
	if err != nil {
		return err
	}
	if tagged != image {
		return errors.New("image must not contain surrounding whitespace")
	}
	return nil
}
