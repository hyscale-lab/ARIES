package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

const (
	defaultDockerSocket   = "/var/run/docker.sock"
	defaultCleanupTimeout = 30 * time.Second
	maxExecInput          = 16 << 20
	maxExecOutput         = 16 << 20
	maxConfiguredOutput   = 1 << 30
)

type execution struct {
	client      dockerClient
	containerID string
}

func (manager *Manager) Exec(ctx context.Context, id string, command core.Command) (core.CommandResult, error) {
	return (&execution{client: manager.client, containerID: id}).Exec(ctx, command)
}

func (manager *Manager) ExecStream(ctx context.Context, id string, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	return (&execution{client: manager.client, containerID: id}).ExecStream(ctx, command, stdin, stdout, stderr)
}

// Exec runs argv directly through Docker's exec API. Nonzero exits are results.
func (s *execution) Exec(ctx context.Context, command core.Command) (core.CommandResult, error) {
	started := time.Now()
	if len(command.Stdin) > maxExecInput {
		return core.CommandResult{ExitCode: -1, Duration: time.Since(started)}, fmt.Errorf("Docker exec stdin exceeds %d bytes", maxExecInput)
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

// ExecStream streams a native Docker exec. Cancellation closes the attachment;
// the remote command may continue until it exits or the sandbox is removed.
func (s *execution) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	endCommand := core.TimeCommandStage(ctx, "command.total")
	defer endCommand()
	started := time.Now()
	failure := func() core.CommandResult { return core.CommandResult{ExitCode: -1, Duration: time.Since(started)} }
	if err := validateCommand(command); err != nil {
		return failure(), err
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
	execCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if command.Timeout > 0 {
		var cancelTimeout context.CancelFunc
		execCtx, cancelTimeout = context.WithTimeout(execCtx, command.Timeout)
		defer cancelTimeout()
	}
	endStage := core.TimeCommandStage(ctx, "command.create_exec")
	created, err := s.client.ExecCreate(execCtx, s.containerID, client.ExecCreateOptions{
		AttachStdin: stdin != nil, AttachStdout: true, AttachStderr: true,
		Cmd: append([]string{command.Path}, command.Args...),
		Env: dockerEnvironment(command.Env), WorkingDir: command.Dir, User: command.User,
	})
	endStage()
	if err != nil {
		return failure(), fmt.Errorf("create Docker exec: %w", err)
	}
	endStage = core.TimeCommandStage(ctx, "command.attach_exec")
	attached, err := s.client.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{})
	endStage()
	if err != nil {
		return failure(), fmt.Errorf("attach Docker exec: %w", err)
	}
	core.MarkCommandTiming(ctx, "command.started")
	defer attached.Close()
	stopCancellation := context.AfterFunc(execCtx, attached.Close)
	defer stopCancellation()

	// The caller owns stdin and closes it when its session ends. Like docker
	// exec, output EOF determines completion even if stdin is still open or
	// the command exits before consuming all input.
	go func() {
		if stdin != nil {
			written, _ := io.Copy(attached.Conn, io.LimitReader(stdin, maxExecInput+1))
			if written > maxExecInput {
				cancel(fmt.Errorf("Docker exec stdin exceeds %d bytes", maxExecInput))
			}
		}
		_ = attached.CloseWrite()
	}()
	endStage = core.TimeCommandStage(ctx, "command.output_drain")
	_, err = stdcopy.StdCopy(
		&limitedWriter{writer: stdout, limit: outputLimit},
		&limitedWriter{writer: stderr, limit: outputLimit},
		attached.Reader,
	)
	endStage()
	attached.Close()
	if cause := context.Cause(execCtx); cause != nil {
		return failure(), cause
	}
	if err != nil {
		return failure(), err
	}
	endStage = core.TimeCommandStage(ctx, "command.inspect_exec")
	inspection, err := s.client.ExecInspect(execCtx, created.ID, client.ExecInspectOptions{})
	endStage()
	if err != nil {
		return failure(), fmt.Errorf("inspect Docker exec: %w", err)
	}
	// Output EOF alone does not prove process exit: an exec may close its
	// streams while remaining alive. Keep the native result semantics and mark
	// completion only when inspection confirms the process has exited.
	if !inspection.Running {
		core.MarkCommandTiming(ctx, "command.exited")
		core.MarkCommandTiming(ctx, "command.completed")
	}
	return core.CommandResult{ExitCode: inspection.ExitCode, Duration: time.Since(started)}, nil
}

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

func dockerEnvironment(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, key := range slices.Sorted(maps.Keys(values)) {
		result = append(result, key+"="+values[key])
	}
	return result
}

func validateIdentity(kind, value string) error {
	limit := 128
	if kind == "task" {
		limit = 149
	}
	if value == "" || len(value) > limit {
		return fmt.Errorf("Docker deployment %s ID must contain 1 to %d characters", kind, limit)
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || index > 0 && (character == '-' || character == '_' || character == '.') {
			continue
		}
		return fmt.Errorf("Docker deployment %s ID %q contains an unsafe character", kind, value)
	}
	return nil
}

func validateCommand(command core.Command) error {
	if command.Path == "" || strings.ContainsRune(command.Path, 0) {
		return errors.New("command path must be nonempty and NUL-free")
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
