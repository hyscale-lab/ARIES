package apptainer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
)

// processEnvironment is what a process runtime inherits from Runner: enough to
// find and run apptainer, and none of Runner's credentials.
var processEnvironment = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "TMPDIR", "APPTAINER_CACHEDIR", "APPTAINER_TMPDIR", runNamespaceEnv}

// ProcessRuntime runs a runtime's command as a plain process in the run's
// namespace instead of an instance. The bridge needs this: it executes in the
// sandboxes with apptainer, which cannot enter their user namespaces from
// inside another instance. Requests name an entrypoint that Executables maps
// to a host program; their paths are host paths confined to the workdir,
// which Create makes and Stop removes.
type ProcessRuntime struct {
	executables  map[string]string
	processStart func(pid int) (uint64, bool, error)
	signalGroup  func(pgid int, signal syscall.Signal) error
	start        func(path string, args []string, dir string, environment []string, stdout, stderr *os.File) (int, error)

	mu       sync.Mutex
	runtimes map[string]*processRecord
}

type processRecord struct {
	request deployment.Request
	program string
	pid     int
	started uint64
}

var (
	_ deployment.Runtime        = (*ProcessRuntime)(nil)
	_ deployment.ServiceRuntime = (*ProcessRuntime)(nil)
)

// NewProcessRuntime maps container entrypoints to host programs, such as
// /usr/local/bin/aries-bridge to the aries-bridge built beside aries.
func NewProcessRuntime(executables map[string]string) (*ProcessRuntime, error) {
	for entrypoint, program := range executables {
		if !filepath.IsAbs(entrypoint) || !filepath.IsAbs(program) {
			return nil, errors.New("process runtime executables must map absolute paths")
		}
	}
	return &ProcessRuntime{
		executables: executables, processStart: procStart,
		signalGroup: func(pgid int, signal syscall.Signal) error { return syscall.Kill(-pgid, signal) },
		start:       startProcess,
		runtimes:    make(map[string]*processRecord),
	}, nil
}

func (p *ProcessRuntime) Close() error { return nil }

func (p *ProcessRuntime) validateRequest(request deployment.Request) (string, error) {
	if err := validName(request.Name); err != nil {
		return "", err
	}
	if len(request.Entrypoint) != 1 {
		return "", errors.New("process runtime requires a single entrypoint")
	}
	program := p.executables[request.Entrypoint[0]]
	if program == "" {
		return "", fmt.Errorf("process runtime has no program for %q", request.Entrypoint[0])
	}
	if !cleanAbsolutePath(request.Workdir) {
		return "", errors.New("process runtime requires an absolute clean host workdir")
	}
	if len(request.Mounts) != 0 || len(request.NetworkAliases) != 0 || len(request.Env) != 0 || request.GPUs != 0 || request.StorageMB != 0 || request.CPU != nil || request.MemoryMB != nil {
		return "", errors.New("process runtime supports no mounts, aliases, environment or resource settings")
	}
	if request.ServicePort <= 0 || request.ServicePort > 65535 {
		return "", errors.New("process runtime requires a service port")
	}
	return program, nil
}

// Create makes the private workdir. Nothing runs until Start.
func (p *ProcessRuntime) Create(_ context.Context, request deployment.Request) (string, error) {
	program, err := p.validateRequest(request)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(program); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("process runtime program %s is not an executable file", program)
	}
	if err := os.MkdirAll(filepath.Dir(request.Workdir), 0o700); err != nil {
		return "", err
	}
	if err := os.Mkdir(request.Workdir, 0o700); err != nil {
		return "", fmt.Errorf("reserve process runtime workdir: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runtimes[request.Name] = &processRecord{request: request, program: program}
	return request.Name, nil
}

func (p *ProcessRuntime) record(id string) (*processRecord, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.runtimes[id]
	if current == nil {
		return nil, fmt.Errorf("process runtime %s: %w", id, errMissing)
	}
	return current, nil
}

func (p *ProcessRuntime) Validate(_ context.Context, id string, request deployment.Request, secrets [][]byte) error {
	if _, err := p.validateRequest(request); err != nil {
		return err
	}
	current, err := p.record(id)
	if err != nil {
		return err
	}
	if id != request.Name || current.request.Workdir != request.Workdir || !slices.Equal(current.request.Entrypoint, request.Entrypoint) ||
		!slices.Equal(current.request.Args, request.Args) || current.request.ServicePort != request.ServicePort {
		return errors.New("process runtime differs from request")
	}
	for key, value := range request.Labels {
		if current.request.Labels[key] != value {
			return errors.New("process runtime labels do not match the request")
		}
	}
	for _, secret := range secrets {
		for _, value := range append(slices.Clone(request.Args), request.Workdir) {
			if len(secret) > 0 && bytes.Contains([]byte(value), secret) {
				return errors.New("process runtime secret entered its command line")
			}
		}
	}
	return nil
}

// confine resolves a path inside the runtime's workdir.
func (p *ProcessRuntime) confine(current *processRecord, path string) (string, error) {
	if !cleanAbsolutePath(path) {
		return "", errors.New("process runtime paths must be absolute and clean")
	}
	if path != current.request.Workdir && !strings.HasPrefix(path, current.request.Workdir+"/") {
		return "", errors.New("process runtime paths must stay inside its workdir")
	}
	return path, nil
}

// UploadArchive extracts regular files and directories beneath destination.
func (p *ProcessRuntime) UploadArchive(_ context.Context, id, destination string, archive io.Reader) error {
	current, err := p.record(id)
	if err != nil {
		return err
	}
	root, err := p.confine(current, destination)
	if err != nil {
		return err
	}
	reader := tar.NewReader(archive)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(header.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("archive entry %q leaves its destination", header.Name)
		}
		target := filepath.Join(root, name)
		mode := fs.FileMode(header.Mode) & fs.ModePerm
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, io.LimitReader(reader, header.Size))
			if err := errors.Join(copyErr, file.Close(), os.Chmod(target, mode)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q is not a regular file or directory", header.Name)
		}
	}
}

// DownloadArchive returns one regular file as a single-entry tar stream.
func (p *ProcessRuntime) DownloadArchive(_ context.Context, id, path string) (io.ReadCloser, deployment.FileInfo, error) {
	current, err := p.record(id)
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	source, err := p.confine(current, path)
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	info, err := os.Lstat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, deployment.FileInfo{}, fmt.Errorf("deployment archive: %w: %s", runner.ErrNotFound, path)
	}
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if !info.Mode().IsRegular() {
		return nil, deployment.FileInfo{}, errors.New("process runtime downloads only regular files")
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return nil, deployment.FileInfo{}, err
	}
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if err := writer.WriteHeader(&tar.Header{Name: filepath.Base(source), Mode: int64(info.Mode().Perm()), Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if _, err := writer.Write(content); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	if err := writer.Close(); err != nil {
		return nil, deployment.FileInfo{}, err
	}
	return io.NopCloser(&buffer), deployment.FileInfo{Size: int64(len(content)), Mode: info.Mode()}, nil
}

// Start runs the program in its workdir, in its own process group.
func (p *ProcessRuntime) Start(_ context.Context, id string) error {
	current, err := p.record(id)
	if err != nil {
		return err
	}
	if alive, err := p.alive(current); err != nil || alive {
		return err
	}
	workdir := current.request.Workdir
	stdout, err := os.OpenFile(filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+".stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+".stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	pid, err := p.start(current.program, current.request.Args, workdir, inheritedEnvironment(), stdout, stderr)
	if err != nil {
		return fmt.Errorf("start process runtime: %w", err)
	}
	// Until its start time is recorded, Stop cannot find the process, so a
	// failure here ends its group at once.
	started, alive, err := p.processStart(pid)
	if err != nil {
		return errors.Join(fmt.Errorf("inspect process runtime: %w", err), p.end(&processRecord{pid: pid}))
	}
	if !alive {
		return errors.Join(errors.New("process runtime exited as it started"), p.end(&processRecord{pid: pid}))
	}
	p.mu.Lock()
	current.pid, current.started = pid, started
	p.mu.Unlock()
	// The run's resource source finds the process through this record.
	raw, err := json.Marshal(processFile{Name: current.request.Name, Labels: current.request.Labels, PID: pid, Start: started})
	if err == nil {
		err = writePrivate(processRecordPath(workdir), raw)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("record process runtime: %w", err), p.end(current))
	}
	return nil
}

func processRecordPath(workdir string) string {
	return filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+processRecordSuffix)
}

func (p *ProcessRuntime) alive(current *processRecord) (bool, error) {
	p.mu.Lock()
	pid, started := current.pid, current.started
	p.mu.Unlock()
	if pid <= 0 {
		return false, nil
	}
	now, alive, err := p.processStart(pid)
	if err != nil {
		return false, err
	}
	return alive && now == started, nil
}

func (p *ProcessRuntime) Running(_ context.Context, id string) (bool, error) {
	current, err := p.record(id)
	if errors.Is(err, errMissing) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.alive(current)
}

func (p *ProcessRuntime) Logs(ctx context.Context, id string, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("deployment log bound must be positive")
	}
	var stdout, stderr boundedBuffer
	stdout.limit, stderr.limit = limit, limit
	if err := p.LogsStream(ctx, id, &stdout, &stderr); err != nil {
		return nil, err
	}
	if stdout.exceeded || stderr.exceeded {
		return nil, errors.New("deployment logs exceeded their bound")
	}
	return append(stdout.Bytes(), stderr.Bytes()...), nil
}

func (p *ProcessRuntime) LogsStream(_ context.Context, id string, stdout, stderr io.Writer) error {
	current, err := p.record(id)
	if errors.Is(err, errMissing) {
		return fmt.Errorf("deployment logs: %w: %w", runner.ErrNotFound, err)
	}
	if err != nil {
		return err
	}
	workdir := current.request.Workdir
	var errs []error
	for suffix, destination := range map[string]io.Writer{".stdout.log": stdout, ".stderr.log": stderr} {
		if destination == nil {
			continue
		}
		file, err := os.Open(filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+suffix))
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

// Address returns the loopback address of the published service port.
func (p *ProcessRuntime) Address(_ context.Context, id string, port int) (string, error) {
	current, err := p.record(id)
	if err != nil {
		return "", err
	}
	if port != current.request.ServicePort {
		return "", fmt.Errorf("process runtime publishes no service on port %d", port)
	}
	return net.JoinHostPort(loopbackHost, strconv.Itoa(port)), nil
}

// TaskAddress resolves a port of the running process on the run's loopback.
func (p *ProcessRuntime) TaskAddress(ctx context.Context, id string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", errors.New("invalid task service port")
	}
	running, err := p.Running(ctx, id)
	if err != nil {
		return "", err
	}
	if !running {
		return "", errors.New("process runtime is not running")
	}
	return net.JoinHostPort(loopbackHost, strconv.Itoa(port)), nil
}

// Stop ends the process group, removes the workdir and its logs, and confirms
// both are gone. It is safe to repeat.
func (p *ProcessRuntime) Stop(_ context.Context, id string) error {
	current, err := p.record(id)
	if errors.Is(err, errMissing) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := p.end(current); err != nil {
		return err
	}
	workdir := current.request.Workdir
	var errs []error
	for _, path := range []string{workdir, filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+".stdout.log"), filepath.Join(filepath.Dir(workdir), filepath.Base(workdir)+".stderr.log"), processRecordPath(workdir)} {
		if err := os.RemoveAll(path); err != nil {
			errs = append(errs, err)
		}
		if _, err := os.Lstat(path); err == nil {
			errs = append(errs, fmt.Errorf("%s remains after removal", path))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.runtimes, id)
	p.mu.Unlock()
	return nil
}

// end stops the runtime's process group and confirms that no member is left:
// a process the program started can outlive the program. A group that is
// already gone is left alone.
func (p *ProcessRuntime) end(current *processRecord) error {
	for _, signal := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		if alive, err := p.groupAlive(current); err != nil || !alive {
			return err
		}
		if err := p.signalGroup(current.pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signal process runtime: %w", err)
		}
		deadline := time.Now().Add(stopGrace)
		for time.Now().Before(deadline) {
			if alive, err := p.groupAlive(current); err != nil || !alive {
				return err
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return errors.New("process runtime group remains after kill")
}

// groupAlive reports whether the runtime's process group still has a member.
// The kernel does not reuse a PID while it names a live process group, so a
// group whose leader has exited is still the runtime's, while a live process
// with that PID and another start time is a new one. Without a start time,
// as just after launch, the group alone decides.
func (p *ProcessRuntime) groupAlive(current *processRecord) (bool, error) {
	p.mu.Lock()
	pid, started := current.pid, current.started
	p.mu.Unlock()
	if pid <= 0 {
		return false, nil
	}
	if err := p.signalGroup(pid, 0); errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("probe process runtime group: %w", err)
	}
	if started == 0 {
		return true, nil
	}
	now, alive, err := p.processStart(pid)
	if err != nil {
		return false, err
	}
	return !alive || now == started, nil
}

func inheritedEnvironment() []string {
	var result []string
	for _, name := range processEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func startProcess(path string, args []string, dir string, environment []string, stdout, stderr *os.File) (int, error) {
	command := exec.Command(path, args...)
	command.Dir, command.Env = dir, environment
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return 0, err
	}
	go func() { _ = command.Wait() }()
	return command.Process.Pid, nil
}

func cleanAbsolutePath(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/" && !strings.ContainsRune(path, 0)
}
