package monitor

import (
	"context"
	"errors"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ErrUnsupported marks a capability absent for the entire source. Return it from
// the initial Sample call, optionally wrapped with a deployment-specific reason.
// Ordinary discovery, permission, and transient failures must not use this value.
var ErrUnsupported = errors.New("resource measurements unsupported")

// ResourceSource is implemented by each deployment package that can observe
// runtime resources. It returns raw counters and gauges so monitor can produce
// one portable rate and artifact schema.
type ResourceSource interface {
	Sample(context.Context) ([]core.ResourceReading, error)
	Close() error
}
