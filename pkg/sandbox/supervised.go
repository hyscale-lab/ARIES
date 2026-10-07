package sandbox

import (
	"context"
	"errors"
	"io"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/deployment"
)

// supervisedDeployment is the narrow direct-exec capability a supervising
// bridge needs. The deployment validates the exact task container against the
// sandbox's request before every operation.
type supervisedDeployment interface {
	ExecSupervisedStream(context.Context, string, deployment.Request, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
	ConfiguredUser(context.Context, string, deployment.Request) (string, error)
}

// ExecSupervisedStream starts one trusted supervisor without the task shell or
// a cancellation helper. The caller owns descendant supervision and must
// independently confirm it before allowing evaluation.
func (s *Sandbox) ExecSupervisedStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error) {
	provider, err := s.supervised(ctx)
	if err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	if err := validateCommand(command); err != nil {
		return core.CommandResult{ExitCode: -1}, err
	}
	if command.Dir == "" {
		command.Dir = s.workdir
	}
	if command.User == "" {
		command.User = s.execUser
	}
	return provider.ExecSupervisedStream(ctx, s.containerID, s.request, command, stdin, stdout, stderr)
}

// TaskUser returns the benchmark's effective user without executing task code.
// Named users remain in their configured form for the trusted supervisor to
// resolve before it starts the native task child.
func (s *Sandbox) TaskUser(ctx context.Context) (string, error) {
	provider, err := s.supervised(ctx)
	if err != nil {
		return "", err
	}
	user, err := provider.ConfiguredUser(ctx, s.containerID, s.request)
	if err != nil {
		return "", err
	}
	if s.execUser != "" {
		return s.execUser, nil
	}
	if user == "" {
		return rootExecUser, nil
	}
	return user, nil
}

func (s *Sandbox) supervised(ctx context.Context) (supervisedDeployment, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	owned := s.owner != nil && s.containerOwned && !s.stopped && !s.stopping && s.containerID != ""
	s.mu.Unlock()
	if !owned {
		return nil, errors.New("supervised execution requires an owned live sandbox")
	}
	provider, ok := s.deployment.(supervisedDeployment)
	if !ok {
		return nil, errors.New("sandbox deployment does not support supervised execution")
	}
	return provider, nil
}
