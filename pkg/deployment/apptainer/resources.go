package apptainer

import (
	"context"
	"fmt"

	"github.com/hyscale-lab/aries/pkg/core"
	"github.com/hyscale-lab/aries/pkg/monitor"
)

// ResourceSource reports per-runtime resource measurement as unsupported.
// Without cgroups v2 Apptainer gives an unprivileged user no per-instance CPU
// or memory accounting, and summing /proc would lose the CPU time of every
// command that exits between samples. Reporting unsupported keeps that
// distinct from a measured zero.
type ResourceSource struct{}

func NewResourceSource() *ResourceSource { return &ResourceSource{} }

func (*ResourceSource) Sample(context.Context) ([]core.ResourceReading, error) {
	return nil, fmt.Errorf("%w: Apptainer gives no per-instance accounting without cgroups v2", monitor.ErrUnsupported)
}

func (*ResourceSource) Close() error { return nil }
