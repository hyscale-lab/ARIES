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
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ExecSupervisedStream starts one trusted supervisor directly, without the
// task shell or a cancellation helper. The caller owns descendant supervision
// and must independently confirm it before allowing evaluation. Any error is
// fail-closed: closing the Docker attach alone does not prove process cleanup.
// The container must still match request and be a live task container.
func (manager *Manager) ExecSupervisedStream(ctx context.Context, id string, request deployment.Request, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	s := &execution{client: manager.client, containerID: id, cleanupTimeout: manager.cleanupTimeout()}
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
	if _, err := manager.inspectSupervisedContainer(execCtx, id, request); err != nil {
		return failure(err)
	}
	created, err := s.client.ExecCreate(execCtx, s.containerID, client.ExecCreateOptions{
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
		Cmd: append([]string{command.Path}, command.Args...),
		Env: dockerEnvironment(command.Env), WorkingDir: command.Dir, User: command.User,
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
	go func() { exitDone <- s.waitForExecExit(execCtx, created.ID, execStartTimeout) }()
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

func (s *execution) confirmSupervisedExit(ctx context.Context, execID string) (int, error) {
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

// ConfiguredUser returns the image-configured user of a live task container
// matching request, without executing task code. Empty means none is declared.
func (manager *Manager) ConfiguredUser(ctx context.Context, id string, request deployment.Request) (string, error) {
	config, err := manager.inspectSupervisedContainer(ctx, id, request)
	if err != nil {
		return "", err
	}
	return config.User, nil
}

// inspectSupervisedContainer requires the exact live task container that
// request created: identity, ownership labels, workdir, no-new-privileges,
// command, network, and mounts are all validated before any direct exec.
func (manager *Manager) inspectSupervisedContainer(ctx context.Context, id string, request deployment.Request) (*container.Config, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(id) == "" || request.Labels["aries.run"] == "" || request.Labels["aries.task"] == "" || request.Labels["aries.kind"] != "task-container" || request.Labels["aries.component"] != "sandbox" {
		return nil, errors.New("supervised Docker execution requires an owned task container")
	}
	if err := manager.Validate(ctx, id, request, nil); err != nil {
		return nil, fmt.Errorf("validate supervised Docker container: %w", err)
	}
	inspection, err := manager.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect supervised Docker container: %w", err)
	}
	c := inspection.Container
	if c.ID != id || c.State == nil || !c.State.Running || c.Config == nil || c.Config.WorkingDir != request.Workdir || c.Config.Labels["aries.kind"] != "task-container" || c.Config.Labels["aries.component"] != "sandbox" {
		return nil, errors.New("supervised Docker container identity or live state does not match")
	}
	if request.NoNewPrivileges && (c.HostConfig == nil || !noNewPrivilegesEnabled(c.HostConfig.SecurityOpt)) {
		return nil, errors.New("supervised Docker container no-new-privileges is not enabled")
	}
	return c.Config, nil
}
