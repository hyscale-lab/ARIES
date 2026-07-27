package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const forcedProcessWait = 5 * time.Second

type modelRuntime interface {
	Start(context.Context) error
	Stop(context.Context) error
	Done() <-chan struct{}
}

type externalModelRuntime struct{}

func (externalModelRuntime) Start(context.Context) error { return nil }
func (externalModelRuntime) Stop(context.Context) error  { return nil }
func (externalModelRuntime) Done() <-chan struct{}       { return nil }

type sglangProcessOptions struct {
	Executable string
	ConfigPath string
	OutputDir  string
}

type sglangProcessRuntime struct {
	mu       sync.Mutex
	options  sglangProcessOptions
	command  *exec.Cmd
	done     chan struct{}
	waitErr  error
	closeErr error
	stopped  bool
	stopErr  error
	stdout   *os.File
	stderr   *os.File
}

func newSGLangProcessRuntime(options sglangProcessOptions) (*sglangProcessRuntime, error) {
	executable, err := exec.LookPath(options.Executable)
	if err != nil {
		return nil, fmt.Errorf("locate managed SGLang executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
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
	return &sglangProcessRuntime{
		options: sglangProcessOptions{Executable: executable, ConfigPath: configPath, OutputDir: outputDir},
		done:    make(chan struct{}),
	}, nil
}

func (runtime *sglangProcessRuntime) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.command != nil {
		return errors.New("managed SGLang runtime is already started")
	}
	artifactDir := filepath.Join(runtime.options.OutputDir, "sglang")
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
	command := exec.Command(runtime.options.Executable, "-m", "sglang.launch_server", "--config", runtime.options.ConfigPath)
	command.Env = append(os.Environ(), "PATH="+filepath.Dir(runtime.options.Executable)+string(os.PathListSeparator)+os.Getenv("PATH"))
	command.Stdout = stdout
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return errors.Join(fmt.Errorf("start managed SGLang: %w", err), stdout.Close(), stderr.Close())
	}
	runtime.command = command
	runtime.stdout = stdout
	runtime.stderr = stderr
	go runtime.wait()
	return nil
}

func (runtime *sglangProcessRuntime) wait() {
	waitErr := runtime.command.Wait()
	closeErr := errors.Join(runtime.stdout.Close(), runtime.stderr.Close())
	runtime.mu.Lock()
	runtime.waitErr = waitErr
	runtime.closeErr = closeErr
	close(runtime.done)
	runtime.mu.Unlock()
}

func (runtime *sglangProcessRuntime) Done() <-chan struct{} {
	return runtime.done
}

func (runtime *sglangProcessRuntime) Stop(ctx context.Context) error {
	runtime.mu.Lock()
	if runtime.stopped {
		err := runtime.stopErr
		runtime.mu.Unlock()
		return err
	}
	if runtime.command == nil {
		runtime.stopped = true
		runtime.mu.Unlock()
		return nil
	}
	pid := runtime.command.Process.Pid
	alreadyExited := false
	select {
	case <-runtime.done:
		alreadyExited = true
	default:
	}
	runtime.mu.Unlock()

	if !alreadyExited {
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			stopErr := fmt.Errorf("terminate managed SGLang process group: %w", err)
			runtime.recordStop(stopErr, false)
			return stopErr
		}
		select {
		case <-runtime.done:
		case <-ctx.Done():
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				stopErr := fmt.Errorf("kill managed SGLang process group: %w", err)
				runtime.recordStop(stopErr, false)
				return stopErr
			}
			timer := time.NewTimer(forcedProcessWait)
			defer timer.Stop()
			select {
			case <-runtime.done:
			case <-timer.C:
				stopErr := errors.New("managed SGLang process did not exit after SIGKILL")
				runtime.recordStop(stopErr, false)
				return stopErr
			}
		}
	}
	if !processGroupAbsent(pid, forcedProcessWait) {
		stopErr := errors.New("managed SGLang process group absence was not confirmed")
		runtime.recordStop(stopErr, false)
		return stopErr
	}

	runtime.mu.Lock()
	var stopErr error
	if alreadyExited && runtime.waitErr != nil {
		stopErr = fmt.Errorf("managed SGLang exited unexpectedly: %w", runtime.waitErr)
	}
	stopErr = errors.Join(stopErr, runtime.closeErr)
	runtime.mu.Unlock()
	runtime.recordStop(stopErr, true)
	return stopErr
}

func (runtime *sglangProcessRuntime) recordStop(err error, confirmed bool) {
	runtime.mu.Lock()
	runtime.stopped = confirmed
	runtime.stopErr = err
	runtime.mu.Unlock()
}

func processGroupAbsent(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if err != nil || !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
