//go:build linux

package execsupervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type workerMessage struct {
	Type       string              `json:"type"`
	Exec       *ExecSpec           `json:"exec,omitempty"`
	Credential *syscall.Credential `json:"credential,omitempty"`
	ExitCode   *int                `json:"exit_code,omitempty"`
	Error      string              `json:"error,omitempty"`
}

// RunWorker starts the private per-command subreaper. It has no proof nonce;
// only the outer broker can prove final agent-session quiescence.
func RunWorker(ctx context.Context, args []string, stdin, stdout, stderr *os.File) (int, error) {
	if len(args) != 1 || args[0] != "--worker" {
		return supervisorFailureCode, errors.New("agent worker requires --worker and private FD 3")
	}
	if os.Geteuid() != 0 {
		return supervisorFailureCode, errors.New("agent worker must run as root")
	}
	control := os.NewFile(3, "agent-worker-control")
	if control == nil {
		return supervisorFailureCode, errors.New("agent worker control socket is absent")
	}
	defer control.Close()
	return runWorker(ctx, stdin, stdout, stderr, control, false)
}

func runWorker(ctx context.Context, stdin, stdout, stderr, control *os.File, allowUnprivileged bool) (int, error) {
	runtime.GOMAXPROCS(1)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	release, err := protectSupervisor()
	if err != nil {
		return supervisorFailureCode, err
	}
	defer release()
	if stdin == nil || stdout == nil || stderr == nil || control == nil {
		return supervisorFailureCode, errors.New("agent worker descriptors are missing")
	}
	var info unix.Stat_t
	if err := unix.Fstat(int(control.Fd()), &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return supervisorFailureCode, errors.New("agent worker control must be a socket")
	}
	// ExtraFiles clears CLOEXEC while handing FD 3 to this process. Restore it
	// before native exec so neither commands nor their descendants receive it.
	unix.CloseOnExec(int(control.Fd()))
	reader := bufio.NewReader(control)
	initial, err := readWorkerMessage(reader)
	if err != nil || initial.Type != "init" || !allowUnprivileged && initial.Credential == nil {
		return supervisorFailureCode, errors.New("agent worker initialization is invalid")
	}
	if err := validateExecSpec(*initial.Exec); err != nil {
		return supervisorFailureCode, err
	}
	if initial.Credential != nil && initial.Credential.NoSetGroups {
		return supervisorFailureCode, errors.New("agent worker must explicitly set supplementary groups")
	}
	stop := make(chan error, 1)
	go func() {
		message, err := readWorkerMessage(reader)
		if errors.Is(err, io.EOF) || err == nil && message.Type == "stop" {
			stop <- nil
			return
		}
		stop <- errors.New("agent worker control failed")
	}()
	spec := *initial.Exec
	native := exec.Command(spec.Path, spec.Args...)
	native.Dir = spec.Dir
	native.Env = workerEnvironment(os.Environ(), spec.Env)
	native.Stdin, native.Stdout, native.Stderr = stdin, stdout, stderr
	native.SysProcAttr = &syscall.SysProcAttr{Credential: initial.Credential, Setsid: true}
	var nativeErr, controlErr error
	stopping := false
	if err := native.Start(); err != nil {
		nativeErr = errors.New("start task command failed")
	} else {
		waited := make(chan error, 1)
		go func() { waited <- native.Wait() }()
		var deadline <-chan time.Time
		var timer *time.Timer
		if spec.TimeoutNS > 0 {
			timer = time.NewTimer(time.Duration(spec.TimeoutNS))
			deadline = timer.C
		}
		select {
		case nativeErr = <-waited:
		case controlErr = <-stop:
			stopping = true
		case <-ctx.Done():
			stopping = true
		case <-deadline:
			stopping = true
		}
		if timer != nil {
			timer.Stop()
		}
		if stopping {
			cleanup, done := context.WithTimeout(context.Background(), supervisorGrace+supervisorCleanup)
			nativeErr, err = stopSupervisorServer(cleanup, native, waited)
			done()
			if err != nil {
				return supervisorFailureCode, err
			}
		}
	}
	code := nativeExitCode(native.ProcessState)
	exit := workerMessage{Type: "exited", ExitCode: &code}
	if native.ProcessState == nil && nativeErr != nil {
		exit.Error = "start task command failed"
	}
	writeErr := writeWorkerMessage(control, exit)
	if writeErr != nil {
		stopping = true
	}
	// Only after the native Cmd.Wait has completed may generic Wait4 consume
	// adopted children. Normal completion reaps zombies but preserves live jobs.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for !stopping {
		empty, err := reapExitedWorkerChildren()
		if err != nil {
			return supervisorFailureCode, err
		}
		if empty {
			return 0, nil
		}
		select {
		case controlErr = <-stop:
			stopping = true
		case <-ctx.Done():
			stopping = true
		case <-ticker.C:
		}
	}
	cleanup, done := context.WithTimeout(context.Background(), supervisorCleanup)
	defer done()
	if err := reapSupervisorChildren(cleanup); err != nil {
		return supervisorFailureCode, err
	}
	if controlErr != nil || writeErr != nil {
		return supervisorFailureCode, errors.Join(controlErr, writeErr)
	}
	return 0, nil
}

func reapExitedWorkerChildren() (bool, error) {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG|unix.WALL, nil)
		if errors.Is(err, unix.ECHILD) {
			return true, nil
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, errors.New("reap completed agent descendants failed")
		}
		if pid == 0 {
			return false, nil
		}
	}
}

func nativeExitCode(state *os.ProcessState) int {
	if state == nil {
		return -1
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

func workerEnvironment(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			result = append(result, entry)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(overrides)) {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func readWorkerMessage(reader *bufio.Reader) (workerMessage, error) {
	var body []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(body)+len(part) > MaxMessageBytes {
			return workerMessage{}, errors.New("worker control record exceeds limit")
		}
		body = append(body, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return workerMessage{}, err
		}
		break
	}
	var message workerMessage
	if err := decodeProtocolJSON(body, &message); err != nil {
		return workerMessage{}, err
	}
	if err := validateWorkerMessage(message); err != nil {
		return workerMessage{}, err
	}
	return message, nil
}

func writeWorkerMessage(writer io.Writer, message workerMessage) error {
	if err := validateWorkerMessage(message); err != nil {
		return err
	}
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(body)+1 > MaxMessageBytes {
		return errors.New("worker control record exceeds limit")
	}
	return writeProtocolBytes(writer, append(body, '\n'))
}

func validateWorkerMessage(message workerMessage) error {
	switch message.Type {
	case "init":
		if message.Exec != nil && message.ExitCode == nil && message.Error == "" {
			return validateExecSpec(*message.Exec)
		}
	case "stop":
		if message.Exec == nil && message.Credential == nil && message.ExitCode == nil && message.Error == "" {
			return nil
		}
	case "exited":
		if message.Exec == nil && message.Credential == nil && message.ExitCode != nil && *message.ExitCode >= -1 && *message.ExitCode <= 255 {
			return nil
		}
	}
	return errors.New("invalid worker control message")
}
