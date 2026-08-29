// Package kubernetes implements the ARIES Tool Sandbox on top of Kubernetes
// pods. It drives the cluster through the kubectl binary rather than a
// client-go dependency: each task gets its own pod, commands run via
// `kubectl exec`, and files move via `kubectl cp` / streamed exec.
//
// This backend satisfies the same interfaces as the Docker backend
// (runner.ToolSandbox / runner.Sandbox plus the filesystem and attached-process
// surface the OpenClaw E2B bridge type-asserts), so it is selected purely by
// `sandbox.type = "kubernetes"` in a profile.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	arsandbox "github.com/hyscale-lab/aries/pkg/sandbox"
	"github.com/sirupsen/logrus"
)

const (
	defaultNamespace      = "aries"
	defaultKubectl        = "kubectl"
	defaultReadyTimeout   = 120 * time.Second
	defaultCleanupTimeout = 60 * time.Second
	pidFilePrefix         = "/tmp/.aries-k8s-"
	maxExecOutput         = 16 << 20
)

var (
	_ runner.ToolSandbox = (*Manager)(nil)
	_ runner.Sandbox     = (*Sandbox)(nil)
)

// Options are the host-local inputs to the Kubernetes sandbox manager.
type Options struct {
	OutputDir      string
	Namespace      string
	KubectlPath    string
	ReadyTimeout   time.Duration
	CleanupTimeout time.Duration
	Logger         *logrus.Logger
}

// Manager creates one isolated pod per task in a fixed namespace.
type Manager struct {
	outputDir      string
	namespace      string
	kubectl        string
	readyTimeout   time.Duration
	cleanupTimeout time.Duration
	logger         *logrus.Logger
	newID          func() (string, error)
}

// Sandbox is a live Kubernetes task environment backed by one pod.
type Sandbox struct {
	owner          *Manager
	namespace      string
	podName        string
	workdir        string
	artifactDir    string
	runID          string
	taskID         string
	cleanupTimeout time.Duration
}

// processHandle is the backend-specific identity carried in
// arsandbox.ProcessRef.Handle for an attached Kubernetes process.
type processHandle struct {
	generation string
	pidFile    string
}

// New constructs a Kubernetes manager without contacting the cluster. The
// kubectl binary and its resolved kubeconfig/context are the cluster contract.
func New(options Options) (*Manager, error) {
	if strings.TrimSpace(options.OutputDir) == "" {
		return nil, errors.New("kubernetes sandbox output directory is required")
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve kubernetes sandbox output directory: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, fmt.Errorf("create kubernetes sandbox output directory: %w", err)
	}
	if options.Namespace == "" {
		options.Namespace = defaultNamespace
	}
	if options.KubectlPath == "" {
		options.KubectlPath = defaultKubectl
	}
	if options.ReadyTimeout <= 0 {
		options.ReadyTimeout = defaultReadyTimeout
	}
	if options.CleanupTimeout <= 0 {
		options.CleanupTimeout = defaultCleanupTimeout
	}
	if options.Logger == nil {
		options.Logger = logrus.StandardLogger()
	}
	return &Manager{
		outputDir:      outputDir,
		namespace:      options.Namespace,
		kubectl:        options.KubectlPath,
		readyTimeout:   options.ReadyTimeout,
		cleanupTimeout: options.CleanupTimeout,
		logger:         options.Logger,
		newID:          randomID,
	}, nil
}

// Close releases manager-level resources. The kubectl backend holds none.
func (m *Manager) Close() error { return nil }

// Start creates one task pod and waits for it to become Ready.
func (m *Manager) Start(ctx context.Context, request core.SandboxRequest) (runner.Sandbox, error) {
	if err := validateIdentity("run", request.RunID); err != nil {
		return nil, err
	}
	if err := validateIdentity("task", request.TaskID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Environment.Image) == "" {
		return nil, errors.New("kubernetes sandbox requires an environment image")
	}
	workdir := request.Environment.Workdir
	if workdir == "" {
		workdir = "/"
	}
	id, err := m.newID()
	if err != nil {
		return nil, fmt.Errorf("generate kubernetes sandbox ID: %w", err)
	}
	sandbox := &Sandbox{
		owner:          m,
		namespace:      m.namespace,
		podName:        "aries-task-" + id,
		workdir:        workdir,
		artifactDir:    filepath.Join(m.outputDir, request.TaskID, "sandbox"),
		runID:          request.RunID,
		taskID:         request.TaskID,
		cleanupTimeout: m.cleanupTimeout,
	}
	if err := os.MkdirAll(sandbox.artifactDir, 0o700); err != nil {
		return nil, fmt.Errorf("create kubernetes sandbox artifact directory: %w", err)
	}

	manifest, err := podManifest(sandbox, request)
	if err != nil {
		return nil, err
	}
	if _, err := m.runInput(ctx, manifest, "apply", "-f", "-"); err != nil {
		return nil, fmt.Errorf("apply kubernetes task pod: %w", err)
	}

	readyCtx, cancel := context.WithTimeout(ctx, m.readyTimeout)
	defer cancel()
	if _, err := m.run(readyCtx, "wait", "-n", sandbox.namespace,
		"--for=condition=Ready", "pod/"+sandbox.podName,
		"--timeout="+strconv.Itoa(int(m.readyTimeout.Seconds()))+"s"); err != nil {
		return nil, errors.Join(fmt.Errorf("wait for kubernetes task pod Ready: %w", err), sandbox.stop(ctx))
	}
	return sandbox, nil
}

// Stop deletes the task pod and confirms it is gone.
func (m *Manager) Stop(ctx context.Context, live runner.Sandbox) error {
	sandbox, ok := live.(*Sandbox)
	if !ok || sandbox.owner != m {
		return errors.New("kubernetes Stop received a foreign sandbox")
	}
	return sandbox.stop(ctx)
}

func (s *Sandbox) stop(ctx context.Context) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cleanupTimeout)
	defer cancel()
	if _, err := s.owner.run(cleanupCtx, "delete", "pod", "-n", s.namespace, s.podName,
		"--ignore-not-found", "--now"); err != nil {
		return fmt.Errorf("delete kubernetes task pod: %w", err)
	}
	// Positively confirm absence.
	if out, err := s.owner.run(cleanupCtx, "get", "pod", "-n", s.namespace, s.podName,
		"--ignore-not-found", "-o", "name"); err != nil {
		return fmt.Errorf("confirm kubernetes task pod absent: %w", err)
	} else if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("kubernetes task pod %q still present after delete", s.podName)
	}
	return nil
}

// --- runner.Sandbox ---------------------------------------------------------

// Exec runs a command to completion and returns buffered output.
func (s *Sandbox) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if err := validateCommand(command); err != nil {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	var stdout, stderr bytes.Buffer
	exit, err := s.execStream(ctx, command, nil,
		&limitedWriter{writer: &stdout, limit: maxExecOutput},
		&limitedWriter{writer: &stderr, limit: maxExecOutput})
	result := core.CommandResult{ExitCode: exit, Duration: time.Since(started), Stdout: stdout.String(), Stderr: stderr.String()}
	return result, err
}

// Upload copies a host file into the sandbox pod.
func (s *Sandbox) Upload(ctx context.Context, source, destination string) error {
	if _, err := validatePath(destination, false); err != nil {
		return fmt.Errorf("invalid kubernetes upload destination: %w", err)
	}
	_, err := s.owner.run(ctx, "cp", source, s.namespace+"/"+s.podName+":"+destination)
	return err
}

// Download copies a sandbox file out to the host.
func (s *Sandbox) Download(ctx context.Context, source, destination string) error {
	if _, err := validatePath(source, false); err != nil {
		return fmt.Errorf("invalid kubernetes download source: %w", err)
	}
	_, err := s.owner.run(ctx, "cp", s.namespace+"/"+s.podName+":"+source, destination)
	return err
}

// --- grantSandbox (bridge routing/auth surface) -----------------------------

// NetworkName identifies the sandbox's network scope. For Kubernetes this is
// the namespace/pod pair.
func (s *Sandbox) NetworkName() string { return s.namespace + "/" + s.podName }

// NetworkGateway returns the pod IP. NOTE: the OpenClaw E2B bridge currently
// authorizes requests by matching this against the connection's origin and uses
// it to build the bridge endpoint address. On Kubernetes the address a pod uses
// to reach the ARIES bridge is not the pod IP, so bridge reachability/auth for
// the E2B pairing still needs adapting for the cluster network model.
func (s *Sandbox) NetworkGateway(ctx context.Context) (string, error) {
	out, err := s.owner.run(ctx, "get", "pod", "-n", s.namespace, s.podName,
		"-o", "jsonpath={.status.podIP}")
	if err != nil {
		return "", fmt.Errorf("resolve kubernetes pod IP: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", errors.New("kubernetes pod has no assigned IP yet")
	}
	return ip, nil
}

// TaskID returns the stable task identity.
func (s *Sandbox) TaskID() string { return s.taskID }

// Workdir returns the sandbox working directory.
func (s *Sandbox) Workdir() string { return s.workdir }

// RunID returns the stable run identity.
func (s *Sandbox) RunID() string { return s.runID }

// ContainerID identifies the backing pod. Kubernetes has no Docker container ID;
// the namespaced pod name is the stable equivalent.
func (s *Sandbox) ContainerID() string { return s.namespace + "/" + s.podName }

// ContainerName returns the pod name.
func (s *Sandbox) ContainerName() string { return s.podName }

// ExecStream is the streaming exec form used by the SSH bridge: it runs a
// command with the given stdin/stdout/stderr and returns its exit code.
func (s *Sandbox) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	if err := validateCommand(command); err != nil {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	exit, err := s.execStream(ctx, command, stdin, stdout, stderr)
	return core.CommandResult{ExitCode: exit, Duration: time.Since(started)}, err
}

// --- filesystemSandbox ------------------------------------------------------

// ReadFile returns the exact bytes of one file in the sandbox.
func (s *Sandbox) ReadFile(ctx context.Context, source string) ([]byte, error) {
	clean, err := validatePath(source, false)
	if err != nil {
		return nil, fmt.Errorf("invalid kubernetes read path: %w", err)
	}
	var stdout bytes.Buffer
	exit, err := s.execStream(ctx, core.Command{Path: "cat", Args: []string{clean}}, nil,
		&limitedWriter{writer: &stdout, limit: maxBridgeFileSize}, io.Discard)
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("read kubernetes file %q (exit %d): %w", clean, exit, err)
	}
	return stdout.Bytes(), nil
}

// WriteFile writes bytes to one file in the sandbox, creating or truncating it.
func (s *Sandbox) WriteFile(ctx context.Context, destination string, content []byte) error {
	clean, err := validatePath(destination, true)
	if err != nil {
		return fmt.Errorf("invalid kubernetes write path: %w", err)
	}
	script := "cat > " + shellQuote(clean)
	exit, err := s.execStream(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", script}},
		bytes.NewReader(content), io.Discard, io.Discard)
	if err != nil || exit != 0 {
		return fmt.Errorf("write kubernetes file %q (exit %d): %w", clean, exit, err)
	}
	return nil
}

// StatPath returns metadata for one path.
func (s *Sandbox) StatPath(ctx context.Context, target string) (arsandbox.FileInfo, error) {
	clean, err := validatePath(target, false)
	if err != nil {
		return arsandbox.FileInfo{}, fmt.Errorf("invalid kubernetes stat path: %w", err)
	}
	var stdout bytes.Buffer
	// %s size, %f raw-hex mode, %Y mtime epoch, %F human type.
	exit, err := s.execStream(ctx, core.Command{Path: "stat", Args: []string{"-c", "%s|%f|%Y|%F", clean}}, nil,
		&stdout, io.Discard)
	if err != nil || exit != 0 {
		return arsandbox.FileInfo{}, fmt.Errorf("stat kubernetes path %q (exit %d): %w", clean, exit, err)
	}
	info, err := parseStat(clean, path.Base(clean), strings.TrimSpace(stdout.String()))
	if err != nil {
		return arsandbox.FileInfo{}, err
	}
	if info.Type == "symlink" {
		info.LinkTarget = s.readlink(ctx, clean)
	}
	return info, nil
}

// ListDir lists one directory's immediate entries.
func (s *Sandbox) ListDir(ctx context.Context, target string) ([]arsandbox.FileInfo, error) {
	clean, err := validatePath(target, false)
	if err != nil {
		return nil, fmt.Errorf("invalid kubernetes list path: %w", err)
	}
	// One shell pass: for each entry emit "name|size|modehex|mtime|type".
	script := "cd " + shellQuote(clean) + " && ls -A1 | while IFS= read -r n; do stat -c \"$n|%s|%f|%Y|%F\" \"$n\"; done"
	var stdout bytes.Buffer
	exit, err := s.execStream(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", script}}, nil, &stdout, io.Discard)
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("list kubernetes dir %q (exit %d): %w", clean, exit, err)
	}
	entries := make([]arsandbox.FileInfo, 0)
	for _, line := range strings.Split(stdout.String(), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		name, rest, ok := strings.Cut(line, "|")
		if !ok {
			continue
		}
		info, err := parseStat(path.Join(clean, name), name, rest)
		if err != nil {
			continue
		}
		entries = append(entries, info)
	}
	return entries, nil
}

// MakeDir creates a directory (and parents) in the sandbox.
func (s *Sandbox) MakeDir(ctx context.Context, target string) error {
	return s.filesystemCommand(ctx, target, "mkdir", "-p")
}

// RemovePath removes a path recursively in the sandbox.
func (s *Sandbox) RemovePath(ctx context.Context, target string) error {
	return s.filesystemCommand(ctx, target, "rm", "-rf")
}

// MovePath moves/renames a path in the sandbox.
func (s *Sandbox) MovePath(ctx context.Context, source, destination string) error {
	src, err := validatePath(source, true)
	if err != nil {
		return fmt.Errorf("invalid kubernetes move source: %w", err)
	}
	dst, err := validatePath(destination, true)
	if err != nil {
		return fmt.Errorf("invalid kubernetes move destination: %w", err)
	}
	exit, err := s.execStream(ctx, core.Command{Path: "mv", Args: []string{src, dst}}, nil, io.Discard, io.Discard)
	if err != nil || exit != 0 {
		return fmt.Errorf("move kubernetes path %q -> %q (exit %d): %w", src, dst, exit, err)
	}
	return nil
}

func (s *Sandbox) filesystemCommand(ctx context.Context, target, name string, args ...string) error {
	clean, err := validatePath(target, true)
	if err != nil {
		return fmt.Errorf("invalid kubernetes %s path: %w", name, err)
	}
	exit, err := s.execStream(ctx, core.Command{Path: name, Args: append(append([]string(nil), args...), clean)}, nil, io.Discard, io.Discard)
	if err != nil || exit != 0 {
		return fmt.Errorf("kubernetes %s %q (exit %d): %w", name, clean, exit, err)
	}
	return nil
}

func (s *Sandbox) readlink(ctx context.Context, target string) string {
	var stdout bytes.Buffer
	if exit, err := s.execStream(ctx, core.Command{Path: "readlink", Args: []string{target}}, nil, &stdout, io.Discard); err != nil || exit != 0 {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

// --- processSandbox (attached process surface) ------------------------------

// ExecProcessStream runs one attached process, reporting its PID via onStart
// before streaming stdout/stderr, then returns its exit code.
func (s *Sandbox) ExecProcessStream(ctx context.Context, command core.Command, stdout, stderr io.Writer, onStart func(arsandbox.ProcessRef) error) (core.CommandResult, error) {
	started := time.Now()
	failure := func() core.CommandResult { return core.CommandResult{ExitCode: -1, Duration: time.Since(started)} }
	if err := validateCommand(command); err != nil {
		return failure(), err
	}
	if onStart == nil {
		return failure(), errors.New("kubernetes process start callback is required")
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	token, err := randomID()
	if err != nil {
		return failure(), fmt.Errorf("generate kubernetes process token: %w", err)
	}
	pidFile := pidFilePrefix + token + ".pid"

	dir := command.Dir
	if dir == "" {
		dir = s.workdir
	}
	// The child records its own PID, then execs the real command so the PID we
	// report is the one running the user process. stdout/stderr stay clean.
	inner := "echo $$ > " + shellQuote(pidFile) + "; exec " + shellCommand(command)
	script := "cd " + shellQuote(dir) + " 2>/dev/null; " + inner

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Poll for the PID file and fire onStart exactly once.
	startResult := make(chan error, 1)
	go func() {
		ref, ok := s.awaitPID(execCtx, pidFile, token)
		if !ok {
			startResult <- errors.New("kubernetes process exited without reporting a PID")
			return
		}
		startResult <- onStart(ref)
	}()

	exit, runErr := s.execStreamEnv(execCtx, command.Env,
		[]string{"/bin/sh", "-c", script}, nil, stdout, stderr)

	cancel()
	startErr := <-startResult
	if startErr != nil {
		if runErr == nil {
			runErr = startErr
		}
		return core.CommandResult{ExitCode: exit, Duration: time.Since(started)}, runErr
	}
	return core.CommandResult{ExitCode: exit, Duration: time.Since(started)}, runErr
}

func (s *Sandbox) awaitPID(ctx context.Context, pidFile, token string) (arsandbox.ProcessRef, bool) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		var stdout bytes.Buffer
		exit, err := s.execStream(ctx, core.Command{Path: "cat", Args: []string{pidFile}}, nil, &stdout, io.Discard)
		if err == nil && exit == 0 {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(stdout.String())); convErr == nil && pid > 1 {
				return arsandbox.ProcessRef{PID: pid, Handle: processHandle{generation: token, pidFile: pidFile}}, true
			}
		}
		select {
		case <-ctx.Done():
			return arsandbox.ProcessRef{}, false
		case <-ticker.C:
		}
	}
}

// SendProcessSignal delivers TERM or KILL to the referenced process.
func (s *Sandbox) SendProcessSignal(ctx context.Context, ref arsandbox.ProcessRef, signal string) error {
	handle, ok := ref.Handle.(processHandle)
	if !ok || ref.PID <= 1 || handle.generation == "" {
		return errors.New("invalid kubernetes process reference")
	}
	var shellSignal string
	switch signal {
	case "SIGNAL_SIGTERM":
		shellSignal = "TERM"
	case "SIGNAL_SIGKILL":
		shellSignal = "KILL"
	default:
		return fmt.Errorf("unsupported process signal %q", signal)
	}
	exit, err := s.execStream(ctx, core.Command{Path: "kill", Args: []string{"-s", shellSignal, strconv.Itoa(ref.PID)}}, nil, io.Discard, io.Discard)
	if err != nil || exit != 0 {
		return fmt.Errorf("signal kubernetes process %d (exit %d): %w", ref.PID, exit, err)
	}
	return nil
}

// TerminateProcess sends TERM then KILL to the referenced process.
func (s *Sandbox) TerminateProcess(ctx context.Context, ref arsandbox.ProcessRef) error {
	handle, ok := ref.Handle.(processHandle)
	if !ok || ref.PID <= 1 || handle.generation == "" {
		return errors.New("invalid kubernetes process reference")
	}
	script := fmt.Sprintf("kill -TERM %d 2>/dev/null; sleep 0.2; kill -KILL %d 2>/dev/null; rm -f %s; exit 0",
		ref.PID, ref.PID, shellQuote(handle.pidFile))
	_, err := s.execStream(ctx, core.Command{Path: "/bin/sh", Args: []string{"-c", script}}, nil, io.Discard, io.Discard)
	return err
}

// --- kubectl exec plumbing --------------------------------------------------

func (s *Sandbox) execStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	argv := append([]string{command.Path}, command.Args...)
	return s.execStreamEnv(ctx, command.Env, argv, stdin, stdout, stderr)
}

// execStreamEnv runs argv inside the pod, injecting env as leading `env K=V`
// tokens (argv, so no shell quoting is needed for the values).
func (s *Sandbox) execStreamEnv(ctx context.Context, env map[string]string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	kubectlArgs := []string{"exec", "-n", s.namespace}
	if stdin != nil {
		kubectlArgs = append(kubectlArgs, "-i")
	}
	kubectlArgs = append(kubectlArgs, s.podName, "--")
	if len(env) > 0 {
		kubectlArgs = append(kubectlArgs, "env")
		for _, kv := range sortedEnv(env) {
			kubectlArgs = append(kubectlArgs, kv)
		}
	}
	kubectlArgs = append(kubectlArgs, argv...)

	cmd := exec.CommandContext(ctx, s.owner.kubectl, kubectlArgs...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// kubectl propagates the remote command's exit code.
		return exitErr.ExitCode(), nil
	}
	return -1, err
}

// run executes a kubectl subcommand and returns its stdout.
func (m *Manager) run(ctx context.Context, args ...string) ([]byte, error) {
	return m.runInput(ctx, nil, args...)
}

func (m *Manager) runInput(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.kubectl, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func randomID() (string, error) {
	value := make([]byte, 8)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
