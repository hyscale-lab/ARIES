//go:build linux

package execsupervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestAgentSupervisorChild(t *testing.T) {
	if os.Getenv("ARIES_AGENT_SUPERVISOR_TEST") != "1" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer done()
	var code int
	var err error
	switch mode {
	case "worker":
		code, err = runWorker(ctx, os.Stdin, os.Stdout, os.Stderr, os.NewFile(3, "worker-control"), true)
	case "broker":
		code, err = runBroker(ctx, brokerConfig{
			stage:      os.Getenv("ARIES_AGENT_SUPERVISOR_STAGE"),
			workerArgs: []string{"-test.run=^TestAgentSupervisorChild$", "--", "worker"},
		}, os.Stdin, os.Stdout, os.Stderr)
	default:
		os.Exit(123)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func TestBrokerRetainsBackgroundAcrossCallsThenReapsEscapes(t *testing.T) {
	fixture := startBrokerFixture(t, false)
	unrelated := exec.Command("/bin/sleep", "60")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	first := filepath.Join(fixture.stage, "setsid.pid")
	second := filepath.Join(fixture.stage, "doublefork.pid")
	leaf := `echo $$ > "$1"; exec sleep 60`
	script := `setsid /bin/sh -c ` + supervisorShellQuote(leaf) + ` fixture ` + supervisorShellQuote(first) + ` </dev/null >/dev/null 2>&1 &
setsid /bin/sh -c ` + supervisorShellQuote(`/bin/sh -c `+supervisorShellQuote(leaf)+` fixture `+supervisorShellQuote(second)+` </dev/null >/dev/null 2>&1 &`) + ` </dev/null >/dev/null 2>&1 &
while [ ! -s ` + supervisorShellQuote(first) + ` ] || [ ! -s ` + supervisorShellQuote(second) + ` ]; do sleep 0.01; done`
	fixture.send(Message{Type: "exec", ID: 1, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", script}}})
	fixture.result(1, true)
	pids := []int{readAgentPID(t, first), readAgentPID(t, second)}
	for _, pid := range pids {
		if err := unix.Kill(pid, 0); err != nil {
			t.Fatalf("background process died before bridge Stop: %v", err)
		}
	}
	fixture.send(Message{Type: "exec", ID: 2, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", `kill -0 "$1" && kill -0 "$2"`, "fixture", strconv.Itoa(pids[0]), strconv.Itoa(pids[1])}}})
	fixture.result(2, true)
	fixture.stop(true)
	for _, pid := range pids {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
			t.Errorf("escaped process %d remains: %v", pid, err)
		}
	}
	if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unrelated benchmark process was killed: %v", err)
	}
	if _, err := os.Lstat(fixture.stage); !os.IsNotExist(err) {
		t.Fatalf("stage remains after proof: %v", err)
	}
}

func TestBrokerBlockedStreamsDoNotBlockSiblingOrCancellation(t *testing.T) {
	fixture := startBrokerFixture(t, false)
	fixture.send(Message{Type: "exec", ID: 1, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", `trap '' TERM; while :; do printf 0123456789abcdef; done`}}})
	fixture.send(Message{Type: "stdin", ID: 1, Data: bytes.Repeat([]byte{'i'}, MaxChunkBytes)})
	fixture.send(Message{Type: "exec", ID: 2, Exec: &ExecSpec{Path: "/bin/printf", Args: []string{"%s", "sibling survives"}}})
	output := fixture.result(2, true) // Deliberately never ACK command 1 output.
	if string(output) != "sibling survives" {
		t.Fatalf("sibling output = %q", output)
	}
	fixture.send(Message{Type: "cancel", ID: 1})
	deadline := time.After(9 * time.Second)
	for {
		select {
		case message, ok := <-fixture.messages:
			if !ok || message.Type == "fatal" {
				t.Fatalf("broker stopped during cancellation: %#v", message)
			}
			if message.Type == "retired" && message.ID == 1 {
				if message.Error != "" {
					t.Fatal(message.Error)
				}
				fixture.stop(true)
				return
			}
		case <-deadline:
			t.Fatal("blocked command did not retire after cancellation")
		}
	}
}

func TestBrokerReexecIgnoresReplacedHelperPath(t *testing.T) {
	fixture := startBrokerFixture(t, true)
	helper := filepath.Join(fixture.stage, "helper")
	script := `mv "$1" "$1.old"; printf '#!/bin/sh\nexit 99\n' > "$1"; chmod 700 "$1"`
	fixture.send(Message{Type: "exec", ID: 1, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", script, "fixture", helper}}})
	fixture.result(1, true)
	fixture.send(Message{Type: "exec", ID: 2, Exec: &ExecSpec{Path: "/bin/printf", Args: []string{"%s", "original executable"}}})
	if got := string(fixture.result(2, true)); got != "original executable" {
		t.Fatalf("replacement helper ran: %q", got)
	}
	fixture.stop(true)
}

func TestBrokerPreservesBufferedOutputAcrossDelayedAcknowledgement(t *testing.T) {
	fixture := startBrokerFixture(t, false)
	fixture.send(Message{Type: "exec", ID: 1, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", `printf first; sleep 0.2; printf last`}}})
	var output []byte
	exited, outEOF, errEOF := false, false, false
	deadline := time.After(5 * time.Second)
	for !exited {
		select {
		case message, ok := <-fixture.messages:
			if !ok || message.Type == "fatal" {
				t.Fatalf("broker failed before delayed ACK: %#v", message)
			}
			switch message.Type {
			case "stdout":
				output = append(output, message.Data...)
			case "exited":
				exited = true
			case "stderr_eof":
				errEOF = true
			}
		case <-deadline:
			t.Fatal("native did not exit with buffered output")
		}
	}
	if string(output) != "first" {
		t.Fatalf("first output chunk = %q", output)
	}
	time.Sleep(300 * time.Millisecond)
	fixture.send(Message{Type: "stdout_ack", ID: 1})
	for !outEOF || !errEOF {
		select {
		case message, ok := <-fixture.messages:
			if !ok || message.Type == "fatal" {
				t.Fatalf("broker failed while draining output: %#v", message)
			}
			switch message.Type {
			case "stdout":
				output = append(output, message.Data...)
				fixture.send(Message{Type: "stdout_ack", ID: 1})
			case "stdout_eof":
				outEOF = true
			case "stderr_eof":
				errEOF = true
			}
		case <-deadline:
			t.Fatal("buffered output did not finish after ACK")
		}
	}
	if string(output) != "firstlast" {
		t.Fatalf("output was truncated by delayed ACK: %q", output)
	}
	fixture.stop(true)
}

func TestBrokerCompletesForegroundWithContinuouslyWritingBackground(t *testing.T) {
	fixture := startBrokerFixture(t, false)
	pidFile := filepath.Join(fixture.stage, "writer.pid")
	background := `trap '' PIPE; echo $$ > "$1"; while :; do printf tick || :; sleep 0.05; done`
	script := `setsid /bin/sh -c ` + supervisorShellQuote(background) + ` fixture ` + supervisorShellQuote(pidFile) + ` </dev/null 2>/dev/null &
while [ ! -s ` + supervisorShellQuote(pidFile) + ` ]; do sleep 0.01; done`
	fixture.send(Message{Type: "exec", ID: 1, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", script}}})
	exited, outEOF, errEOF := false, false, false
	outputBytes := 0
	deadline := time.After(2 * time.Second)
	for !exited || !outEOF || !errEOF {
		select {
		case message, ok := <-fixture.messages:
			if !ok || message.Type == "fatal" {
				t.Fatalf("broker failed while background was writing: %#v", message)
			}
			switch message.Type {
			case "stdout", "stderr":
				outputBytes += len(message.Data)
				fixture.send(Message{Type: message.Type + "_ack", ID: 1})
			case "exited":
				if message.ExitCode == nil || *message.ExitCode != 0 || message.Error != "" {
					t.Fatalf("native result = %#v", message)
				}
				exited = true
			case "stdout_eof":
				outEOF = true
			case "stderr_eof":
				errEOF = true
			}
		case <-deadline:
			fixture.stop(true)
			t.Fatal("background output extended the foreground drain window")
		}
	}
	if outputBytes == 0 {
		t.Fatal("background did not exercise the output stream")
	}
	pid := readAgentPID(t, pidFile)
	fixture.send(Message{Type: "exec", ID: 2, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", `kill -0 "$1"`, "fixture", strconv.Itoa(pid)}}})
	fixture.result(2, true)
	fixture.stop(true)
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatalf("writing background remains after Stop: %v", err)
	}
}

func TestOutputTailDoesNotWaitForConsumedSnapshot(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	content := []byte("already buffered")
	if _, err := writer.Write(content); err != nil {
		t.Fatal(err)
	}
	count, err := outputTailBytes(reader)
	if err != nil || count != len(content) {
		t.Fatalf("pipe snapshot = %d, %v", count, err)
	}
	if _, err := io.ReadFull(reader, make([]byte, count)); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		read, err := readOutputTail(reader, make([]byte, count))
		if read != 0 || !errors.Is(err, io.EOF) {
			finished <- fmt.Errorf("empty output tail = %d, %v", read, err)
			return
		}
		finished <- nil
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = writer.Close() // Unblock an incorrect blocking read before failing.
		<-finished
		t.Fatal("tail reader blocked after snapshotted bytes were consumed")
	}
}

func TestBrokerKilledWorkerFailsClosedAndReapsAdoptedDescendants(t *testing.T) {
	fixture := startBrokerFixture(t, false)
	pidFile := filepath.Join(fixture.stage, "background.pid")
	script := `setsid /bin/sh -c 'echo $$ > "$1"; exec sleep 60' fixture ` + supervisorShellQuote(pidFile) + ` </dev/null >/dev/null 2>&1 &
while [ ! -s ` + supervisorShellQuote(pidFile) + ` ]; do sleep 0.01; done`
	fixture.send(Message{Type: "exec", ID: 1, Exec: &ExecSpec{Path: "/bin/sh", Args: []string{"-c", script}}})
	fixture.result(1, true)
	pid := readAgentPID(t, pidFile)
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ") ")+2:])
	workerPID, err := strconv.Atoi(fields[1])
	if err != nil || workerPID <= 1 || workerPID == os.Getpid() {
		t.Fatalf("invalid worker parent: %s", stat)
	}
	if err := unix.Kill(workerPID, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	fixture.stop(false)
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatalf("adopted descendant remains after failure: %v", err)
	}
}

type brokerFixture struct {
	t        *testing.T
	process  *exec.Cmd
	stdin    io.WriteCloser
	messages chan Message
	finished chan error
	stderr   *bytes.Buffer
	stage    string
	mu       sync.Mutex
}

func startBrokerFixture(t *testing.T, copyExecutable bool) *brokerFixture {
	t.Helper()
	stage := t.TempDir()
	executable := os.Args[0]
	if copyExecutable {
		content, err := os.ReadFile(executable)
		if err != nil {
			t.Fatal(err)
		}
		executable = filepath.Join(stage, "helper")
		if err := os.WriteFile(executable, content, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestAgentSupervisorChild$", "--", "broker")
	command.Env = append(os.Environ(), "ARIES_AGENT_SUPERVISOR_TEST=1", "ARIES_AGENT_SUPERVISOR_STAGE="+stage)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	f := &brokerFixture{t: t, process: command, stdin: stdin, messages: make(chan Message, 128), finished: make(chan error, 1), stderr: new(bytes.Buffer), stage: stage}
	command.Stderr = f.stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = command.Process.Kill() })
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer close(f.messages)
		for {
			message, err := ReadMessage(stdout)
			if err != nil {
				return
			}
			f.messages <- message
		}
	}()
	go func() { <-readDone; f.finished <- command.Wait() }()
	if _, err := io.WriteString(stdin, strings.Repeat("e", 64)+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-f.messages:
		if message.Type != "ready" || message.Version != ProtocolVersion {
			t.Fatalf("broker startup = %#v", message)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broker did not become ready")
	}
	return f
}

func (f *brokerFixture) send(message Message) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := WriteMessage(f.stdin, message); err != nil {
		f.t.Fatal(err)
	}
}

func (f *brokerFixture) result(id uint64, ack bool) []byte {
	f.t.Helper()
	var stdout []byte
	result, outEOF, errEOF := false, false, false
	deadline := time.After(8 * time.Second)
	for !result || !outEOF || !errEOF {
		select {
		case message, ok := <-f.messages:
			if !ok || message.Type == "fatal" {
				f.t.Fatalf("broker stopped before result: %#v", message)
			}
			if message.ID != id {
				continue
			}
			switch message.Type {
			case "stdout", "stderr":
				if message.Type == "stdout" {
					stdout = append(stdout, message.Data...)
				}
				if ack {
					f.send(Message{Type: message.Type + "_ack", ID: id})
				}
			case "exited":
				if message.ExitCode == nil || *message.ExitCode != 0 || message.Error != "" {
					f.t.Fatalf("native result = %#v", message)
				}
				result = true
			case "stdout_eof":
				outEOF = true
			case "stderr_eof":
				errEOF = true
			}
		case <-deadline:
			f.t.Fatal("broker command result timed out")
		}
	}
	return stdout
}

func (f *brokerFixture) stop(success bool) {
	f.t.Helper()
	if success {
		f.send(Message{Type: "stop"})
	}
	deadline := time.After(12 * time.Second)
	messages := f.messages
	for {
		select {
		case message, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			if message.Type == "stdout" || message.Type == "stderr" {
				f.mu.Lock()
				_ = WriteMessage(f.stdin, Message{Type: message.Type + "_ack", ID: message.ID})
				f.mu.Unlock()
			}
		case err := <-f.finished:
			marker := AgentProofPrefix + strings.Repeat("e", 64) + AgentProofSuffix
			if success && (err != nil || !strings.HasSuffix(f.stderr.String(), marker)) {
				f.t.Fatalf("broker cleanup = %v, stderr=%q", err, f.stderr.String())
			}
			if !success && (err == nil || strings.Contains(f.stderr.String(), marker)) {
				f.t.Fatalf("failed broker emitted proof: %v, %q", err, f.stderr.String())
			}
			return
		case <-deadline:
			f.t.Fatal("broker cleanup timed out")
		}
	}
}

func readAgentPID(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid PID fixture: %q", content)
	}
	return pid
}
