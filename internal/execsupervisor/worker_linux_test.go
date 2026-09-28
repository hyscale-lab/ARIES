//go:build linux

package execsupervisor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestWorkerReturnsForegroundStatusButRetainsDetachedProcessUntilStop(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is required")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := `setsid /bin/sh -c 'echo $$ > "$1"; exec sleep 60' fixture ` + supervisorShellQuote(pidFile) + ` </dev/null >/dev/null 2>&1 &
while [ ! -s ` + supervisorShellQuote(pidFile) + ` ]; do sleep 0.01; done
exit 7`
	process, control, finished, _, _ := startWorkerFixture(t, ExecSpec{Path: "/bin/sh", Args: []string{"-c", script}}, nil)
	reader := bufio.NewReader(control)
	message, err := readWorkerMessage(reader)
	if err != nil || message.Type != "exited" || message.ExitCode == nil || *message.ExitCode != 7 {
		t.Fatalf("native exit = %#v, %v", message, err)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(pid, 0); err != nil {
		t.Fatalf("background child did not survive normal return: %v", err)
	}
	select {
	case err := <-finished:
		t.Fatalf("worker exited while background child was alive: %v", err)
	default:
	}
	if err := writeWorkerMessage(control, workerMessage{Type: "stop"}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		_ = process.Process.Kill()
		t.Fatal("worker did not stop")
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatalf("detached child remains after worker stop: %v", err)
	}
}

func TestWorkerPreservesBinaryStdioAndDoesNotInheritControlSocket(t *testing.T) {
	input := []byte{0, 1, 255, '\r', '\n', 'x'}
	_, control, finished, stdout, stderr := startWorkerFixture(t, ExecSpec{Path: "/bin/sh", Args: []string{"-c", `test ! -e /proc/$$/fd/3 || exit 19; cat; printf exact-stderr >&2`}}, input)
	message, err := readWorkerMessage(bufio.NewReader(control))
	if err != nil || message.Type != "exited" || message.ExitCode == nil || *message.ExitCode != 0 {
		t.Fatalf("native exit = %#v, %v", message, err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker without descendants did not retire")
	}
	if !bytes.Equal(stdout.Bytes(), input) || stderr.String() != "exact-stderr" {
		t.Fatalf("native streams changed: stdout=%q stderr=%q", stdout.Bytes(), stderr.Bytes())
	}
}

func startWorkerFixture(t *testing.T, spec ExecSpec, input []byte) (*exec.Cmd, *os.File, <-chan error, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := os.NewFile(uintptr(pair[0]), "worker-parent")
	child := os.NewFile(uintptr(pair[1]), "worker-child")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	process := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAgentSupervisorChild$", "--", "worker")
	process.Env = append(os.Environ(), "ARIES_AGENT_SUPERVISOR_TEST=1")
	process.ExtraFiles = []*os.File{child}
	process.Stdin = bytes.NewReader(input)
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	process.Stdout, process.Stderr = stdout, stderr
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	_ = child.Close()
	finished := make(chan error, 1)
	go func() { finished <- process.Wait() }()
	t.Cleanup(func() { _ = parent.Close(); _ = process.Process.Kill() })
	if err := writeWorkerMessage(parent, workerMessage{Type: "init", Exec: &spec}); err != nil {
		t.Fatal(err)
	}
	return process, parent, finished, stdout, stderr
}
