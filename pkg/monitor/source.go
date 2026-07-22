package monitor

import (
	"context"

	"github.com/hyscale-lab/aries/pkg/core"
)

// ResourceSource is implemented by each deployment package that can observe
// runtime resources. It returns cumulative counters so monitor can produce one
// portable rate and artifact schema.
type ResourceSource interface {
	Sample(context.Context) ([]core.ResourceReading, error)
	Close() error
}
