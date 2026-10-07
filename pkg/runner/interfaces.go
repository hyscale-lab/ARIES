package runner

import (
	"context"
	"errors"
	"io"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ErrNotFound is returned (wrapped) by Sandbox.Download when the requested
// source path does not exist in the sandbox, distinguishing a legitimately
// absent file from a genuine download failure (network/daemon/permission
// errors, disk issues, etc.).
var ErrNotFound = errors.New("sandbox path not found")

// Benchmark owns task discovery and evaluation. Evaluate receives the task
// sandbox the agent used and may start fresh evaluation sandboxes when the
// benchmark's original methodology evaluates in a clean environment.
type Benchmark interface {
	Tasks(context.Context) ([]core.Task, error)
	PrepareSandbox(context.Context, core.Task, Sandbox) error
	Evaluate(context.Context, core.Task, Sandbox, EvaluationSandboxes) (core.Evaluation, error)
}

// EvaluationSandboxes starts fresh sandboxes from a task environment for one
// evaluation. The Runner owns every returned sandbox and stops it after
// Evaluate returns, before the task sandbox.
type EvaluationSandboxes interface {
	Start(context.Context, core.Environment) (Sandbox, error)
}

// AgentHarness owns one task-local agent runtime.
type AgentHarness interface {
	Start(context.Context, core.HarnessRequest) error
	Run(context.Context, string) (core.HarnessResult, error)
	Stop(context.Context) error
}

// ToolSandbox owns the lifecycle of the task environment the agent uses and of
// any fresh evaluation sandboxes requested through SandboxRequest.Purpose.
type ToolSandbox interface {
	Start(context.Context, core.SandboxRequest) (Sandbox, error)
	Stop(context.Context, Sandbox) error
}

// Sandbox is the live capability returned by ToolSandbox, not a fifth
// substitutable component role.
type Sandbox interface {
	// Connectivity supplies explicit runtime placement and resolved task services.
	Connectivity() core.HarnessConnectivity
	Exec(context.Context, core.Command) (core.CommandResult, error)
	Upload(context.Context, string, string) error
	Download(context.Context, string, string) error
}

// LimitedDownloader is an optional sandbox capability for downloads that must
// be rejected before more than maxBytes can be written to the host.
type LimitedDownloader interface {
	DownloadLimit(ctx context.Context, source, destination string, maxBytes int64) error
}

// StreamExecutor is an optional sandbox capability for keeping command output
// outside an agent-writable container filesystem.
type StreamExecutor interface {
	ExecStream(ctx context.Context, command core.Command, stdin io.Reader, stdout, stderr io.Writer) (core.CommandResult, error)
}

// ToolBridge grants and then positively revokes harness access to a sandbox.
// A nil Stop error is the positive revocation confirmation.
type ToolBridge interface {
	Start(context.Context, Sandbox) (core.ToolEndpoint, error)
	Stop(context.Context) error
}
