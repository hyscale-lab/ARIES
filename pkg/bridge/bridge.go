// Package bridge manages temporary access to an independently owned sandbox.
package bridge

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
)

// NativeServer serves one task occurrence's native tool protocol. It receives
// borrowed execution authority, never sandbox lifecycle or verifier ownership.
// Listen opens its endpoint before the sandbox exists; Bind then serves the
// sandbox there, and connections accepted before Bind wait for it. Both must
// retain cleanup ownership even when admission fails. Stop
// closes admission, cancels and joins handlers, and finalizes evidence; a timeout
// leaves cleanup pending and retryable. A revoked server cannot be reused.
//
// Serve also closes borrowed execution admission before the controller collects
// evidence and removes the bridge runtime. Sandbox processes are left intact.
type NativeServer interface {
	Listen(context.Context) (core.ToolEndpoint, error)
	Bind(context.Context, target.Executor) error
	Stop(context.Context) error
}
