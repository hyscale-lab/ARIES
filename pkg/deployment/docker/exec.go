package docker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultDockerSocket   = "/var/run/docker.sock"
	defaultCleanupTimeout = 30 * time.Second
	maxExecInput          = 16 << 20
	maxExecOutput         = 16 << 20
	maxConfiguredOutput   = 1 << 30
	execPollInterval      = 20 * time.Millisecond
	execDrainTimeout      = 200 * time.Millisecond
	execStartTimeout      = 30 * time.Second
	execTrailerKeep       = 128
	execStatePrefix       = "/tmp/.aries-exec-"
	rootExecUser          = "0:0"
	execShell             = `state=$1; token=$2; shift 2; umask 077; trap 'rm -f "$state" "$state.tmp"' EXIT; exec 3<&0; setsid "$@" <&3 & pid=$!; printf '%s\n' "$pid" >"$state.tmp" || exit 125; mv "$state.tmp" "$state" || exit 125; wait "$pid"; status=$?; rm -f "$state" "$state.tmp"; trap - EXIT; printf '\036ARIES_EXEC_EXIT_%s=%d\037' "$token" "$status" >&2; exit "$status"`
	cancelExecShell       = `state=$1; attempts=0; while [ ! -r "$state" ]; do attempts=$((attempts+1)); [ "$attempts" -ge 200 ] && exit 70; sleep 0.01; done; IFS= read -r pgid <"$state" || exit 71; case "$pgid" in ''|*[!0-9]*|0|1) exit 71;; esac; kill -TERM "-$pgid" 2>/dev/null || :; sleep 0.2; kill -KILL "-$pgid" 2>/dev/null || :; rm -f "$state"; exit 0`
)

type execution struct {
	client         dockerClient
	containerID    string
	cleanupTimeout time.Duration
}

func (manager *Manager) Exec(ctx context.Context, id string, command core.Command) (core.CommandResult, error) {
	return (&execution{client: manager.client, containerID: id, cleanupTimeout: manager.cleanupTimeout()}).Exec(ctx, command)
}
func (manager *Manager) ExecStream(ctx context.Context, id string, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	return (&execution{client: manager.client, containerID: id, cleanupTimeout: manager.cleanupTimeout()}).ExecStream(ctx, command, stdin, stdout, stderr)
}
func (manager *Manager) cleanupTimeout() time.Duration {
	if manager.timeout > 0 {
		return manager.timeout
	}
	return defaultCleanupTimeout
}

// Exec runs one argv directly through Docker's typed exec API. Nonzero exits
// are returned as results, not transport errors.
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

// ExecStream is the bridge-facing streaming form of Exec. It starts reading
// output while stdin is still arriving, so interactive SSH commands cannot
// deadlock on full pipes.
func (s *execution) ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	started := time.Now()
	failure := func() core.CommandResult { return core.CommandResult{ExitCode: -1, Duration: time.Since(started)} }
	if err := validateCommand(command); err != nil {
		return failure(), err
	}
	outputLimit := command.OutputLimitBytes
	if outputLimit == 0 {
		outputLimit = maxExecOutput
	}

	attachInput := stdin != nil
	if !attachInput {
		stdin = bytes.NewReader(nil)
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	execCtx := ctx
	cancel := func() {}
	if command.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, command.Timeout)
	}
	defer cancel()

	token, err := randomID()
	if err != nil {
		return failure(), fmt.Errorf("generate Docker exec exit token: %w", err)
	}
	statePath := execStatePrefix + token
	execUser := command.User

	created, err := s.client.ExecCreate(execCtx, s.containerID, client.ExecCreateOptions{
		AttachStdin: attachInput, AttachStdout: true, AttachStderr: true,
		Cmd: wrappedCommand(statePath, token, command),
		Env: dockerEnvironment(command.Env), WorkingDir: command.Dir, User: execUser,
	})
	if err != nil {
		return failure(), fmt.Errorf("create Docker exec: %w", err)
	}
	attached, err := s.client.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return failure(), fmt.Errorf("attach Docker exec: %w", err)
	}
	defer attached.Close()

	readDone := make(chan struct{})
	go func() {
		select {
		case <-execCtx.Done():
			attached.Close()
		case <-readDone:
		}
	}()
	var closeWrite sync.Once
	closeDockerInput := func() { closeWrite.Do(func() { _ = attached.CloseWrite() }) }
	writeDone := make(chan error, 1)
	go func() {
		limited := io.LimitReader(stdin, maxExecInput+1)
		written, writeErr := io.Copy(attached.Conn, limited)
		if writeErr == nil && written > maxExecInput {
			writeErr = fmt.Errorf("Docker exec stdin exceeds %d bytes", maxExecInput)
		}
		closeDockerInput()
		writeDone <- writeErr
	}()

	copyDone := make(chan error, 1)
	exitTrailer := newExitTrailer(&limitedWriter{writer: stderr, limit: outputLimit}, token)
	go func() {
		_, copyErr := stdcopy.StdCopy(
			&limitedWriter{writer: stdout, limit: outputLimit},
			exitTrailer,
			attached.Reader,
		)
		copyDone <- copyErr
	}()
	inspectCtx, cancelInspect := context.WithCancel(execCtx)
	defer cancelInspect()
	exitDone := make(chan error, 1)
	go func() { exitDone <- s.waitForExecExit(inspectCtx, created.ID, execStartTimeout) }()
	copyFinished := false
	var stopRead sync.Once
	stopReading := func() {
		stopRead.Do(func() {
			close(readDone)
			attached.Close()
		})
	}
	abort := func(cause error) (core.CommandResult, error) {
		stopReading()
		cancelInspect()
		if !copyFinished {
			<-copyDone
			copyFinished = true
		}
		// Closing the attach can make a stream-copy error race with cancellation.
		// Cancellation owns the result once it is observable: the caller needs to
		// know whether targeted termination was confirmed, not which attach error
		// happened to win the select.
		contextErr := execCtx.Err()
		terminateCtx, terminateCancel := context.WithTimeout(context.WithoutCancel(execCtx), s.cleanupTimeout)
		defer terminateCancel()
		terminateErr := s.terminateExec(terminateCtx, created.ID, statePath)
		if contextErr != nil {
			if terminateErr == nil {
				return failure(), contextErr
			}
			return failure(), errors.Join(contextErr, terminateErr)
		}
		if terminateErr == nil {
			return failure(), cause
		}
		return failure(), errors.Join(cause, terminateErr)
	}

	var copyErr error
	var observedErr error
	waiting := true
	for waiting {
		select {
		case <-execCtx.Done():
			return abort(execCtx.Err())
		case err := <-copyDone:
			copyFinished, copyErr = true, err
			if err != nil {
				return abort(err)
			}
		case observedErr = <-exitDone:
			waiting = false
		}
	}
	if observedErr != nil {
		return abort(observedErr)
	}
	closeDockerInput()
	forcedClose := false
	if !copyFinished {
		select {
		case copyErr = <-copyDone:
			copyFinished = true
		case <-time.After(execDrainTimeout):
			forcedClose = true
			attached.Close()
			copyErr = <-copyDone
			copyFinished = true
		}
	}
	stopReading()
	if copyErr != nil && !forcedClose {
		return abort(copyErr)
	}
	select {
	case writeErr := <-writeDone:
		if writeErr != nil && !forcedClose {
			return abort(writeErr)
		}
	default:
	}
	exitCode, err := exitTrailer.Finish()
	if err != nil {
		return abort(err)
	}
	return core.CommandResult{
		ExitCode: exitCode,
		Duration: time.Since(started),
	}, nil
}

func wrappedCommand(statePath, token string, command core.Command) []string {
	arguments := []string{"/bin/sh", "-c", execShell, "aries-exec", statePath, token, command.Path}
	return append(arguments, command.Args...)
}

type exitTrailerWriter struct {
	destination io.Writer
	prefix      []byte
	buffer      bytes.Buffer
}

func newExitTrailer(destination io.Writer, token string) *exitTrailerWriter {
	return &exitTrailerWriter{
		destination: destination,
		prefix:      []byte("\x1eARIES_EXEC_EXIT_" + token + "="),
	}
}

func (w *exitTrailerWriter) Write(content []byte) (int, error) {
	written, _ := w.buffer.Write(content)
	if excess := w.buffer.Len() - execTrailerKeep; excess > 0 {
		chunk := w.buffer.Next(excess)
		if n, err := w.destination.Write(chunk); err != nil {
			return 0, err
		} else if n != len(chunk) {
			return 0, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *exitTrailerWriter) Finish() (int, error) {
	content := w.buffer.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\x1f' {
		return -1, errors.New("Docker exec output is missing its exit trailer")
	}
	start := bytes.LastIndex(content[:len(content)-1], w.prefix)
	if start < 0 {
		return -1, errors.New("Docker exec output has an invalid exit trailer")
	}
	codeBytes := content[start+len(w.prefix) : len(content)-1]
	exitCode, err := strconv.Atoi(string(codeBytes))
	if err != nil || exitCode < 0 || exitCode > 255 {
		return -1, errors.New("Docker exec output has an invalid exit code")
	}
	if _, err := w.destination.Write(content[:start]); err != nil {
		return -1, fmt.Errorf("write Docker exec stderr: %w", err)
	}
	return exitCode, nil
}

// waitForExecExit returns once the exec's process has exited. An exec that
// Docker has not started yet is waited for, up to startTimeout.
func (s *execution) waitForExecExit(ctx context.Context, execID string, startTimeout time.Duration) error {
	interval := execPollInterval
	timer := time.NewTimer(interval)
	defer timer.Stop()
	began := time.Now()
	for {
		inspection, err := s.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspect running Docker exec: %w", err)
		}
		started := execHasStarted(inspection)
		if !started && time.Since(began) > startTimeout {
			return fmt.Errorf("Docker exec did not start within %s", startTimeout)
		}
		if !inspection.Running {
			if started {
				return nil
			}
		} else if inspection.PID > 0 {
			present, err := s.containerHasPID(ctx, inspection.PID)
			if err != nil {
				return fmt.Errorf("inspect Docker exec process: %w", err)
			}
			if !present {
				// Docker 29 can keep ExecInspect.Running true until a hijacked
				// attach is closed even after the process has exited.
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		interval = min(interval*2, time.Second)
		timer.Reset(interval)
	}
}

// execHasStarted reports whether Docker has started the exec's process. Docker
// answers the attach (HTTP 101) before it marks the exec running, and marks it
// running before it creates the process, so on a busy host an inspect can
// report an exec with no PID and no exit code, running or not. An exec keeps
// its PID after it exits, and one that failed to start has exit code 126, so
// an exec with neither has not started.
func execHasStarted(inspection client.ExecInspectResult) bool {
	return inspection.PID > 0 || inspection.ExitCode != 0
}

func (s *execution) containerHasPID(ctx context.Context, pid int) (bool, error) {
	top, err := s.client.ContainerTop(ctx, s.containerID, client.ContainerTopOptions{Arguments: []string{"-eo", "pid"}})
	if err != nil {
		return false, err
	}
	pidColumn := -1
	for index, title := range top.Titles {
		if strings.EqualFold(title, "PID") {
			pidColumn = index
			break
		}
	}
	if pidColumn < 0 {
		return false, errors.New("Docker top response has no PID column")
	}
	want := strconv.Itoa(pid)
	for _, process := range top.Processes {
		if pidColumn < len(process) && process[pidColumn] == want {
			return true, nil
		}
	}
	return false, nil
}

func (s *execution) terminateExec(ctx context.Context, execID, statePath string) error {
	inspection, err := s.client.ExecInspect(ctx, execID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect Docker exec before termination: %w", err)
	}
	if !inspection.Running {
		return nil
	}
	if inspection.PID <= 0 {
		return errors.New("inspect Docker exec before termination: running exec has no PID")
	}
	processGroup, present, err := s.findExecProcessGroup(ctx, inspection.PID)
	if err != nil {
		return fmt.Errorf("locate Docker exec process group: %w", err)
	}
	if !present {
		return nil
	}
	created, err := s.client.ExecCreate(ctx, s.containerID, client.ExecCreateOptions{
		Cmd:  []string{"/bin/sh", "-c", cancelExecShell, "aries-cancel", statePath},
		User: rootExecUser,
	})
	if err != nil {
		return fmt.Errorf("create Docker exec termination helper: %w", err)
	}
	if _, err := s.client.ExecStart(ctx, created.ID, client.ExecStartOptions{Detach: true}); err != nil {
		return fmt.Errorf("start Docker exec termination helper: %w", err)
	}
	helper, err := s.client.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return fmt.Errorf("inspect Docker exec termination helper: %w", err)
	}
	if helper.Running && helper.PID <= 0 {
		return errors.New("inspect Docker exec termination helper: running helper has no PID")
	}
	if err := s.waitForProcessAbsence(ctx, inspection.PID, processGroup, helper.PID); err != nil {
		return fmt.Errorf("confirm terminated Docker exec process-group exit: %w", err)
	}
	return nil
}

func (s *execution) findExecProcessGroup(ctx context.Context, wrapperPID int) (int, bool, error) {
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		table, err := s.processTable(ctx)
		if err != nil {
			return 0, false, err
		}
		if !table.hasPID(wrapperPID) {
			return 0, false, nil
		}
		for _, process := range table.processes {
			if process.ppid == wrapperPID && process.pgid > 1 {
				return process.pgid, true, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *execution) waitForProcessAbsence(ctx context.Context, wrapperPID, processGroup, helperPID int) error {
	ticker := time.NewTicker(execPollInterval)
	defer ticker.Stop()
	for {
		table, err := s.processTable(ctx)
		if err != nil {
			return err
		}
		if !table.hasPID(wrapperPID) && !table.hasGroup(processGroup) && (helperPID <= 0 || !table.hasPID(helperPID)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type containerProcess struct {
	pid  int
	ppid int
	pgid int
}

type processTable struct{ processes []containerProcess }

func (s *execution) processTable(ctx context.Context) (processTable, error) {
	top, err := s.client.ContainerTop(ctx, s.containerID, client.ContainerTopOptions{Arguments: []string{"-eo", "pid,ppid,pgid"}})
	if err != nil {
		return processTable{}, err
	}
	columns := map[string]int{}
	for index, title := range top.Titles {
		columns[strings.ToUpper(title)] = index
	}
	for _, name := range []string{"PID", "PPID", "PGID"} {
		if _, ok := columns[name]; !ok {
			return processTable{}, fmt.Errorf("Docker top response has no %s column", name)
		}
	}
	result := processTable{processes: make([]containerProcess, 0, len(top.Processes))}
	for _, row := range top.Processes {
		if columns["PID"] >= len(row) || columns["PPID"] >= len(row) || columns["PGID"] >= len(row) {
			return processTable{}, errors.New("Docker top response contains a short process row")
		}
		pid, pidErr := strconv.Atoi(row[columns["PID"]])
		ppid, ppidErr := strconv.Atoi(row[columns["PPID"]])
		pgid, pgidErr := strconv.Atoi(row[columns["PGID"]])
		if pidErr != nil || ppidErr != nil || pgidErr != nil {
			return processTable{}, errors.New("Docker top response contains a nonnumeric process identity")
		}
		result.processes = append(result.processes, containerProcess{pid: pid, ppid: ppid, pgid: pgid})
	}
	return result, nil
}

func (t processTable) hasPID(pid int) bool {
	return slices.ContainsFunc(t.processes, func(process containerProcess) bool { return process.pid == pid })
}

func (t processTable) hasGroup(pgid int) bool {
	return slices.ContainsFunc(t.processes, func(process containerProcess) bool { return process.pgid == pgid })
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
