package sglang

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	forcedProcessWait = 5 * time.Second
	healthAttemptMax  = 5 * time.Second
	healthBodyMax     = 4 << 10
)

type Options struct {
	Executable    string
	ConfigPath    string
	OutputDir     string
	BaseURL       string
	CredentialEnv string
}

type healthError struct {
	category  string
	status    int
	retryable bool
}

func (e *healthError) Error() string {
	if e.status != 0 {
		return fmt.Sprintf("SGLang health failed: %s (HTTP %d)", e.category, e.status)
	}
	return "SGLang health failed: " + e.category
}
func (e *healthError) Retryable() bool { return e.retryable }

type stopAttempt struct {
	done chan struct{}
	err  error
}

type stopError struct {
	err           error
	cleanupFailed bool
}

func (e *stopError) Error() string       { return e.err.Error() }
func (e *stopError) Unwrap() error       { return e.err }
func (e *stopError) CleanupFailed() bool { return e.cleanupFailed }

func joinStopErrors(stopProtocolErr, residualKillErr, closeErr, unexpectedErr error) error {
	err := errors.Join(stopProtocolErr, residualKillErr, closeErr, unexpectedErr)
	if err == nil {
		return nil
	}
	return &stopError{
		err:           err,
		cleanupFailed: stopProtocolErr != nil || residualKillErr != nil || closeErr != nil,
	}
}

func (attempt *stopAttempt) wait(ctx context.Context) error {
	select {
	case <-attempt.done:
		return attempt.err
	default:
	}
	select {
	case <-attempt.done:
		return attempt.err
	case <-ctx.Done():
		select {
		case <-attempt.done:
			return attempt.err
		default:
			return ctx.Err()
		}
	}
}

type Runtime struct {
	mu sync.Mutex

	options       Options
	client        *http.Client
	healthTimeout time.Duration
	waitCommand   func(*exec.Cmd) error
	probeGroup    func(int) error
	signalGroup   func(int, syscall.Signal) error
	command       *exec.Cmd
	stdout        *os.File
	stderr        *os.File
	done          chan struct{}

	stopping        bool
	exitObserved    bool
	killIssued      bool
	residualKillErr error
	closeErr        error
	unexpectedErr   error
	stopAttempt     *stopAttempt
	confirmed       bool
	confirmedErr    error
}

func New(options Options) (*Runtime, error) {
	executable, err := exec.LookPath(options.Executable)
	if err != nil {
		return nil, fmt.Errorf("locate managed SGLang executable: %w", err)
	}
	if executable, err = filepath.Abs(executable); err != nil {
		return nil, fmt.Errorf("resolve managed SGLang executable: %w", err)
	}
	configPath, err := filepath.Abs(options.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("resolve managed SGLang config: %w", err)
	}
	outputDir, err := filepath.Abs(options.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve managed SGLang output: %w", err)
	}
	healthURL, err := deriveHealthURL(options.BaseURL)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		options:       Options{Executable: executable, ConfigPath: configPath, OutputDir: outputDir, BaseURL: healthURL, CredentialEnv: options.CredentialEnv},
		client:        &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		healthTimeout: healthAttemptMax,
		waitCommand:   func(command *exec.Cmd) error { return command.Wait() },
		probeGroup:    func(pid int) error { return syscall.Kill(-pid, 0) },
		signalGroup:   func(pid int, signal syscall.Signal) error { return syscall.Kill(-pid, signal) },
		done:          make(chan struct{}),
	}, nil
}

func deriveHealthURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(baseURL, "#") || u.Path != "/v1" {
		return "", errors.New("SGLang health endpoint configuration is invalid")
	}
	u.Path = "/health"
	return u.String(), nil
}

func (r *Runtime) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.command != nil {
		return errors.New("managed SGLang runtime is already started")
	}
	artifactDir := filepath.Join(r.options.OutputDir, "sglang")
	if err := os.Mkdir(artifactDir, 0o700); err != nil {
		return fmt.Errorf("create managed SGLang artifact directory: %w", err)
	}
	stdout, err := os.OpenFile(filepath.Join(artifactDir, "stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create managed SGLang stdout log: %w", err)
	}
	stderr, err := os.OpenFile(filepath.Join(artifactDir, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = stdout.Close()
		return fmt.Errorf("create managed SGLang stderr log: %w", err)
	}
	command := exec.Command(r.options.Executable, "-m", "sglang.launch_server", "--config", r.options.ConfigPath)
	command.Env = childEnvironment(os.Environ(), r.options.CredentialEnv, filepath.Dir(r.options.Executable))
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return errors.Join(fmt.Errorf("start managed SGLang: %w", err), stdout.Close(), stderr.Close())
	}
	r.command, r.stdout, r.stderr = command, stdout, stderr
	go r.wait()
	return nil
}

func (r *Runtime) wait() {
	r.mu.Lock()
	command, stdout, stderr := r.command, r.stdout, r.stderr
	r.mu.Unlock()
	observeErr := observeProcessExit(command.Process.Pid)
	r.mu.Lock()
	intentional := r.stopping
	r.exitObserved = true
	if !r.killIssued {
		err := r.signalGroup(command.Process.Pid, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			r.killIssued = true
		} else {
			r.residualKillErr = fmt.Errorf("kill residual managed SGLang process group: %w", err)
		}
	}
	r.mu.Unlock()
	waitErr := r.waitCommand(command)
	closeErr := errors.Join(stdout.Close(), stderr.Close())
	r.mu.Lock()
	if observeErr != nil {
		closeErr = errors.Join(closeErr, fmt.Errorf("observe managed SGLang exit: %w", observeErr))
	}
	r.closeErr = closeErr
	if !intentional {
		if waitErr != nil {
			r.unexpectedErr = fmt.Errorf("managed SGLang exited unexpectedly: %w", waitErr)
		} else if observeErr != nil {
			r.unexpectedErr = fmt.Errorf("managed SGLang exit observation failed: %w", observeErr)
		} else {
			r.unexpectedErr = errors.New("managed SGLang exited unexpectedly")
		}
	}
	close(r.done)
	r.mu.Unlock()
}

func (r *Runtime) Done() <-chan struct{} { return r.done }
func (r *Runtime) Err() error            { r.mu.Lock(); defer r.mu.Unlock(); return r.unexpectedErr }

func (r *Runtime) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return &healthError{category: "timeout"}
	}
	timeout := r.healthTimeout
	if timeout <= 0 {
		timeout = healthAttemptMax
	}
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, r.options.BaseURL, nil)
	if err != nil {
		return &healthError{category: "configuration"}
	}
	resp, err := r.client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return &healthError{category: "timeout"}
		}
		if attemptCtx.Err() != nil {
			return &healthError{category: "timeout", retryable: true}
		}
		return &healthError{category: "transport", retryable: true}
	}
	if resp == nil {
		return &healthError{category: "transport", retryable: true}
	}
	if resp.Body == nil {
		return &healthError{category: "response_read"}
	}
	readBytes, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, healthBodyMax+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		if ctx.Err() != nil {
			return &healthError{category: "timeout"}
		}
		if attemptCtx.Err() != nil {
			return &healthError{category: "timeout", retryable: true}
		}
		return &healthError{category: "response_read"}
	}
	if ctx.Err() != nil {
		return &healthError{category: "timeout"}
	}
	if attemptCtx.Err() != nil {
		return &healthError{category: "timeout", retryable: true}
	}
	if closeErr != nil || readBytes > healthBodyMax {
		return &healthError{category: "response_read"}
	}
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	retry := resp.StatusCode == 408 || resp.StatusCode == 425 || resp.StatusCode == 429 || resp.StatusCode >= 500
	category := "http_status"
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		category = "redirect"
		retry = false
	}
	return &healthError{category: category, status: resp.StatusCode, retryable: retry}
}

func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	if r.confirmed {
		err := r.confirmedErr
		r.mu.Unlock()
		return err
	}
	if attempt := r.stopAttempt; attempt != nil {
		r.mu.Unlock()
		return attempt.wait(ctx)
	}
	attempt := &stopAttempt{done: make(chan struct{})}
	r.stopAttempt = attempt
	command := r.command
	if command == nil {
		r.confirmed = true
		r.confirmedErr = nil
		attempt.err = nil
		r.stopAttempt = nil
		close(attempt.done)
		r.mu.Unlock()
		return nil
	}
	pid := command.Process.Pid
	r.mu.Unlock()

	stopProtocolErr := r.stopAttemptRun(ctx, pid)
	r.mu.Lock()
	err := joinStopErrors(stopProtocolErr, r.residualKillErr, r.closeErr, r.unexpectedErr)
	if stopProtocolErr == nil {
		r.confirmed, r.confirmedErr = true, err
	}
	attempt.err, r.stopAttempt = err, nil
	close(attempt.done)
	r.mu.Unlock()
	return err
}

func (r *Runtime) stopAttemptRun(ctx context.Context, pid int) error {
	signaled, err := r.signalOwned(pid, syscall.SIGTERM)
	if err != nil {
		return err
	}
	if !signaled {
		if err := waitForDone(ctx, r.done); err != nil {
			return r.forceCleanup(ctx, pid)
		}
		return r.confirmProcessGroupAbsent(ctx, pid)
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return r.forceCleanup(ctx, pid)
	}
	return r.confirmProcessGroupAbsent(ctx, pid)
}

func (r *Runtime) forceCleanup(ctx context.Context, pid int) error {
	if _, err := r.signalOwned(pid, syscall.SIGKILL); err != nil {
		return err
	}
	confirmationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forcedProcessWait)
	defer cancel()
	if err := waitForDone(confirmationCtx, r.done); err != nil {
		return errors.New("managed SGLang process did not exit after SIGKILL")
	}
	return r.confirmProcessGroupAbsent(confirmationCtx, pid)
}

func waitForDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *Runtime) signalOwned(pid int, signal syscall.Signal) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.exitObserved || isClosed(r.done) {
		return false, nil
	}
	r.stopping = true
	err := r.signalGroup(pid, signal)
	if signal == syscall.SIGKILL && (err == nil || errors.Is(err, syscall.ESRCH)) {
		r.killIssued = true
	}
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		if signal == syscall.SIGTERM {
			return true, fmt.Errorf("terminate managed SGLang process group: %w", err)
		}
		return true, fmt.Errorf("kill managed SGLang process group: %w", err)
	}
	return true, nil
}

func (r *Runtime) confirmProcessGroupAbsent(ctx context.Context, pid int) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := r.probeGroup(pid)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("probe managed SGLang process group: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("managed SGLang process group absence was not confirmed: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func observeProcessExit(pid int) error {
	for {
		err := unix.Waitid(unix.P_PID, pid, &unix.Siginfo{}, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func childEnvironment(environ []string, credentialEnv, executableDir string) []string {
	filtered := make([]string, 0, len(environ)+1)
	pathValue := ""
	for _, entry := range environ {
		name, value, found := strings.Cut(entry, "=")
		if credentialEnv != "" && found && name == credentialEnv {
			continue
		}
		if found && name == "PATH" {
			pathValue = value
			continue
		}
		filtered = append(filtered, entry)
	}
	if credentialEnv == "PATH" || pathValue == "" {
		pathValue = executableDir
	} else {
		pathValue = executableDir + string(os.PathListSeparator) + pathValue
	}
	return append(filtered, "PATH="+pathValue)
}
