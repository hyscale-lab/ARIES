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
	"sync"
	"syscall"
	"time"
)

const (
	forcedProcessWait = 5 * time.Second
	healthAttemptMax  = 5 * time.Second
	healthBodyMax     = 4 << 10
)

type Options struct {
	Executable string
	ConfigPath string
	OutputDir  string
	BaseURL    string
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

type Runtime struct {
	mu sync.Mutex

	options     Options
	client      *http.Client
	probeGroup  func(int) error
	signalGroup func(int, syscall.Signal) error
	command     *exec.Cmd
	stdout      *os.File
	stderr      *os.File
	done        chan struct{}

	stopping       bool
	waitErr        error
	closeErr       error
	unexpectedErr  error
	stopAttempt    chan struct{}
	stopAttemptErr error
	confirmed      bool
	confirmedErr   error
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
		options:     Options{Executable: executable, ConfigPath: configPath, OutputDir: outputDir, BaseURL: healthURL},
		client:      &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		probeGroup:  func(pid int) error { return syscall.Kill(-pid, 0) },
		signalGroup: func(pid int, signal syscall.Signal) error { return syscall.Kill(-pid, signal) },
		done:        make(chan struct{}),
	}, nil
}

func deriveHealthURL(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "/v1" {
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
	command.Env = append(os.Environ(), "PATH="+filepath.Dir(r.options.Executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
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
	waitErr := command.Wait()
	closeErr := errors.Join(stdout.Close(), stderr.Close())
	r.mu.Lock()
	r.waitErr, r.closeErr = waitErr, closeErr
	if !r.stopping {
		if waitErr != nil {
			r.unexpectedErr = fmt.Errorf("managed SGLang exited unexpectedly: %w", waitErr)
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
		return &healthError{category: "timeout", retryable: true}
	}
	deadline := time.Now().Add(healthAttemptMax)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	attemptCtx, cancel := context.WithDeadline(ctx, deadline)
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
	if readErr != nil || closeErr != nil || readBytes > healthBodyMax {
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
	for {
		r.mu.Lock()
		if r.confirmed {
			err := r.confirmedErr
			r.mu.Unlock()
			return err
		}
		if attempt := r.stopAttempt; attempt != nil {
			r.mu.Unlock()
			<-attempt
			r.mu.Lock()
			err := r.stopAttemptErr
			r.mu.Unlock()
			return err
		}
		attempt := make(chan struct{})
		r.stopAttempt = attempt
		command := r.command
		if command == nil {
			r.confirmed = true
			r.confirmedErr = nil
			r.stopAttemptErr = nil
			r.stopAttempt = nil
			close(attempt)
			r.mu.Unlock()
			return nil
		}
		r.stopping = true
		pid := command.Process.Pid
		done := isClosed(r.done)
		r.mu.Unlock()

		err := r.stopAttemptRun(ctx, pid, done)
		r.mu.Lock()
		if err == nil {
			err = errors.Join(r.closeErr, r.unexpectedErr)
			r.confirmed, r.confirmedErr = true, err
		}
		r.stopAttemptErr, r.stopAttempt = err, nil
		close(attempt)
		r.mu.Unlock()
		return err
	}
}

func (r *Runtime) stopAttemptRun(ctx context.Context, pid int, done bool) error {
	if !done {
		if err := r.signalGroup(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("terminate managed SGLang process group: %w", err)
		}
		select {
		case <-r.done:
		case <-ctx.Done():
			if err := r.signalGroup(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill managed SGLang process group: %w", err)
			}
			select {
			case <-r.done:
			case <-time.After(forcedProcessWait):
				return errors.New("managed SGLang process did not exit after SIGKILL")
			}
		}
	}
	if !r.processGroupAbsent(pid) {
		return errors.New("managed SGLang process group absence was not confirmed")
	}
	return nil
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
func (r *Runtime) processGroupAbsent(pid int) bool {
	deadline := time.NewTimer(forcedProcessWait)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := r.probeGroup(pid)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if err != nil {
			return false
		}
		select {
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}
