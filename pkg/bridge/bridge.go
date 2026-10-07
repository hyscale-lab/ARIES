// Package bridge manages temporary access to an independently owned sandbox.
package bridge

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/bridge/target"
	"github.com/hyscale-lab/aries/pkg/core"
)

// NativeServer serves one task occurrence's native tool protocol. It receives
// borrowed execution authority, never sandbox lifecycle or verifier ownership.
// StartTarget must retain cleanup ownership even when admission fails. Stop
// closes admission, cancels and joins handlers, and finalizes evidence; a timeout
// leaves cleanup pending and retryable. A revoked server cannot be reused.
//
// Successful Stop is only the native drain proof. Serve must also revoke the
// borrowed target before the controller collects evidence and removes the runtime.
type NativeServer interface {
	StartTarget(context.Context, target.Executor) (core.ToolEndpoint, error)
	Stop(context.Context) error
}
