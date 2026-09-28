package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// Docker can keep an exec attachment open after the child exits. The random
// trailer positively records that child's status without racing ExecInspect.
const execShell = `token=$1
shift
"$@"
status=$?
printf '\036ARIES_CODEX_EXIT_%s=%s\037' "$token" "$status" >&2
exit "$status"`

type execResult struct {
	stdout, stderr []byte
	exitCode       int
}

func (manager *Manager) execAttached(ctx context.Context, containerID string, command []string, workdir string) (execResult, error) {
	token, err := randomID()
	if err != nil {
		return execResult{exitCode: -1}, err
	}
	wrapped := append([]string{"/bin/sh", "-c", execShell, "aries-codex-exec", token}, command...)
	created, err := manager.client.ExecCreate(ctx, containerID, client.ExecCreateOptions{AttachStdout: true, AttachStderr: true, Cmd: wrapped, WorkingDir: workdir})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("create Codex exec: %w", err)
	}
	attached, err := manager.client.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return execResult{exitCode: -1}, fmt.Errorf("attach Codex exec: %w", err)
	}
	defer attached.Close()
	_ = attached.CloseWrite()
	stdout, stderr := &limitedBuffer{}, &limitedBuffer{}
	trailer := &execTrailer{destination: stderr, prefix: []byte("\x1eARIES_CODEX_EXIT_" + token + "="), done: make(chan struct{})}
	copied := make(chan error, 1)
	go func() { _, copyErr := stdcopy.StdCopy(stdout, trailer, attached.Reader); copied <- copyErr }()
	var copyErr error
	select {
	case <-ctx.Done():
		attached.Close()
		<-copied
		_, _ = trailer.finish()
		return execResult{stdout.Bytes(), stderr.Bytes(), -1}, ctx.Err()
	case copyErr = <-copied:
	case <-trailer.done:
		select {
		case copyErr = <-copied:
		case <-ctx.Done():
			attached.Close()
			<-copied
			_, _ = trailer.finish()
			return execResult{stdout.Bytes(), stderr.Bytes(), -1}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			attached.Close()
			<-copied
		}
	}
	result := execResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exitCode: -1}
	if copyErr != nil {
		return result, fmt.Errorf("read Codex exec: %w", copyErr)
	}
	result.exitCode, err = trailer.finish()
	result.stderr = stderr.Bytes()
	if stdout.exceeded || stderr.exceeded {
		return result, errors.New("Codex exec output exceeded its bound")
	}
	return result, err
}

type limitedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	length := len(content)
	if length > maxOutputBytes-buffer.Len() {
		content = content[:max(0, maxOutputBytes-buffer.Len())]
		buffer.exceeded = true
	}
	_, err := buffer.Buffer.Write(content)
	return length, err
}

type execTrailer struct {
	destination io.Writer
	prefix      []byte
	buffer      bytes.Buffer
	done        chan struct{}
	once        sync.Once
}

func (trailer *execTrailer) Write(content []byte) (int, error) {
	written, _ := trailer.buffer.Write(content)
	buffered := trailer.buffer.Bytes()
	if len(buffered) > 0 && buffered[len(buffered)-1] == '\x1f' && bytes.LastIndex(buffered[:len(buffered)-1], trailer.prefix) >= 0 {
		trailer.once.Do(func() { close(trailer.done) })
	}
	if excess := trailer.buffer.Len() - 256; excess > 0 {
		chunk := trailer.buffer.Next(excess)
		n, err := trailer.destination.Write(chunk)
		if err != nil {
			return 0, err
		}
		if n != len(chunk) {
			return 0, io.ErrShortWrite
		}
	}
	return written, nil
}
func (trailer *execTrailer) finish() (int, error) {
	content := trailer.buffer.Bytes()
	if len(content) == 0 || content[len(content)-1] != '\x1f' {
		_, _ = trailer.destination.Write(content)
		return -1, errors.New("Codex exec is missing its exit trailer")
	}
	start := bytes.LastIndex(content[:len(content)-1], trailer.prefix)
	if start < 0 {
		_, _ = trailer.destination.Write(content)
		return -1, errors.New("Codex exec has an invalid exit trailer")
	}
	status, err := strconv.Atoi(string(content[start+len(trailer.prefix) : len(content)-1]))
	if err != nil || status < 0 || status > 255 {
		return -1, errors.New("Codex exec has an invalid exit status")
	}
	if _, err := trailer.destination.Write(content[:start]); err != nil {
		return -1, err
	}
	return status, nil
}
