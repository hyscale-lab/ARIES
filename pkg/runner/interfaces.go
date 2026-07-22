package runner

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/core"
)

// Benchmark owns task discovery and evaluation.
type Benchmark interface {
	Tasks(context.Context) ([]core.Task, error)
	Evaluate(context.Context, core.Task, Sandbox) (core.Evaluation, error)
}

// AgentHarness owns one task-local agent runtime.
type AgentHarness interface {
	Start(context.Context, core.HarnessRequest) error
	Run(context.Context, string) (core.HarnessResult, error)
	Stop(context.Context) error
}

// ToolSandbox owns the lifecycle of the live environment later inspected by
// evaluation.
type ToolSandbox interface {
	Start(context.Context, core.SandboxRequest) (Sandbox, error)
	Stop(context.Context, Sandbox) error
}

// Sandbox is the live capability returned by ToolSandbox, not a fifth
// substitutable component role.
type Sandbox interface {
	Exec(context.Context, core.Command) (core.CommandResult, error)
	Upload(context.Context, string, string) error
	Download(context.Context, string, string) error
}

// ToolBridge grants and then positively revokes harness access to a sandbox.
// A nil Stop error is the positive revocation confirmation.
type ToolBridge interface {
	Start(context.Context, Sandbox) (core.ToolEndpoint, error)
	Stop(context.Context) error
}
