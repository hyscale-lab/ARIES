// Package target binds native bridge execution to one already-owned runtime.
package target

import (
	"context"
	"errors"
	"fmt"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
	"github.com/hyscale-lab/aries/pkg/sandbox"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// Executor deliberately grants no creation, deletion, transfer or network ownership.
type Executor interface {
	ContainerID() string
	ContainerName() string
	RunID() string
	TaskID() string
	Workdir() string
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

type Backend interface {
	SnapshotBridgeProcesses(context.Context, string) ([]deployment.ProcessIdentity, error)
	RevokeBridgeProcesses(context.Context, string, []deployment.ProcessIdentity) error
	ValidateBridgeTarget(context.Context, core.BridgeTarget) error
	ExecStream(context.Context, string, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

type Borrowed struct {
	mu         sync.Mutex
	baseline   []deployment.ProcessIdentity
	revoked    bool
	revoking   bool
	descriptor core.BridgeTarget
	backend    Backend
}

var backendIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// Validate checks the shared grant contract. The selected backend must verify
// support and the actual resource's immutable identity and ownership.
func Validate(d core.BridgeTarget) error {
	if d.Version != 1 {
		return errors.New("unsupported bridge target protocol version")
	}
	for _, v := range []string{d.RunID, d.TaskID, d.OccurrenceID, d.RuntimeName} {
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
	if d.ExpectedLabels["aries.managed"] != "true" || d.ExpectedLabels["aries.kind"] != "task-container" || d.ExpectedLabels["aries.component"] != "sandbox" || d.ExpectedLabels["aries.run"] != d.RunID || d.ExpectedLabels["aries.task"] != d.TaskID {
		return errors.New("bridge target ownership does not match task")
	}
	if d.MaxInputBytes != 16<<20 || d.MaxOutputBytes != 1<<30 {
		return errors.New("unsupported bridge target command limits")
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
	d.ExpectedLabels = maps.Clone(d.ExpectedLabels)
	if err := backend.ValidateBridgeTarget(ctx, d); err != nil {
		return nil, err
	}
	baseline, err := backend.SnapshotBridgeProcesses(ctx, d.RuntimeID)
	if err != nil {
		return nil, err
	}
	if len(baseline) == 0 {
		return nil, errors.New("bridge target process baseline is empty")
	}
	return &Borrowed{descriptor: d, backend: backend, baseline: slices.Clone(baseline)}, nil
}
func (b *Borrowed) ContainerID() string   { return b.descriptor.RuntimeID }
func (b *Borrowed) ContainerName() string { return b.descriptor.RuntimeName }
func (b *Borrowed) RunID() string         { return b.descriptor.RunID }
func (b *Borrowed) TaskID() string        { return b.descriptor.TaskID }
func (b *Borrowed) Workdir() string       { return b.descriptor.Workdir }
func (b *Borrowed) ExecStream(ctx context.Context, c core.Command, in io.Reader, out, errout io.Writer) (core.CommandResult, error) {
	b.mu.Lock()
	closed := b.revoking
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

// Revoke must follow the native bridge's confirmed session drain. Ownership of
// the original baseline persists across failed cleanup attempts.
func (b *Borrowed) Revoke(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoking = true
	if b.revoked {
		return nil
	}
	if err := b.backend.ValidateBridgeTarget(ctx, b.descriptor); err != nil {
		return err
	}
	if err := b.backend.RevokeBridgeProcesses(ctx, b.descriptor.RuntimeID, b.baseline); err != nil {
		return err
	}
	b.revoked = true
	return nil
}
