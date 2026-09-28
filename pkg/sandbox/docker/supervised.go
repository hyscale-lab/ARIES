package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ExecSupervisedStream starts one trusted supervisor directly, without the
// task shell or a cancellation helper. The caller owns descendant supervision
// and must independently confirm it before allowing evaluation. Any error is
// fail-closed: closing the Docker attach alone does not prove process cleanup.
func (s *Sandbox) ExecSupervisedStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	failure := func(err error) (core.CommandResult, error) {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, err
	}
	if err := validateCommand(command); err != nil {
		return failure(err)
	}
	var execCtx context.Context
	var cancel context.CancelFunc
	if command.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	if _, err := s.inspectSupervisedContainer(execCtx); err != nil {
		return failure(err)
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	execUser := command.User
	if execUser == "" {
		execUser = s.execUser
	}
	created, err := s.client.ExecCreate(execCtx, s.containerID, client.ExecCreateOptions{
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
		Cmd: append([]string{command.Path}, command.Args...),
		Env: dockerEnvironment(command.Env), WorkingDir: command.Dir, User: execUser,
	})
	if err != nil {
		return failure(fmt.Errorf("create supervised Docker exec: %w", err))
	}
	if strings.TrimSpace(created.ID) == "" {
		return failure(errors.New("create supervised Docker exec: empty exec ID"))
	}
	attached, err := s.client.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return failure(fmt.Errorf("attach supervised Docker exec: %w", err))
	}
	var closeAttach sync.Once
	stopAttach := func() { closeAttach.Do(attached.Close) }
	defer stopAttach()
	var closeWrite sync.Once
	closeInput := func() { closeWrite.Do(func() { _ = attached.CloseWrite() }) }
	writeDone := make(chan error, 1)
	if stdin == nil {
		closeInput()
	} else {
		go func() {
			// The private supervisor preamble is separate from the 16 MiB RPC
			// budget. Its fixed allowance is still bounded here.
			const inputLimit = maxExecInput + 1024
			written, err := io.Copy(attached.Conn, io.LimitReader(stdin, inputLimit+1))
			if err == nil && written > inputLimit {
				err = fmt.Errorf("supervised Docker exec stdin exceeds %d bytes", inputLimit)
			}
			closeInput()
			writeDone <- err
		}()
	}
	outputLimit := command.OutputLimitBytes
	if outputLimit == 0 {
		outputLimit = maxExecOutput
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	copyDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&limitedWriter{writer: stdout, limit: outputLimit}, &limitedWriter{writer: stderr, limit: outputLimit}, attached.Reader)
		copyDone <- err
	}()
	exitDone := make(chan error, 1)
	go func() { exitDone <- s.waitForExecExit(execCtx, created.ID) }()
	copyFinished := false
	abort := func(cause error) (core.CommandResult, error) {
		if err := execCtx.Err(); err != nil {
			cause = err
		}
		cancel()
		stopAttach()
		if !copyFinished {
			select {
			case <-copyDone:
				copyFinished = true
			case <-time.After(execDrainTimeout):
				cause = errors.Join(cause, errors.New("supervised Docker output did not drain after attach closed"))
			}
		}
		return failure(cause)
	}
	for waiting := true; waiting; {
		select {
		case <-execCtx.Done():
			return abort(execCtx.Err())
		case err := <-writeDone:
			if err != nil {
				return abort(err)
			}
		case err := <-copyDone:
			copyFinished = true
			if err != nil {
				return abort(err)
			}
		case err := <-exitDone:
			if err != nil {
				return abort(err)
			}
			waiting = false
		}
	}
	closeInput()
	if !copyFinished {
		select {
		case err := <-copyDone:
			copyFinished = true
			if err != nil {
				return abort(err)
			}
		case <-execCtx.Done():
			return abort(execCtx.Err())
		case <-time.After(execDrainTimeout):
			// Docker 29 can retain the hijacked attach after process exit.
			stopAttach()
			select {
			case err := <-copyDone:
				copyFinished = true
				if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
					return abort(err)
				}
			case <-execCtx.Done():
				return abort(execCtx.Err())
			case <-time.After(execDrainTimeout):
				return abort(errors.New("supervised Docker output did not drain after process exit"))
			}
		}
	}
	stopAttach()
	exitCode, err := s.confirmSupervisedExit(execCtx, created.ID)
	if err != nil {
		return abort(err)
	}
	select {
	case err := <-writeDone:
		if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
			return abort(err)
		}
	default:
	}
	return core.CommandResult{ExitCode: exitCode, Duration: time.Since(started)}, nil
}

func (s *Sandbox) confirmSupervisedExit(ctx context.Context, execID string) (int, error) {
	timeout := s.cleanupTimeout
	if timeout <= 0 {
		timeout = defaultCleanupTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		inspection, err := s.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return -1, fmt.Errorf("confirm supervised Docker exec exit: %w", err)
		}
		if inspection.ID != execID || inspection.ContainerID != s.containerID {
			return -1, errors.New("confirm supervised Docker exec exit: exec identity does not match")
		}
		if !inspection.Running {
			if inspection.ExitCode < 0 || inspection.ExitCode > 255 {
				return -1, errors.New("confirm supervised Docker exec exit: invalid exit code")
			}
			return inspection.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, fmt.Errorf("confirm supervised Docker exec exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// TaskUser returns the benchmark's effective user without executing task code.
// Named users remain in Docker's original form for the trusted supervisor to
// resolve before it starts the native task child.
func (s *Sandbox) TaskUser(ctx context.Context) (string, error) {
	config, err := s.inspectSupervisedContainer(ctx)
	if err != nil {
		return "", err
	}
	if s.execUser != "" {
		return s.execUser, nil
	}
	if config.User == "" {
		return rootExecUser, nil
	}
	return config.User, nil
}

func (s *Sandbox) inspectSupervisedContainer(ctx context.Context) (*container.Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	owned := s.owner != nil && s.client != nil && s.containerOwned && !s.stopped && !s.stopping && s.containerID != ""
	s.mu.Unlock()
	if !owned {
		return nil, errors.New("supervised Docker execution requires an owned live sandbox")
	}
	inspection, err := s.client.ContainerInspect(ctx, s.containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect supervised Docker container: %w", err)
	}
	c := inspection.Container
	if c.ID != s.containerID || c.State == nil || !c.State.Running || c.Config == nil || c.Config.WorkingDir != s.workdir || !sameIdentity(c.Config.Labels, s.runID, s.taskID) || c.Config.Labels["aries.kind"] != "task-container" || c.Config.Labels["aries.component"] != "sandbox" {
		return nil, errors.New("supervised Docker container identity or live state does not match")
	}
	if s.execUser != "" && (c.HostConfig == nil || !noNewPrivilegesEnabled(c.HostConfig.SecurityOpt)) {
		return nil, errors.New("supervised Docker container no-new-privileges is not enabled")
	}
	return c.Config, nil
}
