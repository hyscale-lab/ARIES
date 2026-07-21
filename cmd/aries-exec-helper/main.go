package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/hyscale-lab/aries/pkg/sandbox/docker/execproto"
)

const maxIO = 16 << 20

type sharedLimit struct {
	mu        sync.Mutex
	remaining int
	exceeded  bool
}

type limitedBuffer struct {
	limit  *sharedLimit
	buffer bytes.Buffer
}

func (b *limitedBuffer) Write(content []byte) (int, error) {
	b.limit.mu.Lock()
	defer b.limit.mu.Unlock()
	consumed := len(content)
	if len(content) > b.limit.remaining {
		b.limit.exceeded = true
		content = content[:b.limit.remaining]
	}
	b.limit.remaining -= len(content)
	if _, err := b.buffer.Write(content); err != nil {
		return 0, err
	}
	return consumed, nil
}

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(125)
	}
}

func run() error {
	if len(os.Args) < 3 {
		return errors.New("usage: aries-exec-helper SOCKET COMMAND [ARG...]")
	}
	connection, err := net.Dial("unix", os.Args[1])
	if err != nil {
		return fmt.Errorf("connect host: %w", err)
	}
	defer connection.Close()
	if err := execproto.WriteHello(connection); err != nil {
		return err
	}
	input, err := execproto.ReadInput(connection, maxIO)
	if err != nil {
		return err
	}
	command := exec.Command(os.Args[2], os.Args[3:]...)
	command.Stdin = bytes.NewReader(input)
	limit := &sharedLimit{remaining: maxIO}
	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	command.Stdout = stdout
	command.Stderr = stderr
	err = command.Run()
	limit.mu.Lock()
	exceeded := limit.exceeded
	limit.mu.Unlock()
	if exceeded {
		return errors.New("command output limit exceeded")
	}
	exitCode := 0
	if err != nil {
		exitCode = 125
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
			if exitCode < 0 {
				if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					exitCode = 128 + int(status.Signal())
				}
			}
		} else if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			exitCode = 127
		}
	}
	return execproto.WriteResult(connection, execproto.Result{ExitCode: exitCode, Stdout: stdout.buffer.Bytes(), Stderr: stderr.buffer.Bytes()})
}
