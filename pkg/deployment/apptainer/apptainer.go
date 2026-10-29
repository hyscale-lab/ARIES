// Package apptainer implements shared runtime deployment with Apptainer, the
// container runtime formerly called Singularity, for hosts where a normal user
// cannot run Docker, such as HPC clusters.
//
// It drives the apptainer command, since Apptainer has no daemon to call.
// Each runtime is a named Apptainer instance of an image that PullImages
// unpacked. An instance runs as the invoking user with the appearance of root
// (--fakeroot), hides the host's home directory and environment (--containall)
// and writes to its own overlay. Create starts the instance with nothing
// running in it, so private files can be staged before Start runs the
// requested command, which Docker gives by separating create from start.
//
// Apptainer has no labels, so ownership lives in a record this provider writes
// beside each instance; an instance's name and PID are its identity.
//
// Environment values reach the instance in owner-only files under a read-only
// bind, and the instance's own shell reads them: on apptainer's command line
// they would be visible to every user of the host, and apptainer's --env-file
// does not preserve every value (1.5.0 turned a quote in "it's" into "\'").
//
// A normal user cannot give an instance its own network, so every instance of
// a run shares the network namespace of the process that starts it. Services
// are therefore addressed on loopback, and the run must provide a private
// namespace for its processes; this package does not create one.
package apptainer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/runner"
	"github.com/sirupsen/logrus"
)

const (
	defaultApptainer      = "apptainer"
	defaultCleanupTimeout = 60 * time.Second
	stopGrace             = 5 * time.Second
	recordFile            = "record.json"
	environmentFile       = "env"
	shareDir              = "share"
	shareTarget           = "/.aries"
	runscriptPath         = "/.singularity.d/runscript"
	// startShell exports the runtime's environment and runs the command.
	startShell   = `set -a; . "$1"; set +a; shift; exec "$@"`
	stdoutLog    = "stdout.log"
	stderrLog    = "stderr.log"
	loopbackHost = "127.0.0.1"
)

// errMissing marks a runtime with no ARIES record: never created, or removed.
// It is not runner.ErrNotFound, which callers read as a missing file inside a
// runtime that exists.
var errMissing = errors.New("Apptainer runtime has no ARIES record")

// commandRunner runs one apptainer invocation to completion.
type commandRunner interface {
	Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error
}

// launcher starts one long-lived apptainer invocation in its own process
// group, with its output appended to the given files, and returns its PID.
type launcher interface {
	Launch(args []string, stdout, stderr *os.File) (int, error)
}

// Options configures one Apptainer transport.
type Options struct {
	// ApptainerPath is the apptainer binary; empty means apptainer on PATH.
	ApptainerPath string
	// StateDir holds each runtime's ownership record, environment file and
	// logs. It is private to the invoking user.
	StateDir string
	// ImageDir holds the images PullImages unpacked, named by reference.
	ImageDir string
	// OverlayDir holds each runtime's writable layer. Node-local disk is much
	// faster than a parallel filesystem. Empty uses StateDir.
	OverlayDir string
	// UnenforcedLimits accepts CPU and memory requests that Apptainer cannot
	// enforce without cgroups v2. The default rejects them before any effect;
	// a run that sets it must record that the limits were not applied.
	UnenforcedLimits bool
	// DNS replaces the host's name servers inside instances. A run namespace
	// needs it when the host resolves through its own loopback, as
	// systemd-resolved does at 127.0.0.53, which the namespace cannot reach.
	DNS            []string
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Manager implements deployment operations with Apptainer instances.
type Manager struct {
	options   Options
	apptainer commandRunner
	launcher  launcher
	// processStart returns the start time of a live, non-zombie process, which
	// distinguishes it from a later process that reuses its PID.
	processStart func(pid int) (uint64, bool, error)
	// signalGroup signals a process group.
	signalGroup func(pgid int, signal syscall.Signal) error
	logger      *logrus.Logger
	mu          sync.Mutex
}

var (
	_ deployment.Deployment     = (*Manager)(nil)
	_ deployment.ServiceRuntime = (*Manager)(nil)
)

// New configures a transport without running apptainer.
func New(options Options) (*Manager, error) {
	if options.ApptainerPath == "" {
		options.ApptainerPath = defaultApptainer
	}
	if options.StateDir == "" || !filepath.IsAbs(options.StateDir) {
		return nil, errors.New("Apptainer deployment requires an absolute state directory")
	}
	if options.ImageDir != "" && !filepath.IsAbs(options.ImageDir) {
		return nil, errors.New("Apptainer image directory must be absolute")
	}
	if options.OverlayDir == "" {
		options.OverlayDir = options.StateDir
	}
	if !filepath.IsAbs(options.OverlayDir) {
		return nil, errors.New("Apptainer overlay directory must be absolute")
	}
	for _, server := range options.DNS {
		if net.ParseIP(server) == nil {
			return nil, fmt.Errorf("Apptainer DNS server %q is not an IP address", server)
		}
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	logger := options.Logger
	if logger == nil {
		logger = logrus.StandardLogger()
	}
	return &Manager{
		options: options, apptainer: execApptainer{path: options.ApptainerPath},
		launcher: execLauncher{path: options.ApptainerPath}, processStart: procStart, logger: logger,
		signalGroup: func(pgid int, signal syscall.Signal) error { return syscall.Kill(-pgid, signal) },
	}, nil
}

// Close releases nothing: the transport holds no connection.
func (manager *Manager) Close() error { return nil }

// record is the ownership and configuration ARIES keeps for one instance.
type record struct {
	Name          string             `json:"name"`
	Image         string             `json:"image"`
	Rootfs        string             `json:"rootfs"`
	Labels        map[string]string  `json:"labels,omitempty"`
	Attachment    string             `json:"attachment"`
	Workdir       string             `json:"workdir,omitempty"`
	Entrypoint    []string           `json:"entrypoint,omitempty"`
	EntrypointSet bool               `json:"entrypoint_set"`
	Args          []string           `json:"args,omitempty"`
	Mounts        []deployment.Mount `json:"mounts,omitempty"`
	ServicePort   int                `json:"service_port,omitempty"`
	InternalPort  int                `json:"internal_port,omitempty"`
	GPUs          int                `json:"gpus,omitempty"`
	StorageMB     int                `json:"storage_mb,omitempty"`
	Overlay       string             `json:"overlay"`
	InstancePID   int                `json:"instance_pid,omitempty"`
	MainPID       int                `json:"main_pid,omitempty"`
	MainStart     uint64             `json:"main_start,omitempty"`
}

// Create starts an empty instance for the request. The identity it returns is
// the instance name, which callers generate with a random nonce.
func (manager *Manager) Create(ctx context.Context, request deployment.Request) (string, error) {
	if err := manager.validateRequest(request); err != nil {
		return "", err
	}
	rootfs, err := manager.imagePath(request.Image)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(rootfs); err != nil || !info.IsDir() {
		return "", fmt.Errorf("Apptainer image %q is not prepared at %s; run aries setup first", request.Image, rootfs)
	}
	// As with Docker, a runtime that names no workdir uses its image's.
	workdir := request.Workdir
	if workdir == "" {
		if workdir, err = imageWorkdir(rootfs); err != nil {
			return "", fmt.Errorf("Apptainer image %q: %w", request.Image, err)
		}
	}
	directory := manager.runtimeDir(request.Name)
	if err := os.MkdirAll(manager.options.StateDir, 0o700); err != nil {
		return "", fmt.Errorf("create Apptainer state directory: %w", err)
	}
	// An existing directory means the name is already in use; never adopt it.
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", fmt.Errorf("reserve Apptainer runtime %s: %w", request.Name, err)
	}
	created := record{
		Name: request.Name, Image: request.Image, Rootfs: rootfs, Labels: request.Labels, Attachment: request.Placement.AttachmentID,
		Workdir: workdir, Entrypoint: request.Entrypoint, EntrypointSet: request.Entrypoint != nil, Args: request.Args,
		Mounts: request.Mounts, ServicePort: request.ServicePort, InternalPort: request.InternalPort,
		GPUs: request.GPUs, StorageMB: request.StorageMB, Overlay: manager.overlayPath(request),
	}
	// Until the instance exists nothing outside ARIES's own files is allocated,
	// so a failure here removes them and reports no identity.
	discard := func(cause error) (string, error) {
		return "", errors.Join(cause, manager.removeFiles(created))
	}
	if err := os.Mkdir(manager.sharePath(request.Name), 0o700); err != nil {
		return discard(fmt.Errorf("create Apptainer share directory: %w", err))
	}
	if err := writePrivate(filepath.Join(manager.sharePath(request.Name), environmentFile), environmentScript(request.Env, nil)); err != nil {
		return discard(fmt.Errorf("write Apptainer environment: %w", err))
	}
	if err := manager.createOverlay(ctx, created); err != nil {
		return discard(err)
	}
	if err := manager.saveRecord(created); err != nil {
		return discard(err)
	}
	args := []string{"instance", "start", "--containall", "--fakeroot", "--overlay", created.Overlay, "--bind", manager.sharePath(created.Name) + ":" + shareTarget + ":ro"}
	if len(manager.options.DNS) != 0 {
		args = append(args, "--dns", strings.Join(manager.options.DNS, ","))
	}
	if created.GPUs > 0 {
		args = append(args, "--nv")
	}
	for _, mount := range created.Mounts {
		spec := mount.Source + ":" + mount.Target
		if mount.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "--bind", spec)
	}
	args = append(args, rootfs, created.Name)
	if _, startErr := manager.run(ctx, nil, args...); startErr != nil {
		return manager.recoverCreate(ctx, created, fmt.Errorf("start Apptainer instance: %w", startErr))
	}
	instance, found, err := manager.instance(ctx, created.Name)
	if err != nil || !found {
		if err == nil {
			err = errors.New("instance is not listed after it started")
		}
		return created.Name, fmt.Errorf("confirm Apptainer instance %s: %w", created.Name, err)
	}
	if instance.Image != rootfs {
		return created.Name, fmt.Errorf("Apptainer instance %s runs %s, not the requested image", created.Name, instance.Image)
	}
	created.InstancePID = instance.PID
	if err := manager.saveRecord(created); err != nil {
		return created.Name, err
	}
	return created.Name, nil
}

// recoverCreate resolves a failed instance start. A start that failed late can
// leave an instance behind, so absence is confirmed before the files go.
func (manager *Manager) recoverCreate(ctx context.Context, created record, cause error) (string, error) {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manager.options.CleanupTimeout)
	defer cancel()
	instance, found, err := manager.instance(recoveryCtx, created.Name)
	if err != nil {
		// The files stay: they are how Stop would find the instance later.
		return "", errors.Join(cause, fmt.Errorf("%w: %w", deployment.ErrAllocationUnconfirmed, err))
	}
	if !found {
		return "", errors.Join(cause, manager.removeFiles(created))
	}
	if instance.Image != created.Rootfs {
		return "", errors.Join(cause, fmt.Errorf("%w: instance %s runs another image", deployment.ErrAllocationUnconfirmed, created.Name))
	}
	created.InstancePID = instance.PID
	return created.Name, errors.Join(cause, manager.saveRecord(created))
}

// Validate confirms identity, ownership and configuration, and that no supplied
// secret reached the record or the environment file.
func (manager *Manager) Validate(ctx context.Context, id string, request deployment.Request, secrets [][]byte) error {
	if err := manager.validateRequest(request); err != nil {
		return err
	}
	current, raw, err := manager.loadRecord(id)
	if err != nil {
		return err
	}
	if current.Name != id || request.Name != "" && request.Name != id {
		return errors.New("deployment name differs from request")
	}
	rootfs, err := manager.imagePath(request.Image)
	if err != nil {
		return err
	}
	if current.Image != request.Image || current.Rootfs != rootfs || !slices.Equal(current.Args, request.Args) ||
		request.Entrypoint != nil && (!current.EntrypointSet || !slices.Equal(current.Entrypoint, request.Entrypoint)) {
		return errors.New("deployment image or command differs from requested configuration")
	}
	if request.Workdir != "" && current.Workdir != request.Workdir {
		return errors.New("deployment workdir differs from request")
	}
	if current.Attachment != request.Placement.AttachmentID {
		return errors.New("deployment must use only the task attachment")
	}
	for key, value := range request.Labels {
		if current.Labels[key] != value {
			return errors.New("deployment labels do not match the task")
		}
	}
	if !slices.Equal(current.Mounts, request.Mounts) {
		return errors.New("deployment host mounts differ from request")
	}
	if current.ServicePort != request.ServicePort || current.GPUs != request.GPUs || current.StorageMB != request.StorageMB {
		return errors.New("deployment resources differ from request")
	}
	instance, found, err := manager.instance(ctx, id)
	if err != nil {
		return err
	}
	if !found || instance.PID != current.InstancePID || instance.Image != current.Rootfs {
		return errors.New("deployment instance identity differs from its record")
	}
	environment, err := os.ReadFile(filepath.Join(manager.sharePath(id), environmentFile))
	if err != nil {
		return fmt.Errorf("read deployment environment: %w", err)
	}
	for _, secret := range secrets {
		if len(secret) > 0 && (bytes.Contains(raw, secret) || bytes.Contains(environment, secret)) {
			return errors.New("deployment secret entered Apptainer metadata")
		}
	}
	if request.NoNewPrivileges {
		status, err := manager.run(ctx, nil, append(execPrefix(id, "/"), "cat", "/proc/self/status")...)
		if err != nil {
			return fmt.Errorf("inspect deployment privileges: %w", err)
		}
		if !hasNoNewPrivileges(status) {
			return errors.New("deployment no-new-privileges is not enabled")
		}
	}
	return nil
}

func hasNoNewPrivileges(status []byte) bool {
	for _, line := range strings.Split(string(status), "\n") {
		if name, value, ok := strings.Cut(line, ":"); ok && name == "NoNewPrivs" {
			return strings.TrimSpace(value) == "1"
		}
	}
	return false
}

// Start runs the requested command in the instance with the runtime's
// environment: the image's runscript when no entrypoint was given, otherwise
// the entrypoint with its arguments. A command already running is left alone.
func (manager *Manager) Start(ctx context.Context, id string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current, _, err := manager.loadRecord(id)
	if err != nil {
		return err
	}
	if alive, err := manager.mainAlive(current); err != nil {
		return err
	} else if alive {
		return nil
	}
	if _, found, err := manager.instance(ctx, id); err != nil {
		return err
	} else if !found {
		return fmt.Errorf("deployment instance %s is not running", id)
	}
	directory := manager.runtimeDir(id)
	args := append(execPrefix(id, current.Workdir), "/bin/sh", "-c", startShell, "aries-start", shareTarget+"/"+environmentFile)
	if current.EntrypointSet {
		args = append(args, current.Entrypoint...)
	} else {
		args = append(args, runscriptPath)
	}
	args = append(args, current.Args...)
	stdout, err := os.OpenFile(filepath.Join(directory, stdoutLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open deployment log: %w", err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(directory, stderrLog), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open deployment log: %w", err)
	}
	defer stderr.Close()
	pid, err := manager.launcher.Launch(args, stdout, stderr)
	if err != nil {
		return fmt.Errorf("start deployment command: %w", err)
	}
	started, alive, err := manager.processStart(pid)
	if err != nil {
		return fmt.Errorf("inspect deployment command: %w", err)
	}
	if !alive {
		return errors.New("deployment command exited as it started")
	}
	current.MainPID, current.MainStart = pid, started
	return manager.saveRecord(current)
}

// Running reports whether the started command is still running. A runtime that
// was created but not started, like a created Docker container, is not.
func (manager *Manager) Running(ctx context.Context, id string) (bool, error) {
	current, _, err := manager.loadRecord(id)
	if errors.Is(err, errMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	instance, found, err := manager.instance(ctx, id)
	if err != nil || !found || instance.PID != current.InstancePID {
		return false, err
	}
	return manager.mainAlive(current)
}

func (manager *Manager) mainAlive(current record) (bool, error) {
	if current.MainPID <= 0 {
		return false, nil
	}
	started, alive, err := manager.processStart(current.MainPID)
	if err != nil {
		return false, err
	}
	return alive && started == current.MainStart, nil
}

// Logs returns the started command's bounded output.
func (manager *Manager) Logs(ctx context.Context, id string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("deployment log bound must be positive")
	}
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = limit, limit
	if err := manager.LogsStream(ctx, id, &stdout, &stderr); err != nil {
		return nil, err
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errors.New("deployment logs exceeded their bound")
	}
	return append(stdout.Bytes(), stderr.Bytes()...), nil
}

// LogsStream copies the started command's output so far.
func (manager *Manager) LogsStream(_ context.Context, id string, stdout, stderr io.Writer) error {
	if _, _, err := manager.loadRecord(id); errors.Is(err, errMissing) {
		return fmt.Errorf("deployment logs: %w: %w", runner.ErrNotFound, err)
	} else if err != nil {
		return fmt.Errorf("deployment logs: %w", err)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	var errs []error
	for name, destination := range map[string]io.Writer{stdoutLog: stdout, stderrLog: stderr} {
		file, err := os.Open(filepath.Join(manager.runtimeDir(id), name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, copyErr := io.Copy(destination, file)
		errs = append(errs, copyErr, file.Close())
	}
	return errors.Join(errs...)
}

// Address returns the loopback address of the runtime's published service.
// Every instance of the run shares the network namespace of the run's
// processes, so the service listens there directly.
func (manager *Manager) Address(ctx context.Context, id string, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", errors.New("deployment service port is invalid")
	}
	current, _, err := manager.loadRecord(id)
	if err != nil {
		return "", err
	}
	if current.ServicePort != port {
		return "", fmt.Errorf("deployment publishes no service on port %d", port)
	}
	if _, found, err := manager.instance(ctx, id); err != nil {
		return "", err
	} else if !found {
		return "", errors.New("deployment service runtime is not running")
	}
	return net.JoinHostPort(loopbackHost, strconv.Itoa(port)), nil
}

// TaskAddress resolves a port of a running runtime for other runtimes of the
// run, which share its loopback.
func (manager *Manager) TaskAddress(ctx context.Context, id string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid task service port")
	}
	running, err := manager.Running(ctx, id)
	if err != nil {
		return "", err
	}
	if !running {
		return "", errors.New("runtime task attachment is not ready")
	}
	return net.JoinHostPort(loopbackHost, strconv.Itoa(port)), nil
}

// Stop ends the started command and the instance, removes the overlay and the
// record, and confirms all of them are gone. It is safe to repeat.
func (manager *Manager) Stop(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if err := validName(id); err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	current, _, recordErr := manager.loadRecord(id)
	if recordErr != nil && !errors.Is(recordErr, errMissing) {
		return recordErr
	}
	var errs []error
	if recordErr == nil {
		if err := manager.stopMain(current); err != nil {
			errs = append(errs, err)
		}
	}
	instance, found, err := manager.instance(ctx, id)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("inspect deployment before stop: %w", err))...)
	}
	if found && recordErr != nil {
		// An instance with this name but no record is not known to be ours.
		return errors.New("deployment instance has no ARIES record; refusing to stop it")
	}
	if found && current.InstancePID != 0 && instance.PID != current.InstancePID {
		return errors.New("deployment instance identity differs from its record; refusing to stop it")
	}
	if found {
		if _, err := manager.run(ctx, nil, "instance", "stop", "--timeout", strconv.Itoa(int(stopGrace.Seconds())), id); err != nil {
			errs = append(errs, fmt.Errorf("stop deployment instance: %w", err))
			if _, err := manager.run(ctx, nil, "instance", "stop", "--force", id); err != nil {
				errs = append(errs, fmt.Errorf("force-stop deployment instance: %w", err))
			}
		}
		if _, found, err = manager.instance(ctx, id); err != nil {
			return errors.Join(append(errs, fmt.Errorf("verify deployment removal: %w", err))...)
		} else if found {
			return errors.Join(append(errs, errors.New("deployment remains after removal"))...)
		}
	}
	if recordErr == nil {
		if err := manager.removeFiles(current); err != nil {
			return errors.Join(append(errs, err)...)
		}
	}
	if warning := errors.Join(errs...); warning != nil {
		manager.logger.WithContext(ctx).WithError(warning).Warn("deployment cleanup recovered after lifecycle errors")
	}
	return nil
}

// stopMain ends the started command's process group, which also ends any
// process it started, and waits until the group is gone.
func (manager *Manager) stopMain(current record) error {
	alive, err := manager.mainAlive(current)
	if err != nil || !alive {
		return err
	}
	if err := manager.signalGroup(current.MainPID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal deployment command: %w", err)
	}
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if alive, err := manager.mainAlive(current); err != nil || !alive {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := manager.signalGroup(current.MainPID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill deployment command: %w", err)
	}
	deadline = time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if alive, err := manager.mainAlive(current); err != nil || !alive {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("deployment command remains after kill")
}

// removeFiles deletes the overlay and the runtime directory and confirms both
// are gone. Files created as root inside the instance belong to the invoking
// user, but directories may lack write permission, which is restored first.
func (manager *Manager) removeFiles(current record) error {
	var errs []error
	for _, path := range []string{current.Overlay, manager.runtimeDir(current.Name)} {
		if path == "" {
			continue
		}
		// A directory is opened up before it is read; whatever still cannot be
		// read makes RemoveAll fail below, which is reported.
		_ = filepath.WalkDir(path, func(entry string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				_ = os.Chmod(entry, 0o700)
			}
			return nil
		})
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, fmt.Errorf("remove %s: %w", path, err))
		}
		if _, err := os.Lstat(path); err == nil {
			errs = append(errs, fmt.Errorf("%s remains after removal", path))
		} else if !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("verify removal of %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

// createOverlay prepares the writable layer: a fixed-size overlay image when
// storage is bounded, so the bound holds, and a directory otherwise. The image
// is sparse: without --sparse apptainer writes every byte of it (10 GB of zeros
// for a Terminal-Bench task) before the instance can start.
func (manager *Manager) createOverlay(ctx context.Context, current record) error {
	if err := os.MkdirAll(manager.options.OverlayDir, 0o700); err != nil {
		return fmt.Errorf("create Apptainer overlay directory: %w", err)
	}
	if current.StorageMB == 0 {
		if err := os.Mkdir(current.Overlay, 0o700); err != nil {
			return fmt.Errorf("create Apptainer overlay: %w", err)
		}
		return nil
	}
	if _, err := manager.run(ctx, nil, "overlay", "create", "--fakeroot", "--sparse", "--size", strconv.Itoa(current.StorageMB), current.Overlay); err != nil {
		return fmt.Errorf("create Apptainer overlay image: %w", err)
	}
	return nil
}

func (manager *Manager) overlayPath(request deployment.Request) string {
	path := filepath.Join(manager.options.OverlayDir, request.Name+".overlay")
	if request.StorageMB > 0 {
		path += ".img"
	}
	return path
}

func (manager *Manager) runtimeDir(name string) string {
	return filepath.Join(manager.options.StateDir, name)
}

// sharePath is the runtime's directory that the instance sees, read-only, at
// shareTarget. It holds environment files and nothing else.
func (manager *Manager) sharePath(name string) string {
	return filepath.Join(manager.runtimeDir(name), shareDir)
}

func (manager *Manager) saveRecord(current record) error {
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(manager.runtimeDir(current.Name), recordFile), raw); err != nil {
		return fmt.Errorf("write Apptainer runtime record: %w", err)
	}
	return nil
}

// loadRecord returns the runtime's record and its raw bytes. A missing record
// wraps errMissing.
func (manager *Manager) loadRecord(id string) (record, []byte, error) {
	if err := validName(id); err != nil {
		return record{}, nil, err
	}
	raw, err := os.ReadFile(filepath.Join(manager.runtimeDir(id), recordFile))
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, nil, fmt.Errorf("deployment %s: %w", id, errMissing)
	}
	if err != nil {
		return record{}, nil, fmt.Errorf("read Apptainer runtime record: %w", err)
	}
	var current record
	if err := json.Unmarshal(raw, &current); err != nil {
		return record{}, nil, fmt.Errorf("decode Apptainer runtime record: %w", err)
	}
	if current.Name != id {
		return record{}, nil, errors.New("Apptainer runtime record names another runtime")
	}
	return current, raw, nil
}

// writePrivate replaces path atomically with an owner-only file.
func writePrivate(path string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	_, writeErr := temporary.Write(content)
	closeErr := temporary.Close()
	if err := errors.Join(writeErr, closeErr, os.Chmod(name, 0o600)); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// instanceInfo is one entry of `apptainer instance list --json`.
type instanceInfo struct {
	Name  string `json:"instance"`
	PID   int    `json:"pid"`
	Image string `json:"img"`
}

// instance reads one instance, reporting absence without an error.
func (manager *Manager) instance(ctx context.Context, name string) (instanceInfo, bool, error) {
	raw, err := manager.run(ctx, nil, "instance", "list", "--json", name)
	if err != nil {
		return instanceInfo{}, false, fmt.Errorf("list Apptainer instances: %w", err)
	}
	var listing struct {
		Instances []instanceInfo `json:"instances"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return instanceInfo{}, false, fmt.Errorf("decode Apptainer instance list: %w", err)
	}
	for _, instance := range listing.Instances {
		if instance.Name == name {
			return instance, true, nil
		}
	}
	return instanceInfo{}, false, nil
}

// imagePath resolves a reference to its unpacked image.
func (manager *Manager) imagePath(reference string) (string, error) {
	if manager.options.ImageDir == "" {
		return "", fmt.Errorf("Apptainer image %q cannot be found: no image directory is configured", reference)
	}
	return rootfsPath(manager.options.ImageDir, reference), nil
}

// rootfsPath names a reference's unpacked image by its hash, since a
// reference holds characters that do not belong in file names.
func rootfsPath(directory, reference string) string {
	sum := sha256.Sum256([]byte(reference))
	return filepath.Join(directory, hex.EncodeToString(sum[:])+".rootfs")
}

func (manager *Manager) validateRequest(request deployment.Request) error {
	if strings.TrimSpace(request.Placement.AttachmentID) == "" {
		return errors.New("Apptainer deployment requires a nonempty task attachment")
	}
	if err := validName(request.Name); err != nil {
		return err
	}
	if request.Image == "" {
		return errors.New("Apptainer deployment requires an image")
	}
	if (request.CPU != nil || request.MemoryMB != nil) && !manager.options.UnenforcedLimits {
		return errors.New("Apptainer cannot enforce CPU or memory limits without cgroups v2; the deployment must accept unenforced limits explicitly")
	}
	if request.GPUs < 0 || request.StorageMB < 0 {
		return errors.New("deployment GPU and storage bounds must be nonnegative")
	}
	if len(request.NetworkAliases) != 0 {
		return errors.New("Apptainer runtimes share one loopback and cannot take network aliases")
	}
	if request.ServicePort < 0 || request.ServicePort > 65535 || request.InternalPort < 0 || request.InternalPort > 65535 {
		return errors.New("deployment service port is invalid")
	}
	if request.Workdir != "" && (!filepath.IsAbs(request.Workdir) || filepath.Clean(request.Workdir) != request.Workdir) {
		return errors.New("deployment workdir must be absolute and clean")
	}
	if request.Entrypoint != nil && len(request.Entrypoint)+len(request.Args) == 0 {
		return errors.New("deployment command is empty")
	}
	targets := make(map[string]bool, len(request.Mounts))
	for _, declared := range request.Mounts {
		if !filepath.IsAbs(declared.Source) || filepath.Clean(declared.Source) != declared.Source || !filepath.IsAbs(declared.Target) || filepath.Clean(declared.Target) != declared.Target || strings.ContainsAny(declared.Source+declared.Target, ":,\x00") {
			return errors.New("deployment mounts require absolute clean source and target paths without ':' or ','")
		}
		if targets[declared.Target] || declared.Target == shareTarget || strings.HasPrefix(declared.Target, shareTarget+"/") {
			return errors.New("deployment mount targets must be distinct and outside " + shareTarget)
		}
		targets[declared.Target] = true
	}
	for _, entry := range request.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvName(name) || strings.ContainsRune(value, 0) {
			return fmt.Errorf("invalid deployment environment entry for %q", name)
		}
	}
	return nil
}

// validName accepts the names ARIES generates; anything else could escape the
// state directory or be read by apptainer as an option.
func validName(name string) error {
	if name == "" || len(name) > 128 {
		return errors.New("Apptainer runtime name must contain 1 to 128 characters")
	}
	for index, character := range name {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return fmt.Errorf("Apptainer runtime name %q contains an unsafe character", name)
	}
	return nil
}

// environmentScript renders variables for --env-file, which apptainer reads as
// shell assignments: every value is single-quoted, so nothing is expanded.
// Later entries override earlier ones, as in a Docker exec's environment.
func environmentScript(entries []string, overrides map[string]string) []byte {
	var script bytes.Buffer
	write := func(name, value string) {
		script.WriteString(name + "=" + shellQuote(value) + "\n")
	}
	for _, entry := range entries {
		name, value, _ := strings.Cut(entry, "=")
		write(name, value)
	}
	for _, name := range slices.Sorted(maps.Keys(overrides)) {
		write(name, overrides[name])
	}
	return script.Bytes()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// run executes apptainer and returns its stdout. Failures carry its stderr.
func (manager *Manager) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := manager.apptainer.Run(ctx, stdin, &stdout, &stderr, args...); err != nil {
		return stdout.Bytes(), fmt.Errorf("apptainer %s: %w: %s", strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

type execApptainer struct{ path string }

func (a execApptainer) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	command := exec.CommandContext(ctx, a.path, args...)
	command.Stdout, command.Stderr = stdout, stderr
	// Bound the wait for output pipes after cancellation kills apptainer.
	command.WaitDelay = 5 * time.Second
	if stdin != nil {
		// Copy input ourselves: handing the reader to os/exec would make Wait
		// block on a caller stream that stays open after the command exits.
		pipe, err := command.StdinPipe()
		if err != nil {
			return err
		}
		go func() {
			_, _ = io.Copy(pipe, stdin)
			_ = pipe.Close()
		}()
	}
	err := command.Run()
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return ctxErr
	}
	return err
}

type execLauncher struct{ path string }

// Launch starts apptainer in a new process group, so Stop can signal the
// command together with everything it started, and reaps it when it exits.
func (l execLauncher) Launch(args []string, stdout, stderr *os.File) (int, error) {
	command := exec.Command(l.path, args...)
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
}

// procStart reads a process's start time from /proc. A zombie has exited.
func procStart(pid int) (uint64, bool, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return parseProcStat(raw)
}

func parseProcStat(raw []byte) (uint64, bool, error) {
	// The command name may contain spaces and parentheses; fields follow the last ')'.
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return 0, false, errors.New("unexpected /proc stat format")
	}
	fields := strings.Fields(string(raw[end+1:]))
	// fields[0] is the state (field 3); the start time is field 22.
	if len(fields) < 20 {
		return 0, false, errors.New("unexpected /proc stat format")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return 0, false, nil
	}
	started, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("parse process start time: %w", err)
	}
	return started, true, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (buffer *boundedBuffer) Write(content []byte) (int, error) {
	consumed := len(content)
	if remaining := buffer.limit - buffer.Len(); len(content) > remaining {
		content = content[:max(0, remaining)]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return consumed, err
}

// minimumVersion is the oldest Apptainer that has run this provider. Without
// subordinate IDs, 1.2.2 could not start an unprivileged --fakeroot instance
// with an overlay; 1.5.0 could.
var minimumVersion = [2]int{1, 5}

// CheckVersion confirms the apptainer binary is at least minimumVersion.
func CheckVersion(ctx context.Context, apptainerPath string) error {
	if apptainerPath == "" {
		apptainerPath = defaultApptainer
	}
	return (&Manager{apptainer: execApptainer{path: apptainerPath}}).checkVersion(ctx)
}

func (manager *Manager) checkVersion(ctx context.Context) error {
	raw, err := manager.run(ctx, nil, "version")
	if err != nil {
		return fmt.Errorf("read Apptainer version: %w", err)
	}
	text := strings.TrimSpace(string(raw))
	major, minor, ok := parseVersion(text)
	if !ok {
		return fmt.Errorf("unrecognised Apptainer version %q", text)
	}
	if major < minimumVersion[0] || major == minimumVersion[0] && minor < minimumVersion[1] {
		return fmt.Errorf("Apptainer %s is older than %d.%d, the oldest version ARIES has run with", text, minimumVersion[0], minimumVersion[1])
	}
	return nil
}

// parseVersion reads the major and minor numbers of a version such as
// "1.5.0" or "1.2.2-1.el8".
func parseVersion(text string) (int, int, bool) {
	fields := strings.SplitN(text, ".", 3)
	if len(fields) < 2 {
		return 0, 0, false
	}
	major, majorErr := strconv.Atoi(fields[0])
	minor, minorErr := strconv.Atoi(strings.TrimRightFunc(fields[1], func(r rune) bool { return r < '0' || r > '9' }))
	return major, minor, majorErr == nil && minorErr == nil
}
