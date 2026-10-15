// Package target binds native bridge execution to one already-owned runtime.
package target

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/sandbox"
)

// Executor grants file stat, read and write in the bound runtime. It still
// grants no creation, deletion or network ownership of runtimes. File errors use
// fs.ErrNotExist, fs.ErrPermission, syscall.EISDIR and syscall.ENOTDIR, so a
// bridge never imports backend types.
type Executor interface {
	ContainerID() string
	ContainerName() string
	RunID() string
	TaskID() string
	Workdir() string
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
	// StatFile follows a symlink.
	StatFile(ctx context.Context, path string) (fs.FileInfo, error)
	// OpenFile follows a symlink. Closing the stream early stops the transfer;
	// a non-regular path gives its info and an empty stream.
	OpenFile(ctx context.Context, path string) (io.ReadCloser, fs.FileInfo, error)
	// WriteFile lands the write only if content gives exactly size bytes and
	// then io.EOF.
	WriteFile(ctx context.Context, path string, content io.Reader, size int64) (created bool, err error)
}

type Backend interface {
	ValidateBridgeTarget(context.Context, core.BridgeTarget) error
	ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
	StatFile(ctx context.Context, id, path string) (fs.FileInfo, error)
	OpenFile(ctx context.Context, id, path string) (io.ReadCloser, fs.FileInfo, error)
	// WriteFile gives new files and parents to user, a numeric uid:gid; root
	// when it is empty.
	WriteFile(ctx context.Context, id, path string, content io.Reader, size int64, user string) (created bool, err error)
}

type Borrowed struct {
	mu         sync.Mutex
	revoked    bool
	descriptor core.BridgeTarget
	backend    Backend
}

var backendIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Validate checks the shared grant contract. The selected backend must verify
// support and the actual resource's immutable identity and ownership.
func Validate(d core.BridgeTarget) error {
	for _, v := range []string{d.RunID, d.TaskID, d.SandboxID} {
		if v == "" || len(v) > 256 || strings.ContainsAny(v, "/\\\x00\r\n") || v == "." || v == ".." {
			return errors.New("invalid bridge target identity")
		}
	}
	if d.RuntimeID == "" || len(d.RuntimeID) > 1024 || strings.ContainsAny(d.RuntimeID, "\x00\r\n") {
		return errors.New("invalid immutable runtime identity")
	}
	if !backendIdentifier.MatchString(d.Backend) {
		return errors.New("invalid bridge target backend identifier")
	}
	_, err := sandbox.PrepareCommand(core.Command{Path: "/bin/true"}, d.Workdir, d.ExecUser)
	if err != nil {
		return fmt.Errorf("invalid bridge target defaults: %w", err)
	}
	return nil
}

func New(ctx context.Context, d core.BridgeTarget, backend Backend) (*Borrowed, error) {
	if err := Validate(d); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("bridge execution backend is required")
	}
	if err := backend.ValidateBridgeTarget(ctx, d); err != nil {
		return nil, err
	}
	return &Borrowed{descriptor: d, backend: backend}, nil
}
func (b *Borrowed) ContainerID() string   { return b.descriptor.RuntimeID }
func (b *Borrowed) ContainerName() string { return b.descriptor.SandboxID }
func (b *Borrowed) RunID() string         { return b.descriptor.RunID }
func (b *Borrowed) TaskID() string        { return b.descriptor.TaskID }
func (b *Borrowed) Workdir() string       { return b.descriptor.Workdir }
func (b *Borrowed) ExecStream(ctx context.Context, c core.Command, in io.Reader, out, errout io.Writer) (core.CommandResult, error) {
	b.mu.Lock()
	closed := b.revoked
	b.mu.Unlock()
	if closed {
		return core.CommandResult{ExitCode: -1}, errors.New("bridge target admission closed")
	}
	c, err := sandbox.PrepareCommand(c, b.descriptor.Workdir, b.descriptor.ExecUser)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	return b.backend.ExecStream(ctx, b.descriptor.RuntimeID, c, in, out, errout)
}

func (b *Borrowed) StatFile(ctx context.Context, path string) (fs.FileInfo, error) {
	b.mu.Lock()
	closed := b.revoked
	b.mu.Unlock()
	if closed {
		return nil, errors.New("bridge target admission closed")
	}
	return b.backend.StatFile(ctx, b.descriptor.RuntimeID, path)
}

func (b *Borrowed) OpenFile(ctx context.Context, path string) (io.ReadCloser, fs.FileInfo, error) {
	b.mu.Lock()
	closed := b.revoked
	b.mu.Unlock()
	if closed {
		return nil, nil, errors.New("bridge target admission closed")
	}
	return b.backend.OpenFile(ctx, b.descriptor.RuntimeID, path)
}

func (b *Borrowed) WriteFile(ctx context.Context, path string, content io.Reader, size int64) (bool, error) {
	b.mu.Lock()
	closed := b.revoked
	b.mu.Unlock()
	if closed {
		return false, errors.New("bridge target admission closed")
	}
	return b.backend.WriteFile(ctx, b.descriptor.RuntimeID, path, content, size, b.descriptor.ExecUser)
}

// Revoke closes admission without changing the independently owned sandbox.
// The native bridge owns cancellation and joining of its active handlers.
func (b *Borrowed) Revoke() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked = true
}
