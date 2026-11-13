// Package bridge manages temporary access to an independently owned sandbox.
package bridge

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/runner"
)

// SandboxStaging places a native executor into the live sandbox before the
// session exposes access, and removes it with confirmed absence after access is
// released. It is optional and selected by composition for a protocol whose
// executor is not part of the task image (Codex's exec-server). The session
// owns ordering and retries; staging owns only the sandbox-side files.
type SandboxStaging interface {
	Stage(context.Context, runner.Sandbox, core.BridgeTarget) error
	Remove(context.Context, runner.Sandbox, core.BridgeTarget) error
}

// NativeServer serves one task occurrence's native tool protocol. It receives
// borrowed execution authority, never sandbox lifecycle or verifier ownership.
// StartTarget must retain cleanup ownership even when admission fails. Stop
// closes admission, cancels and joins handlers, and finalizes evidence; a timeout
// leaves cleanup pending and retryable. A revoked server cannot be reused.
//
// Serve also closes borrowed execution admission before the controller collects
// evidence and removes the bridge runtime. Sandbox processes are left intact.
type NativeServer interface {
	StartTarget(context.Context, target.Executor) (core.ToolEndpoint, error)
	Stop(context.Context) error
}
