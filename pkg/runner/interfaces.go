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

// Benchmark owns task discovery and evaluation.
type Benchmark interface {
	Tasks(context.Context) ([]core.Task, error)
	PrepareSandbox(context.Context, core.Task, Sandbox) error
	Evaluate(context.Context, core.Task, Sandbox) (core.Evaluation, error)
}

// AgentHarness owns one task-local agent runtime.
type AgentHarness interface {
	Start(context.Context, core.HarnessRequest) error
	Run(context.Context, string) (core.HarnessResult, error)
	Stop(context.Context) error
}

// ToolSandbox creates and removes the live environment later inspected by
// evaluation. The ToolBridge that exposes the environment owns its lifecycle.
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

// Checkpointer is an optional sandbox capability that saves the live
// environment's process and filesystem state, stops it, and later restores it
// in place. Restore is a no-op for a running environment and fails rather than
// resuming from an older state.
type Checkpointer interface {
	Checkpoint(context.Context) error
	Restore(context.Context) error
}

// ToolBridge owns the task sandbox through its composed ToolSandbox, grants
// the harness temporary access to it, and positively revokes that access.
// Open creates the sandbox for preparation; Start grants access; a nil Stop
// error is the positive revocation confirmation and leaves the sandbox live
// for evaluation; Close removes the sandbox and a nil error confirms absence.
// Every Open attempt must be followed by Close, and every Start attempt by Stop.
type ToolBridge interface {
	Open(context.Context, core.SandboxRequest) (Sandbox, error)
	Start(context.Context) (core.ToolEndpoint, error)
	Stop(context.Context) error
	Close(context.Context) error
}
