package kubernetes

import (
	"context"
	"io"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
	arsandbox "github.com/hyscale-lab/aries/pkg/sandbox"
)

// The OpenClaw E2B bridge type-asserts a live sandbox to these method sets at
// runtime (they are unexported in pkg/bridge/openclawe2b). Mirroring them here
// as compile-time assertions means any drift in a Sandbox method signature is a
// build failure in this package rather than a runtime "sandbox does not
// support ..." error during a task.
type grantSandbox interface {
	runner.Sandbox
	NetworkName() string
	NetworkGateway(context.Context) (string, error)
	TaskID() string
	Workdir() string
}

type filesystemSandbox interface {
	runner.Sandbox
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte) error
	StatPath(context.Context, string) (arsandbox.FileInfo, error)
	ListDir(context.Context, string) ([]arsandbox.FileInfo, error)
	MakeDir(context.Context, string) error
	RemovePath(context.Context, string) error
	MovePath(context.Context, string, string) error
}

type processSandbox interface {
	runner.Sandbox
	ExecProcessStream(context.Context, core.Command, io.Writer, io.Writer, func(arsandbox.ProcessRef) error) (core.CommandResult, error)
	SendProcessSignal(context.Context, arsandbox.ProcessRef, string) error
	TerminateProcess(context.Context, arsandbox.ProcessRef) error
}

// sshBridgeSandbox mirrors the unexported bridgeSandbox in pkg/bridge/openclawssh
// so the SSH bridge's requirements are enforced here at compile time too.
type sshBridgeSandbox interface {
	runner.Sandbox
	ContainerID() string
	ContainerName() string
	NetworkName() string
	NetworkGateway(context.Context) (string, error)
	RunID() string
	TaskID() string
	Workdir() string
	ExecStream(context.Context, core.Command, io.Reader, io.Writer, io.Writer) (core.CommandResult, error)
}

var (
	_ grantSandbox      = (*Sandbox)(nil)
	_ filesystemSandbox = (*Sandbox)(nil)
	_ processSandbox    = (*Sandbox)(nil)
	_ sshBridgeSandbox  = (*Sandbox)(nil)
)
